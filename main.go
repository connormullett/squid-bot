package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println(err)
	}

	db, err := sql.Open("sqlite", "/app/db")
	if err != nil {
		log.Fatalf("Failed to open database: %s", err.Error())
	}
	defer db.Close()

	createTagsTable := `CREATE TABLE IF NOT EXISTS tags (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		created_by INTEGER
	)`
	if _, err := db.Exec(createTagsTable); err != nil {
		log.Fatalf("Failed to create tags table: %s", err.Error())
	}

	createTagImageTable := `CREATE TABLE IF NOT EXISTS tag_images (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		tag_id INTEGER NOT NULL,
		file_name TEXT NOT NULL,
		FOREIGN KEY(tag_id) REFERENCES tags(id)
	)`
	if _, err := db.Exec(createTagImageTable); err != nil {
		log.Fatalf("Failed to create tag_images table: %s", err.Error())
	}

	b, err := bot.New(os.Getenv("BOT_TOKEN"))
	if err != nil {
		log.Fatalf("Failed to create bot: %s", err.Error())
	}
	log.Println("Created bot")

	cache, _ := lru.New[string, string](12)

	b.RegisterHandler(bot.HandlerTypeMessageText, "/squid", bot.MatchTypePrefix, makeHandleGetSquid(cache, db))

	ctx := context.Background()

	log.Println("starting bot")
	b.Start(ctx)
}

func makeHandleGetSquid(cache *lru.Cache[string, string], db *sql.DB) bot.HandlerFunc {
	return func(c context.Context, b *bot.Bot, update *models.Update) {
		commandParts := strings.Split(update.Message.Text, " ")

		var err error
		if len(commandParts) == 1 && strings.HasPrefix(commandParts[0], "/squid") {
			// just get squid
			err = handleGetSquid(c, b, update, cache, db)
		} else if len(commandParts) == 2 {
			// get squid with tag
			err = handleGetSquidWithTag(c, b, update, cache, db, commandParts[1])
		} else if len(commandParts) == 3 && commandParts[1] == "add" {
			// add squid with tag
			err = handleAddTag(c, b, update, cache, db, commandParts[2])
		}

		if err != nil {
			log.Println(err)
		}
	}
}

type TagQuery struct {
	ID   int
	Name string
}

func handleGetSquidWithTag(c context.Context, b *bot.Bot, update *models.Update, cache *lru.Cache[string, string], db *sql.DB, tag string) error {
	// check if tag exists
	tagsQuery := "SELECT id, name FROM tags WHERE name = ?"
	tagsStmt, err := db.Prepare(tagsQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare tags query: %s", err.Error())
	}
	defer tagsStmt.Close()

	var foundTag TagQuery
	err = tagsStmt.QueryRow(tag).Scan(&foundTag.ID, &foundTag.Name)
	if err != nil {
		if err == sql.ErrNoRows {
			b.SendMessage(c, &bot.SendMessageParams{
				ChatID: update.Message.Chat.ID,
				Text:   fmt.Sprintf("failed to find tag %s", tag),
			})
			return fmt.Errorf("tag %s not found", tag)
		}
		return fmt.Errorf("failed to query tags: %s", err.Error())
	}

	// get image by tag
	imageQuery := "SELECT file_name FROM tag_images WHERE tag_id = ? ORDER BY RANDOM() LIMIT 1"
	imageStmt, err := db.Prepare(imageQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare image query: %s", err.Error())
	}
	defer imageStmt.Close()

	var imageName string
	err = imageStmt.QueryRow(foundTag.ID).Scan(&imageName)
	if err != nil {
		if err == sql.ErrNoRows {
			b.SendMessage(c, &bot.SendMessageParams{
				ChatID: update.Message.Chat.ID,
				Text:   fmt.Sprintf("no images found for tag %s", tag),
			})
			return fmt.Errorf("no images found for tag %s", tag)
		}
		return fmt.Errorf("failed to query images: %s", err.Error())
	}

	if _, err := b.SendPhoto(c, &bot.SendPhotoParams{
		ChatID: update.Message.Chat.ID,
		Photo:  &models.InputFileString{Data: imageName},
	}); err != nil {
		return fmt.Errorf("failed to send photo: %s", err.Error())
	}

	return nil
}

const maxTagsPerUser = 100

func handleAddTag(c context.Context, b *bot.Bot, update *models.Update, cache *lru.Cache[string, string], db *sql.DB, tag string) error {
	// the image to associate with the tag comes from the replied-to message
	if update.Message.ReplyToMessage == nil {
		b.SendMessage(c, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "reply to an image to add a tag",
		})
		return fmt.Errorf("add tag: no replied-to message")
	}

	fileName := replyImageFileID(update.Message.ReplyToMessage)
	if fileName == "" {
		b.SendMessage(c, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "the replied-to message does not contain an image",
		})
		return errors.New("add tag: replied-to message has no image")
	}

	if update.Message.From == nil {
		return errors.New("add tag: message has no sender")
	}
	senderID := update.Message.From.ID

	// check if the tag already exists
	var tagID int64
	err := db.QueryRow("SELECT id FROM tags WHERE name = ?", tag).Scan(&tagID)
	switch {
	case err == sql.ErrNoRows:
		// tag doesn't exist yet, create it only if the sender is under
		// their tag creation limit
		var tagCount int
		if err := db.QueryRow("SELECT COUNT(*) FROM tags WHERE created_by = ?", senderID).Scan(&tagCount); err != nil {
			return fmt.Errorf("failed to count tags for user: %s", err.Error())
		}
		if tagCount >= maxTagsPerUser {
			b.SendMessage(c, &bot.SendMessageParams{
				ChatID: update.Message.Chat.ID,
				Text:   fmt.Sprintf("you have reached the limit of %d tags", maxTagsPerUser),
			})
			return fmt.Errorf("add tag: user %d reached tag limit", senderID)
		}

		res, err := db.Exec("INSERT INTO tags (name, created_by) VALUES (?, ?)", tag, senderID)
		if err != nil {
			return fmt.Errorf("failed to create tag: %s", err.Error())
		}
		tagID, err = res.LastInsertId()
		if err != nil {
			return fmt.Errorf("failed to get created tag id: %s", err.Error())
		}
	case err != nil:
		return fmt.Errorf("failed to query tags: %s", err.Error())
	}

	// associate the replied-to image with the tag
	if _, err := db.Exec("INSERT INTO tag_images (tag_id, file_name) VALUES (?, ?)", tagID, fileName); err != nil {
		return fmt.Errorf("failed to add tag image: %s", err.Error())
	}

	b.SendMessage(c, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   fmt.Sprintf("added image to tag %s", tag),
	})

	return nil
}

func replyImageFileID(msg *models.Message) string {
	if msg.Document != nil && msg.Document.FileID != "" {
		return msg.Document.FileID
	}
	if len(msg.Photo) > 0 {
		// the last PhotoSize is the largest resolution
		return msg.Photo[len(msg.Photo)-1].FileID
	}
	return ""
}

func handleGetSquid(c context.Context, b *bot.Bot, update *models.Update, cache *lru.Cache[string, string], db *sql.DB) error {
	file, err := getRandomFile(cache, "/app/squid")
	if err != nil {
		return err
	}
	if file == "" {
		return errors.New("no files found")
	}

	if err := sendPhoto(c, b, update, file); err != nil {
		return fmt.Errorf("failed to send photo: %s", err.Error())
	}
	return nil
}

func sendPhoto(c context.Context, b *bot.Bot, update *models.Update, file string) error {
	photoContent, err := os.ReadFile(file)
	if err != nil {
		return err
	}

	_, err = b.SendPhoto(c, &bot.SendPhotoParams{
		ChatID: update.Message.Chat.ID,
		Photo: &models.InputFileUpload{
			Filename: filepath.Base(file),
			Data:     bytes.NewReader(photoContent),
		},
	})

	return err
}

func getRandomFile(cache *lru.Cache[string, string], dirPath string) (string, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return "", err
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() {
			files = append(files, entry.Name())
		}
	}

	for {
		if len(files) == 0 {
			return "", nil
		}

		randomIndex := rand.Intn(len(files))
		randomFileName := files[randomIndex]

		path := filepath.Join(dirPath, randomFileName)

		if cache.Contains(path) {
			continue
		}
		cache.Add(path, path)
		return path, nil
	}
}
