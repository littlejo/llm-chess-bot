package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/littlejo/llm-chess-bot/internal/lichess"
	"github.com/littlejo/llm-chess-bot/internal/llm"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeAPI records the calls made to Lichess. Game events are pushed through
// the game channel; closing it ends the stream.
type fakeAPI struct {
	mu        sync.Mutex
	game      chan lichess.GameEvent
	events    []lichess.Event
	moves     []string
	drawFlags []bool
	chats     []string
	resigned  bool
	accepted  []string
	declined  map[string]string
	streams   int
	moveErr   error
	notBot    bool
	aborted   bool
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{game: make(chan lichess.GameEvent, 16), declined: map[string]string{}}
}

func (f *fakeAPI) Account(context.Context) (lichess.Account, error) {
	if f.notBot {
		return lichess.Account{ID: "llmbot", Username: "LLMBot"}, nil
	}
	return lichess.Account{ID: "llmbot", Username: "LLMBot", Title: "BOT"}, nil
}

func (f *fakeAPI) StreamEvents(ctx context.Context, fn func(lichess.Event)) error {
	for _, ev := range f.events {
		fn(ev)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeAPI) AcceptChallenge(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepted = append(f.accepted, id)
	return nil
}

func (f *fakeAPI) DeclineChallenge(_ context.Context, id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.declined[id] = reason
	return nil
}

func (f *fakeAPI) StreamGame(ctx context.Context, _ string, fn func(lichess.GameEvent)) error {
	f.mu.Lock()
	f.streams++
	f.mu.Unlock()
	for {
		select {
		case ev, ok := <-f.game:
			if !ok {
				return nil
			}
			fn(ev)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *fakeAPI) Move(_ context.Context, _, uci string, draw bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.moveErr != nil {
		return f.moveErr
	}
	f.moves = append(f.moves, uci)
	f.drawFlags = append(f.drawFlags, draw)
	return nil
}

func (f *fakeAPI) Chat(_ context.Context, _, room, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chats = append(f.chats, text)
	return nil
}

func (f *fakeAPI) Abort(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted = true
	return nil
}

func (f *fakeAPI) Resign(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resigned = true
	return nil
}

func (f *fakeAPI) snapshot() (moves, chats []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.moves...), append([]string(nil), f.chats...)
}

// fakeLLM answers according to the kind of request found in the prompt.
type fakeLLM struct {
	mu       sync.Mutex
	prompts  []string // last user message of every call
	move     func(prompt string) (string, error)
	chat     string
	opening  string
	farewell string
	down     bool // every request fails, as when the gateway is unreachable
	slow     bool // move requests never answer before their deadline
}

func (f *fakeLLM) Complete(ctx context.Context, msgs []llm.Message) (string, error) {
	prompt := msgs[min(1, len(msgs)-1)].Content
	last := msgs[len(msgs)-1].Content
	f.mu.Lock()
	f.prompts = append(f.prompts, last)
	down, slow := f.down, f.slow
	f.mu.Unlock()
	if down {
		return "", errors.New("connection error")
	}
	if slow && strings.Contains(prompt, "It is your turn") {
		<-ctx.Done()
		return "", ctx.Err()
	}
	switch {
	case strings.Contains(prompt, "pong"):
		return "pong", nil
	case strings.Contains(prompt, "It is your turn"):
		return f.move(last)
	case strings.Contains(prompt, "New messages from your opponent"):
		return f.chat, nil
	case strings.Contains(prompt, "The game is starting"):
		return f.opening, nil
	case strings.Contains(prompt, "The game is over"):
		return f.farewell, nil
	}
	return "", context.DeadlineExceeded
}

func (f *fakeLLM) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...)
}

func fixedMove(answer string) func(string) (string, error) {
	return func(string) (string, error) { return answer, nil }
}

// waitFor polls cond until it is true or fails the test after 2 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func gameFull(meWhite bool, moves string) lichess.GameEvent {
	me := lichess.GamePlayer{ID: "llmbot", Name: "LLMBot", Title: "BOT"}
	opp := lichess.GamePlayer{ID: "bob", Name: "Bob", Rating: 1500}
	full := &lichess.GameFull{
		ID: "g1", InitialFEN: "startpos", Clock: &lichess.Clock{Initial: 300000, Increment: 3000},
		White: me, Black: opp,
		State: lichess.GameState{Moves: moves, WTime: 300000, BTime: 300000, WInc: 3000, BInc: 3000, Status: "started"},
	}
	if !meWhite {
		full.White, full.Black = opp, me
	}
	return lichess.GameEvent{Type: "gameFull", Full: full}
}

func gameState(moves, status string) lichess.GameEvent {
	return lichess.GameEvent{Type: "gameState", State: &lichess.GameState{
		Moves: moves, WTime: 290000, BTime: 290000, WInc: 3000, BInc: 3000, Status: status,
	}}
}

func chatLine(user, room, text string) lichess.GameEvent {
	return lichess.GameEvent{Type: "chatLine", Chat: &lichess.ChatLine{Username: user, Room: room, Text: text}}
}
