package main

import (
	"context"
	"database/sql"
	"log"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	lru "github.com/hashicorp/golang-lru/v2"
)

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
