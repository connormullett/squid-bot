package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	lru "github.com/hashicorp/golang-lru/v2"
)

func handleStats(c context.Context, b *bot.Bot, update *models.Update, cache *lru.Cache[string, string], db *sql.DB, args ...string) error {
	// number of image files available in the squid directory
	var imageCount int
	if entries, err := os.ReadDir("/app/squid"); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				imageCount++
			}
		}
	}

	var tagCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM tags").Scan(&tagCount); err != nil {
		return fmt.Errorf("failed to count tags: %s", err.Error())
	}

	var taggedImages int
	if err := db.QueryRow("SELECT COUNT(DISTINCT file_name) FROM tag_images").Scan(&taggedImages); err != nil {
		return fmt.Errorf("failed to count tagged images: %s", err.Error())
	}

	// top tags by number of images
	rows, err := db.Query(`SELECT t.name, COUNT(*) AS c
		FROM tag_images ti JOIN tags t ON ti.tag_id = t.id
		GROUP BY t.id
		ORDER BY c DESC, t.name
		LIMIT 5`)
	if err != nil {
		return fmt.Errorf("failed to query top tags: %s", err.Error())
	}
	defer rows.Close()

	var topTags []string
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return fmt.Errorf("failed to scan top tag: %s", err.Error())
		}
		topTags = append(topTags, fmt.Sprintf("%s (%d)", name, count))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read top tags: %s", err.Error())
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "squid-bot stats:\n")
	fmt.Fprintf(&sb, "images: %d\n", imageCount)
	fmt.Fprintf(&sb, "tags: %d\n", tagCount)
	fmt.Fprintf(&sb, "tagged images: %d\n", taggedImages)

	if len(topTags) > 0 {
		fmt.Fprintf(&sb, "top tags: %s\n", strings.Join(topTags, ", "))
	}

	if update.Message.From != nil {
		var myTags int
		if err := db.QueryRow("SELECT COUNT(*) FROM tags WHERE created_by = ?", update.Message.From.ID).Scan(&myTags); err != nil {
			return fmt.Errorf("failed to count your tags: %s", err.Error())
		}
		fmt.Fprintf(&sb, "your tags: %d", myTags)
	}

	sendText(c, b, update, strings.TrimRight(sb.String(), "\n"))
	return nil
}
