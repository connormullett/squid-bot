package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	lru "github.com/hashicorp/golang-lru/v2"
)

func handleTagInfo(c context.Context, b *bot.Bot, update *models.Update, cache *lru.Cache[string, string], db *sql.DB, args ...string) error {
	tagsQuery := "SELECT COUNT(*) FROM tag_images ti JOIN tags t ON ti.tag_id = t.id WHERE t.name = ?"
	var count int
	tag := args[0]
	err := db.QueryRow(tagsQuery, tag).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to query tag info: %s", err.Error())
	}

	sendText(c, b, update, fmt.Sprintf("tag %s has %d images", tag, count))
	return nil
}
