package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	lru "github.com/hashicorp/golang-lru/v2"
)

func handleSubmit(c context.Context, b *bot.Bot, update *models.Update, cache *lru.Cache[string, string], db *sql.DB, args ...string) error {
	adminID := os.Getenv("ADMIN_ID")
	if update.Message.From == nil || adminID == "" || strconv.FormatInt(update.Message.From.ID, 10) != adminID {
		sendText(c, b, update, "only @surgethewolf can use this command")
		return nil
	}

	reply := update.Message.ReplyToMessage
	if reply == nil {
		sendText(c, b, update, "reply to an image to submit it to the squid pool")
		return nil
	}

	fileID := replyImageFileID(reply)
	if fileID == "" {
		sendText(c, b, update, "the replied-to message does not contain an image")
		return nil
	}

	// resolve the file path on Telegram's servers
	file, err := b.GetFile(c, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return fmt.Errorf("failed to get file: %s", err.Error())
	}

	// name the saved file after its unique id so re-submissions are idempotent
	ext := filepath.Ext(file.FilePath)
	if ext == "" {
		ext = ".jpg"
	}
	dest := filepath.Join("/app/squid", file.FileUniqueID+ext)

	if _, err := os.Stat(dest); err == nil {
		sendText(c, b, update, "this image is already in the squid pool")
		return nil
	}

	link := b.FileDownloadLink(file)
	req, err := http.NewRequestWithContext(c, http.MethodGet, link, nil)
	if err != nil {
		return fmt.Errorf("failed to build download request: %s", err.Error())
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download file: %s", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download file: unexpected status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read downloaded file: %s", err.Error())
	}

	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return fmt.Errorf("failed to save image: %s", err.Error())
	}

	sendText(c, b, update, "added image")
	return nil
}
