package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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

	db, err := sql.Open("sqlite", "/app/db")
	if err != nil {
		log.Fatalf("Failed to open database: %s", err.Error())
	}
	defer db.Close()

	createTagsTable := `CREATE TABLE IF NOT EXISTS tags (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		created_by INTEGER
	)`
	if _, err := db.Exec(createTagsTable); err != nil {
		log.Fatalf("Failed to create tags table: %s", err.Error())
	}

	createTagImageTable := `CREATE TABLE IF NOT EXISTS tag_images (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		tag_id INTEGER NOT NULL,
		file_name TEXT NOT NULL,
		FOREIGN KEY(tag_id) REFERENCES tags(id)
	)`
	if _, err := db.Exec(createTagImageTable); err != nil {
		log.Fatalf("Failed to create tag_images table: %s", err.Error())
	}

	createTagLimitsTable := `CREATE TABLE IF NOT EXISTS tag_limits (
		user_id INTEGER PRIMARY KEY,
		tag_limit INTEGER NOT NULL DEFAULT 100
	)`
	if _, err := db.Exec(createTagLimitsTable); err != nil {
		log.Fatalf("Failed to create tag_limits table: %s", err.Error())
	}

	if os.Getenv("ADMIN_ID") == "" {
		log.Fatal("ADMIN_ID environment variable is not set")
	}

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

type CommandHandler interface {
	Register(b *bot.Bot, me *models.User)
	Execute(c context.Context, b *bot.Bot, update *models.Update) error
}

type CommandFunc func(c context.Context, b *bot.Bot, update *models.Update, cache *lru.Cache[string, string], db *sql.DB, args ...string) error

type Command struct {
	Name string
	fn   CommandFunc
}

type SquidCommandHandler struct {
	db       *sql.DB
	cache    *lru.Cache[string, string]
	bot      *bot.Bot
	commands []Command
}

func (h *SquidCommandHandler) Register(commandName string, f CommandFunc) {
	h.commands = append(h.commands, Command{Name: commandName, fn: f})
}

func (h *SquidCommandHandler) Execute(c context.Context, update *models.Update, args []string) error {
	// If the first arg names a registered subcommand, route to it and strip
	// the subcommand name from the args passed to the handler. Otherwise fall
	// through to the "" handler with all args intact (bare /squid or /squid <tag>).
	command := ""
	cmdArgs := args
	if len(args) > 0 {
		for _, cmd := range h.commands {
			if cmd.Name != "" && cmd.Name == args[0] {
				command = args[0]
				cmdArgs = args[1:]
				break
			}
		}
	}

	for _, cmd := range h.commands {
		if cmd.Name == command {
			err := cmd.fn(c, h.bot, update, h.cache, h.db, cmdArgs...)
			if err != nil {
				log.Printf("Error executing command %s: %v", cmd.Name, err)
				sendText(c, h.bot, update, "Something went wrong D:\n"+err.Error())
			}

			return err
		}
	}

	return nil
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

	sendText(c, b, update, "thanks! added your image to the squid pool")
	return nil
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

func tagsCaption(db *sql.DB, fileName string) (string, error) {
	rows, err := db.Query(
		"SELECT DISTINCT t.name FROM tags t JOIN tag_images ti ON ti.tag_id = t.id WHERE ti.file_name = ?",
		fileName,
	)
	if err != nil {
		return "", fmt.Errorf("failed to query tags for image: %s", err.Error())
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", fmt.Errorf("failed to scan tag name: %s", err.Error())
		}
		tags = append(tags, name)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("failed to read tags for image: %s", err.Error())
	}

	return strings.Join(tags, ", "), nil
}

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

func queryTagNames(db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query tags: %s", err.Error())
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("failed to scan tag name: %s", err.Error())
		}
		tags = append(tags, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read tags: %s", err.Error())
	}

	return tags, nil
}

const defaultTagLimit = 100

// rowQuerier is satisfied by both *sql.DB and *sql.Tx.
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func tagLimit(q rowQuerier, userID int64) (int, error) {
	var limit int
	err := q.QueryRow("SELECT tag_limit FROM tag_limits WHERE user_id = ?", userID).Scan(&limit)
	if err == sql.ErrNoRows {
		return defaultTagLimit, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to query tag limit: %s", err.Error())
	}
	return limit, nil
}

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

func sendText(c context.Context, b *bot.Bot, update *models.Update, text string) error {
	_, err := b.SendMessage(c, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   text,
	})
	return err
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
