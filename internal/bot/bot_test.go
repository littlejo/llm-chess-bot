package bot

import (
	"context"
	"testing"
	"time"

	"github.com/littlejo/llm-chess-bot/internal/config"
	"github.com/littlejo/llm-chess-bot/internal/lichess"
)

func TestBotHandlesChallengesAndDeduplicatesGames(t *testing.T) {
	api := newFakeAPI()
	ok := lichess.Challenge{ID: "ok", Variant: lichess.Variant{Key: "standard"},
		TimeControl: lichess.TimeControl{Type: "clock", Limit: 600}, Challenger: lichess.User{ID: "bob"}}
	fast := ok
	fast.ID, fast.TimeControl.Limit = "fast", 60
	mine := ok
	mine.ID, mine.Direction = "mine", "out"
	api.events = []lichess.Event{
		{Type: "challenge", Challenge: &ok},
		{Type: "challenge", Challenge: &fast},
		{Type: "challenge", Challenge: &mine},
		{Type: "gameStart", Game: &lichess.GameInfo{GameID: "g1"}},
		{Type: "gameStart", Game: &lichess.GameInfo{GameID: "g1"}}, // resent after a reconnection
	}
	cfg := config.Default()
	b := &Bot{API: api, Brain: &Brain{LLM: &fakeLLM{}, Log: discardLogger()}, Config: cfg, Log: discardLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- b.Run(ctx) }()
	waitFor(t, "game stream", func() bool { api.mu.Lock(); defer api.mu.Unlock(); return api.streams >= 1 })
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.accepted) != 1 || api.accepted[0] != "ok" {
		t.Errorf("accepted = %v", api.accepted)
	}
	if len(api.declined) != 1 || api.declined["fast"] != "tooFast" {
		t.Errorf("declined = %v", api.declined)
	}
	if api.streams != 1 {
		t.Errorf("game streams opened = %d, want 1", api.streams)
	}
}
