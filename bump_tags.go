package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	lru "github.com/hashicorp/golang-lru/v2"
)

func handleBumpTags(c context.Context, b *bot.Bot, update *models.Update, cache *lru.Cache[string, string], db *sql.DB, args ...string) error {
	adminID := os.Getenv("ADMIN_ID")
	if update.Message.From == nil || adminID == "" || strconv.FormatInt(update.Message.From.ID, 10) != adminID {
		sendText(c, b, update, "only @surgethewolf can use this command")
		return nil
	}

	if update.Message.ReplyToMessage == nil {
		sendText(c, b, update, "reply to a message from the user whose limit you want to bump")
		return nil
	}
	if update.Message.ReplyToMessage.From == nil {
		sendText(c, b, update, "the replied-to message has no sender")
		return nil
	}
	targetID := update.Message.ReplyToMessage.From.ID

	if len(args) == 0 {
		sendText(c, b, update, "amount must be specified")
		return nil
	}
	amount, err := strconv.Atoi(args[0])
	if err != nil || amount <= 0 {
		sendText(c, b, update, "amount must be a positive integer")
		return nil
	}

	// start from the default limit for users who have no row yet, then add.
	if _, err := db.Exec(
		`INSERT INTO tag_limits (user_id, tag_limit) VALUES (?, ?)
			ON CONFLICT(user_id) DO UPDATE SET tag_limit = tag_limit + ?`,
		targetID, defaultTagLimit+amount, amount,
	); err != nil {
		return fmt.Errorf("failed to bump tag limit: %s", err.Error())
	}

	newLimit, err := tagLimit(db, targetID)
	if err != nil {
		return err
	}

	sendText(c, b, update, fmt.Sprintf("bumped tag limit for %s by %d, now %d", replyToSenderName(update.Message.ReplyToMessage), amount, newLimit))
	return nil
}
