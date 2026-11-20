// Package bot connects Lichess and the LLM: it filters challenges, starts a
// Session per game and lets the LLM play and chat.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/littlejo/llm-chess-bot/internal/config"
	"github.com/littlejo/llm-chess-bot/internal/lichess"
)

// Bot follows the Lichess event stream.
type Bot struct {
	API    API
	Brain  *Brain
	Config config.Config
	Log    *slog.Logger

	me    string
	mu    sync.Mutex
	games map[string]context.CancelFunc
	wg    sync.WaitGroup
}

// Run reads the event stream, reconnecting as needed, until ctx is canceled.
// Running games are left alone on shutdown: Lichess resends them when the
// bot restarts.
func (b *Bot) Run(ctx context.Context) error {
	acct, err := b.API.Account(ctx)
	if err != nil {
		return err
	}
	// Without the BOT title, the Bot API rejects game streams and moves, so
	// accepting a challenge would only create a game the bot cannot play,
	// and a game played makes the account impossible to upgrade.
	if acct.Title != "BOT" {
		return fmt.Errorf("account %q is not a BOT account: upgrade it first with POST /api/bot/account/upgrade (see README)", acct.Username)
	}
	b.me = strings.ToLower(acct.ID)
	b.games = map[string]context.CancelFunc{}
	b.Log.Info("connected", "account", acct.Username, "model", b.Config.Model)

	defer b.wg.Wait()
	err = lichess.Reconnect(ctx, b.Log, "events", false, func(ctx context.Context) error {
		return b.API.StreamEvents(ctx, func(ev lichess.Event) { b.handleEvent(ctx, ev) })
	})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (b *Bot) handleEvent(ctx context.Context, ev lichess.Event) {
	switch ev.Type {
	case "challenge":
		if ev.Challenge != nil {
			b.onChallenge(ctx, *ev.Challenge)
		}
	case "gameStart":
		if ev.Game != nil {
			b.startGame(ctx, ev.Game.GameID)
		}
	case "gameFinish":
		if ev.Game != nil {
			b.Log.Info("game finish event", "game", ev.Game.GameID)
		}
	default:
		b.Log.Debug("event", "type", ev.Type)
	}
}

func (b *Bot) onChallenge(ctx context.Context, ch lichess.Challenge) {
	if ch.Direction == "out" || strings.ToLower(ch.Challenger.ID) == b.me {
		return
	}
	log := b.Log.With("challenge", ch.ID, "from", ch.Challenger.Name, "rating", ch.Challenger.Rating,
		"variant", ch.Variant.Key, "speed", ch.Speed, "rated", ch.Rated)
	ok, reason := EvaluateChallenge(b.Config.Challenges, ch, b.activeGames() >= b.Config.Bot.MaxGames)
	if !ok {
		log.Info("declining challenge", "reason", reason)
		if err := b.API.DeclineChallenge(ctx, ch.ID, reason); err != nil {
			log.Warn("decline failed", "err", err)
		}
		return
	}
	log.Info("accepting challenge")
	if err := b.API.AcceptChallenge(ctx, ch.ID); err != nil {
		log.Warn("accept failed", "err", err)
	}
}

func (b *Bot) activeGames() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.games)
}

// startGame starts a session unless one already runs for this game (the
// event stream resends gameStart for running games after a reconnection).
func (b *Bot) startGame(ctx context.Context, id string) {
	b.mu.Lock()
	if _, ok := b.games[id]; ok {
		b.mu.Unlock()
		return
	}
	gctx, cancel := context.WithCancel(ctx)
	b.games[id] = cancel
	b.mu.Unlock()

	log := b.Log.With("game", id)
	openings := b.Config.Openings
	if b.Config.OpeningsFile != "" {
		o, err := config.LoadOpenings(b.Config.OpeningsFile)
		if err != nil {
			log.Warn("cannot read openings file, using the configured openings", "err", err)
		} else {
			openings = o
		}
	}
	sess := NewSession(id, b.me, b.API, b.Brain, b.Config.Bot, openings, log)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() {
			cancel()
			b.mu.Lock()
			delete(b.games, id)
			b.mu.Unlock()
		}()
		sess.Run(gctx)
	}()
}
