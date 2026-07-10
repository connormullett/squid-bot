package main

import (
	"bytes"
	"context"
	"log"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println(err)
	}

	b, err := bot.New(os.Getenv("BOT_TOKEN"))
	if err != nil {
		log.Fatalf("Failed to create bot: %s", err.Error())
	}
	log.Println("Created bot")

	cache, _ := lru.New[string, string](12)

	b.RegisterHandler(bot.HandlerTypeMessageText, "/squid", bot.MatchTypePrefix, makeHandleGetSquid(cache))

	ctx := context.Background()

	log.Println("starting bot")
	b.Start(ctx)
}

func makeHandleGetSquid(cache *lru.Cache[string, string]) bot.HandlerFunc {
	return func(c context.Context, b *bot.Bot, update *models.Update) {
		file, err := getRandomFile(cache, "/app/squid")
		if err != nil {
			log.Println(err)
			return
		}
		if file == "" {
			log.Println("No files found")
			return
		}

		if err := sendPhoto(c, b, update, file); err != nil {
			log.Println(err)
			return
		}
		log.Printf("Sent photo: %+v", file)
	}
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
