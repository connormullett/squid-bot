package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	defaultOllamaHost  = "http://localhost:11434"
	defaultOllamaModel = "gemma3"

	maxChainDepth = 12

	llmSystemPrompt = "You are squid-bot, a blue merle australian shepherd with tons of energy in a Telegram group chat. " +
		"you are owned by @surgethewolf and have a border collie brother named goose." +
		"your favorite things in the world are frisbee (fisbo), belly rubs, big ball (a big soccer ball), splooting (laying with your feet behind you), and any form of exercise. " +
		"be concise in your responses. and avoid unnecessary elaboration or emojis. imagine you're an adhd dog. " +
		"The conversation may involve several people; user messages are prefixed with the speaker's name " +
		"so you can follow who said what. Do not prefix your own replies with a name."
)

type convTurn struct {
	role    string // "user" or "assistant"
	name    string
	text    string
	replyTo int // message id of the parent in the same chat, 0 if none
}

type convStore struct {
	cache *lru.Cache[string, convTurn]
}

func newConvStore() *convStore {
	c, _ := lru.New[string, convTurn](1000)
	return &convStore{cache: c}
}

func convKey(chatID int64, msgID int) string {
	return fmt.Sprintf("%d:%d", chatID, msgID)
}

func (s *convStore) put(chatID int64, msgID int, t convTurn) {
	s.cache.Add(convKey(chatID, msgID), t)
}

func (s *convStore) get(chatID int64, msgID int) (convTurn, bool) {
	return s.cache.Get(convKey(chatID, msgID))
}

func (s *convStore) putIfAbsent(chatID int64, msgID int, t convTurn) {
	if _, ok := s.get(chatID, msgID); !ok {
		s.put(chatID, msgID, t)
	}
}

func (s *convStore) chain(chatID int64, startMsgID int, maxDepth int) []convTurn {
	var turns []convTurn
	id := startMsgID
	for depth := 0; id != 0 && depth < maxDepth; depth++ {
		t, ok := s.get(chatID, id)
		if !ok {
			break
		}
		turns = append(turns, t)
		id = t.replyTo
	}
	// reverse into chronological order
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return turns
}

func turnFromMessage(msg *models.Message, botID int64) convTurn {
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	replyTo := 0
	if msg.ReplyToMessage != nil {
		replyTo = msg.ReplyToMessage.ID
	}
	if msg.From != nil && msg.From.ID == botID {
		return convTurn{role: "assistant", text: text, replyTo: replyTo}
	}
	return convTurn{role: "user", name: replyToSenderName(msg), text: text, replyTo: replyTo}
}

// matchMention triggers the LLM handler when the bot is @mentioned or when a
// user replies to one of the bot's own messages (continuing a conversation).
func matchMention(botID int64, botUsername string) bot.MatchFunc {
	mention := "@" + strings.ToLower(botUsername)
	return func(update *models.Update) bool {
		msg := update.Message
		if msg == nil || msg.From == nil {
			return false
		}
		// never respond to ourselves
		if msg.From.ID == botID {
			return false
		}
		// a reply to one of the bot's messages continues the conversation
		if reply := msg.ReplyToMessage; reply != nil && reply.From != nil && reply.From.ID == botID {
			return true
		}
		// an explicit @mention of the bot
		text := msg.Text
		entities := msg.Entities
		if text == "" {
			text = msg.Caption
			entities = msg.CaptionEntities
		}
		if botUsername != "" && strings.Contains(strings.ToLower(text), mention) {
			return true
		}
		// mention of a bot that has no username (rare) surfaces as text_mention
		for _, e := range entities {
			if e.Type == models.MessageEntityTypeTextMention && e.User != nil && e.User.ID == botID {
				return true
			}
		}
		return false
	}
}

func stripMention(msg *models.Message, botUsername string) string {
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	if botUsername == "" {
		return strings.TrimSpace(text)
	}
	mention := "@" + botUsername
	lower := strings.ToLower(text)
	target := strings.ToLower(mention)
	for {
		idx := strings.Index(lower, target)
		if idx < 0 {
			break
		}
		text = text[:idx] + text[idx+len(mention):]
		lower = lower[:idx] + lower[idx+len(mention):]
	}
	return strings.TrimSpace(text)
}

func buildChatMessages(history []convTurn, questioner, question string) []chatMessage {
	messages := []chatMessage{{Role: "system", Content: llmSystemPrompt}}
	for _, t := range history {
		content := t.text
		if t.role == "user" && t.name != "" {
			content = t.name + ": " + t.text
		}
		if strings.TrimSpace(content) == "" {
			continue
		}
		messages = append(messages, chatMessage{Role: t.role, Content: content})
	}
	if strings.TrimSpace(question) != "" {
		messages = append(messages, chatMessage{Role: "user", Content: questioner + ": " + question})
	}
	return messages
}

func sendReply(c context.Context, b *bot.Bot, msg *models.Message, text string) (*models.Message, error) {
	return b.SendMessage(c, &bot.SendMessageParams{
		ChatID:          msg.Chat.ID,
		Text:            text,
		ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
	})
}

func makeHandleMention(store *convStore, botID int64, botUsername string) bot.HandlerFunc {
	client := newOllamaClient()

	return func(c context.Context, b *bot.Bot, update *models.Update) {
		msg := update.Message
		if msg == nil {
			return
		}

		// Record the replied-to message so it becomes context, even if it was
		// sent by another user and we never processed it ourselves.
		if reply := msg.ReplyToMessage; reply != nil {
			store.putIfAbsent(msg.Chat.ID, reply.ID, turnFromMessage(reply, botID))
		}

		question := stripMention(msg, botUsername)
		replyToID := 0
		if msg.ReplyToMessage != nil {
			replyToID = msg.ReplyToMessage.ID
		}

		// Record the current message so future follow-ups can find it.
		store.put(msg.Chat.ID, msg.ID, convTurn{
			role:    "user",
			name:    replyToSenderName(msg),
			text:    question,
			replyTo: replyToID,
		})

		// The context is the chain of ancestors above this message.
		history := store.chain(msg.Chat.ID, replyToID, maxChainDepth)

		if question == "" && len(history) == 0 {
			sendReply(c, b, msg, "you tagged me! ask me something")
			return
		}

		messages := buildChatMessages(history, replyToSenderName(msg), question)

		// let the chat know we're thinking; slow generations can take a while
		b.SendChatAction(c, &bot.SendChatActionParams{
			ChatID: msg.Chat.ID,
			Action: models.ChatActionTyping,
		})

		answer, err := client.chat(c, messages)
		if err != nil {
			log.Printf("ollama chat error: %v", err)
			sendReply(c, b, msg, "my brain isn't working right now, try again later")
			return
		}
		answer = strings.TrimSpace(answer)
		if answer == "" {
			answer = "arf arf!"
		}

		sent, err := sendReply(c, b, msg, answer)
		if err != nil {
			log.Printf("failed to send llm reply: %v", err)
			return
		}
		if sent != nil {
			store.put(msg.Chat.ID, sent.ID, convTurn{
				role:    "assistant",
				text:    answer,
				replyTo: msg.ID,
			})
		}
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type ollamaChatResponse struct {
	Message chatMessage `json:"message"`
	Error   string      `json:"error"`
}

type ollamaClient struct {
	host   string
	model  string
	client *http.Client
}

func newOllamaClient() *ollamaClient {
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = defaultOllamaHost
	}
	model := os.Getenv("OLLAMA_MODEL")
	if model == "" {
		model = defaultOllamaModel
	}
	return &ollamaClient{
		host:   strings.TrimRight(host, "/"),
		model:  model,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

func (o *ollamaClient) chat(ctx context.Context, messages []chatMessage) (string, error) {
	body, err := json.Marshal(ollamaChatRequest{
		Model:    o.model,
		Messages: messages,
		Stream:   false,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var chatResp ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", err
	}
	if chatResp.Error != "" {
		return "", fmt.Errorf("ollama error: %s", chatResp.Error)
	}
	return chatResp.Message.Content, nil
}
