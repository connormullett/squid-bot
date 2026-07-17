package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	lru "github.com/hashicorp/golang-lru/v2"
)

func handleListMyTags(c context.Context, b *bot.Bot, update *models.Update, cache *lru.Cache[string, string], db *sql.DB, args ...string) error {
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
