package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	lru "github.com/hashicorp/golang-lru/v2"
)

func handleGetSquid(c context.Context, b *bot.Bot, update *models.Update, cache *lru.Cache[string, string], db *sql.DB, args ...string) error {
	// /squid <tag> [tag...] -> a random image having all the specified tags
	if len(args) > 0 {
		return handleGetSquidWithTag(c, b, update, db, args...)
	}

	// bare /squid -> a random image
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
