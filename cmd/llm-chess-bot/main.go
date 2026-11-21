// Command llm-chess-bot plays chess on Lichess with an LLM and chats with
// its opponents.
//
// Usage:
//
//	llm-chess-bot -config config.json          # play
//	llm-chess-bot -config config.json -check   # check the Lichess token and the LLM, then exit
//
// See README.md for the configuration.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/littlejo/llm-chess-bot/internal/bot"
	"github.com/littlejo/llm-chess-bot/internal/config"
	"github.com/littlejo/llm-chess-bot/internal/lichess"
	"github.com/littlejo/llm-chess-bot/internal/llm"
)

func main() {
	configPath := flag.String("config", "config.json", "configuration file (empty: environment variables only)")
	check := flag.Bool("check", false, "check the Lichess token and the LLM connection, then exit")
	flag.Parse()

	if err := run(*configPath, *check); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(configPath string, check bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log)

	name, providerCfg, model, err := cfg.LLM()
	if err != nil {
		return err
	}
	provider, err := llm.New(name, providerCfg, model)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := lichess.NewClient(cfg.Lichess.BaseURL, cfg.Lichess.Token, cfg.Lichess.UserAgent, log)
	if check {
		return runCheck(ctx, client, provider, cfg.Model)
	}

	b := &bot.Bot{
		API: client,
		Brain: &bot.Brain{
			LLM:         provider,
			Persona:     cfg.Bot.Persona,
			MoveRetries: cfg.Bot.MoveRetries,
			Log:         log,
		},
		Config: cfg,
		Log:    log,
	}
	err = b.Run(ctx)
	log.Info("bot stopped")
	return err
}

// runCheck verifies both external services without playing.
func runCheck(ctx context.Context, client *lichess.Client, provider llm.Provider, model string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	acct, err := client.Account(ctx)
	if err != nil {
		return fmt.Errorf("checking the Lichess token: %w", err)
	}
	fmt.Printf("lichess: OK, account %q, title %q\n", acct.Username, acct.Title)
	if acct.Title != "BOT" {
		fmt.Println("lichess: WARNING, this is not a BOT account yet (see README)")
	}

	answer, err := provider.Complete(ctx, []llm.Message{
		{Role: "user", Content: "Reply with the single word: pong"},
	})
	if err != nil {
		return fmt.Errorf("checking the LLM: %w", err)
	}
	fmt.Printf("llm: OK, model %q answered: %s\n", model, strings.TrimSpace(answer))
	return nil
}

func newLogger(cfg config.Log) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(cfg.Format, "text") {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
