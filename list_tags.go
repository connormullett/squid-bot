package main

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	lru "github.com/hashicorp/golang-lru/v2"
)

func handleListTags(c context.Context, b *bot.Bot, update *models.Update, cache *lru.Cache[string, string], db *sql.DB, args ...string) error {
	// if replying to a squid-bot image, list that image's tags instead of all tags
	if reply := update.Message.ReplyToMessage; reply != nil && sentBySquidBot(reply, b.ID()) {
		fileName := replyImageFileID(reply)
		if fileName == "" {
			sendText(c, b, update, "the replied-to message does not contain an image")
			return nil
		}

		caption, err := tagsCaption(db, fileName)
		if err != nil {
			return err
		}
		if caption == "" {
			caption = "this image has no tags"
		}
		sendText(c, b, update, caption)
		return nil
	}

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
