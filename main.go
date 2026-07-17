package main

import (
	"context"
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

	if os.Getenv("ADMIN_ID") == "" {
		log.Fatal("ADMIN_ID environment variable is not set")
	}

	db, err := initDB()
	if err != nil {
		log.Fatalf("Failed to initialize database: %s", err.Error())
	}
	defer db.Close()

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

	commandHandler := &SquidCommandHandler{
		db:    db,
		cache: cache,
		bot:   b,
	}

	commandHandler.Register("tags", handleListTags)
	commandHandler.Register("mytags", handleListMyTags)
	commandHandler.Register("stats", handleStats)
	commandHandler.Register("submit", handleSubmit)
	commandHandler.Register("tagsinfo", handleTagInfo)
	commandHandler.Register("add", handleAddTag)
	commandHandler.Register("bumptags", handleBumpTags)
	commandHandler.Register("", handleGetSquid)

	b.RegisterHandlerMatchFunc(matchCommand("squid", me.Username), makeHandleGetSquid(commandHandler))
	b.RegisterHandlerMatchFunc(matchCommand("help", me.Username), func(c context.Context, b *bot.Bot, update *models.Update) {
		sendText(c, b, update, `Commands:
/squid - get a random squid image
/squid <tag> [tag...] - get a random squid image having all the specified tags
/squid tags - list all tags
/squid tagsinfo <tag> - get info about the specified tag
/squid mytags - list your created tags
/squid stats - show squid-bot statistics
/squid submit - reply to an image to add it to the squid pool
/squid add <tag> [tag...] - add the replied-to image to the specified tag(s)
/squid bumptags <n> - (admin) reply to a user's message to raise their tag limit by n`)
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

func makeHandleGetSquid(commandHandler *SquidCommandHandler) bot.HandlerFunc {
	return func(c context.Context, b *bot.Bot, update *models.Update) {
		fields := strings.Fields(update.Message.Text)
		args := []string{}

		if len(fields) > 1 {
			args = fields[1:]
		}

		err := commandHandler.Execute(c, update, args)

		if err != nil {
			sendText(c, b, update, "Something went wrong D:\n"+err.Error())
			log.Println(err)
		}
	}
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
