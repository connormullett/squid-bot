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

	ctx := context.Background()

	me, err := b.GetMe(ctx)
	if err != nil {
		log.Fatalf("Failed to get bot info: %s", err.Error())
	}

	b.RegisterHandlerMatchFunc(matchCommand("squid", me.Username), makeHandleGetSquid(cache, db))
	b.RegisterHandlerMatchFunc(matchCommand("help", me.Username), func(c context.Context, b *bot.Bot, update *models.Update) {
		sendText(c, b, update, `Commands:
/squid - get a random squid image
/squid <tag> [tag...] - get a random squid image having all the specified tags
/squid tags - list all tags
/squid tagsinfo <tag> - get info about the specified tag
/squid mytags - list your created tags
/squid add <tag> [tag...] - add the replied-to image to the specified tag(s)`)
	})

	log.Println("starting bot")
	b.Start(ctx)
}

func matchCommand(command, botUsername string) bot.MatchFunc {
	return func(update *models.Update) bool {
		if update.Message == nil {
			return false
		}
		for _, e := range update.Message.Entities {
			if e.Type != models.MessageEntityTypeBotCommand || e.Offset != 0 {
				continue
			}
			name, mention, _ := strings.Cut(update.Message.Text[1:e.Length], "@")
			return name == command && (mention == "" || strings.EqualFold(mention, botUsername))
		}
		return false
	}
}

func makeHandleGetSquid(cache *lru.Cache[string, string], db *sql.DB) bot.HandlerFunc {
	return func(c context.Context, b *bot.Bot, update *models.Update) {
		// matchCommand guarantees the message starts with /squid[@botname];
		// everything after it is arguments
		args := strings.Fields(update.Message.Text)[1:]

		var sub string
		if len(args) > 0 {
			sub = args[0]
		}

		var err error
		switch {
		case len(args) == 0:
			// just get squid
			err = handleGetSquid(c, b, update, cache, db)
		case sub == "tags" && len(args) == 1:
			// list all tags
			err = handleListTags(c, b, update, db)
		case sub == "mytags" && len(args) == 1:
			// list tags created by user
			err = handleListMyTags(c, b, update, db)
		case sub == "tagsinfo" && len(args) == 2:
			// get info about a tag
			err = handleTagInfo(c, b, update, db, args[1])
		case sub == "add" && len(args) >= 2:
			// add the replied-to image to one or more tags
			err = handleAddTag(c, b, update, db, args[1:]...)
		default:
			// get squid matching all given tags
			err = handleGetSquidWithTag(c, b, update, db, args...)
		}

		if err != nil {
			sendText(c, b, update, "Something went wrong D:\n"+err.Error())
			log.Println(err)
		}
	}
}

func handleTagInfo(c context.Context, b *bot.Bot, update *models.Update, db *sql.DB, tag string) error {
	tagsQuery := "SELECT COUNT(*) FROM tag_images ti JOIN tags t ON ti.tag_id = t.id WHERE t.name = ?"
	var count int
	err := db.QueryRow(tagsQuery, tag).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to query tag info: %s", err.Error())
	}

	sendText(c, b, update, fmt.Sprintf("tag %s has %d images", tag, count))
	return nil
}

func handleGetSquidWithTag(c context.Context, b *bot.Bot, update *models.Update, db *sql.DB, tags ...string) error {
	// dedupe tags, preserving order
	seen := make(map[string]bool, len(tags))
	uniqueTags := make([]string, 0, len(tags))
	for _, tag := range tags {
		if !seen[tag] {
			seen[tag] = true
			uniqueTags = append(uniqueTags, tag)
		}
	}
	tags = uniqueTags

	if len(tags) > 10 {
		sendText(c, b, update, "too many tags, limit 10")
		return nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(tags)), ",")
	tagArgs := make([]any, len(tags))
	for i, tag := range tags {
		tagArgs[i] = tag
	}

	// check that all tags exist
	found, err := queryTagNames(db, "SELECT name FROM tags WHERE name IN ("+placeholders+")", tagArgs...)
	if err != nil {
		return err
	}
	if len(found) != len(tags) {
		foundSet := make(map[string]bool, len(found))
		for _, name := range found {
			foundSet[name] = true
		}
		var missing []string
		for _, tag := range tags {
			if !foundSet[tag] {
				missing = append(missing, tag)
			}
		}
		sendText(c, b, update, fmt.Sprintf("failed to find tag(s) %s", strings.Join(missing, ", ")))
		return nil
	}

	// pick a random image that has every requested tag
	imageQuery := `SELECT ti.file_name FROM tag_images ti
		JOIN tags t ON ti.tag_id = t.id
		WHERE t.name IN (` + placeholders + `)
		GROUP BY ti.file_name
		HAVING COUNT(DISTINCT t.id) = ?
		ORDER BY RANDOM() LIMIT 1`

	var imageName string
	err = db.QueryRow(imageQuery, append(tagArgs, len(tags))...).Scan(&imageName)
	if err != nil {
		if err == sql.ErrNoRows {
			sendText(c, b, update, fmt.Sprintf("no images found for tag(s) %s", strings.Join(tags, ", ")))
			return nil
		}
		return fmt.Errorf("failed to query images: %s", err.Error())
	}

	caption, err := tagsCaption(db, imageName)
	if err != nil {
		return err
	}

	if _, err := b.SendPhoto(c, &bot.SendPhotoParams{
		ChatID:  update.Message.Chat.ID,
		Photo:   &models.InputFileString{Data: imageName},
		Caption: caption,
	}); err != nil {
		return fmt.Errorf("failed to send photo: %s", err.Error())
	}

	return nil
}

// tagsCaption returns all tags associated with an image as comma separated
// values, or "" if the image has no tags.
func tagsCaption(db *sql.DB, fileName string) (string, error) {
	rows, err := db.Query(
		"SELECT DISTINCT t.name FROM tags t JOIN tag_images ti ON ti.tag_id = t.id WHERE ti.file_name = ?",
		fileName,
	)
	if err != nil {
		return "", fmt.Errorf("failed to query tags for image: %s", err.Error())
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", fmt.Errorf("failed to scan tag name: %s", err.Error())
		}
		tags = append(tags, name)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("failed to read tags for image: %s", err.Error())
	}

	return strings.Join(tags, ", "), nil
}

func handleListTags(c context.Context, b *bot.Bot, update *models.Update, db *sql.DB) error {
	tags, err := queryTagNames(db, "SELECT name FROM tags ORDER BY name")
	if err != nil {
		return err
	}

	text := "no tags yet"
	if len(tags) > 0 {
		text = strings.Join(tags, ", ")
	}

	sendText(c, b, update, text)
	return nil
}

func handleListMyTags(c context.Context, b *bot.Bot, update *models.Update, db *sql.DB) error {
	if update.Message.From == nil {
		return errors.New("mytags: message has no sender")
	}

	tags, err := queryTagNames(db, "SELECT name FROM tags WHERE created_by = ? ORDER BY name", update.Message.From.ID)
	if err != nil {
		return err
	}

	text := "you have not created any tags"
	if len(tags) > 0 {
		text = strings.Join(tags, ", ")
	}

	sendText(c, b, update, text)
	return nil
}

func queryTagNames(db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query tags: %s", err.Error())
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("failed to scan tag name: %s", err.Error())
		}
		tags = append(tags, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read tags: %s", err.Error())
	}

	return tags, nil
}

const maxTagsPerUser = 100

func handleAddTag(c context.Context, b *bot.Bot, update *models.Update, db *sql.DB, tags ...string) error {
	// the image to associate with the tag comes from the replied-to message
	if update.Message.ReplyToMessage == nil {
		sendText(c, b, update, "reply to an image to add a tag")
		return nil
	}

	// only images sent by the bot itself, directly or forwarded, can be tagged
	if !sentBySquidBot(update.Message.ReplyToMessage, b.ID()) {
		sendText(c, b, update, "only images sent by squid-bot can be tagged")
		return nil
	}

	fileName := replyImageFileID(update.Message.ReplyToMessage)
	if fileName == "" {
		sendText(c, b, update, "the replied-to message does not contain an image")
		return nil
	}

	if len(tags) == 0 {
		sendText(c, b, update, "no tags given")
		return nil
	}

	if len(tags) > 10 {
		sendText(c, b, update, "too many tags, limit 10")
		return nil
	}

	if update.Message.From == nil {
		return errors.New("add tag: message has no sender")
	}
	senderID := update.Message.From.ID

	// dedupe tags, preserving order
	seen := make(map[string]bool, len(tags))
	uniqueTags := make([]string, 0, len(tags))
	for _, tag := range tags {
		if !seen[tag] {
			seen[tag] = true
			uniqueTags = append(uniqueTags, tag)
		}
	}
	tags = uniqueTags

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(tags)), ",")
	tagArgs := make([]any, len(tags))
	for i, tag := range tags {
		tagArgs[i] = tag
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %s", err.Error())
	}
	defer tx.Rollback()

	// find which tags need to be created
	existing := make(map[string]bool, len(tags))
	rows, err := tx.Query("SELECT name FROM tags WHERE name IN ("+placeholders+")", tagArgs...)
	if err != nil {
		return fmt.Errorf("failed to query tags: %s", err.Error())
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("failed to scan tag name: %s", err.Error())
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("failed to read tags: %s", err.Error())
	}
	rows.Close()

	var newTags []string
	for _, tag := range tags {
		if !existing[tag] {
			newTags = append(newTags, tag)
		}
	}

	if len(newTags) > 0 {
		// creating tags is only allowed while the sender is under their
		// tag creation limit
		var tagCount int
		if err := tx.QueryRow("SELECT COUNT(*) FROM tags WHERE created_by = ?", senderID).Scan(&tagCount); err != nil {
			return fmt.Errorf("failed to count tags for user: %s", err.Error())
		}
		if tagCount+len(newTags) > maxTagsPerUser {
			sendText(c, b, update, fmt.Sprintf("you have reached the limit of %d tags", maxTagsPerUser))
			return nil
		}

		insertValues := strings.TrimSuffix(strings.Repeat("(?, ?),", len(newTags)), ",")
		insertArgs := make([]any, 0, len(newTags)*2)
		for _, tag := range newTags {
			insertArgs = append(insertArgs, tag, senderID)
		}
		if _, err := tx.Exec("INSERT INTO tags (name, created_by) VALUES "+insertValues+" ON CONFLICT(name) DO NOTHING", insertArgs...); err != nil {
			return fmt.Errorf("failed to create tags: %s", err.Error())
		}
	}

	// associate the image with every tag in one insert, ignoring duplicates
	res, err := tx.Exec(
		"INSERT INTO tag_images (tag_id, file_name) SELECT id, ? FROM tags WHERE name IN ("+placeholders+") ON CONFLICT(tag_id, file_name) DO NOTHING",
		append([]any{fileName}, tagArgs...)...,
	)
	if err != nil {
		return fmt.Errorf("failed to add tag images: %s", err.Error())
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check tag image insert: %s", err.Error())
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %s", err.Error())
	}

	text := fmt.Sprintf("added image to tag(s) %s", strings.Join(tags, ", "))
	if inserted == 0 {
		text = fmt.Sprintf("image already has tag(s) %s", strings.Join(tags, ", "))
	}
	sendText(c, b, update, text)

	return nil
}

// sentBySquidBot reports whether the message was sent by the bot itself,
// either directly or forwarded from it by another user.
func sentBySquidBot(msg *models.Message, botID int64) bool {
	if msg.From != nil && msg.From.ID == botID {
		return true
	}
	if msg.ForwardOrigin != nil &&
		msg.ForwardOrigin.Type == models.MessageOriginTypeUser &&
		msg.ForwardOrigin.MessageOriginUser != nil {
		return msg.ForwardOrigin.MessageOriginUser.SenderUser.ID == botID
	}
	return false
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

func sendText(c context.Context, b *bot.Bot, update *models.Update, text string) error {
	_, err := b.SendMessage(c, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   text,
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
