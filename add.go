package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	lru "github.com/hashicorp/golang-lru/v2"
)

func handleAddTag(c context.Context, b *bot.Bot, update *models.Update, cache *lru.Cache[string, string], db *sql.DB, tags ...string) error {
	if update.Message.ReplyToMessage == nil {
		sendText(c, b, update, "reply to an image to add a tag")
		return nil
	}

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
		// creating tags is only allowed while the sender is under their tag creation limit
		var tagCount int
		if err := tx.QueryRow("SELECT COUNT(*) FROM tags WHERE created_by = ?", senderID).Scan(&tagCount); err != nil {
			return fmt.Errorf("failed to count tags for user: %s", err.Error())
		}
		limit, err := tagLimit(tx, senderID)
		if err != nil {
			return err
		}
		if tagCount+len(newTags) > limit {
			sendText(c, b, update, fmt.Sprintf("you have reached the limit of %d tags", limit))
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

	// associate the image with every tag
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
