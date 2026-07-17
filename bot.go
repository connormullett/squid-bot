package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func replyToSenderName(msg *models.Message) string {
	if msg.From == nil {
		return "user"
	}
	if msg.From.Username != "" {
		return "@" + msg.From.Username
	}
	name := strings.TrimSpace(msg.From.FirstName + " " + msg.From.LastName)
	if name == "" {
		return fmt.Sprintf("user %d", msg.From.ID)
	}
	return name
}

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
		return msg.Photo[len(msg.Photo)-1].FileID
	}
	return ""
}

func sendText(c context.Context, b *bot.Bot, update *models.Update, text string) error {
	_, err := b.SendMessage(c, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   text,
	})
	return err
}
