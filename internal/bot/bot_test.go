package bot

import (
	"context"
	"strings"
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

func TestBotRefusesNonBotAccount(t *testing.T) {
	api := newFakeAPI()
	api.notBot = true
	ok := lichess.Challenge{ID: "ok", Variant: lichess.Variant{Key: "standard"},
		TimeControl: lichess.TimeControl{Type: "clock", Limit: 600}, Challenger: lichess.User{ID: "bob"}}
	api.events = []lichess.Event{{Type: "challenge", Challenge: &ok}}
	b := &Bot{API: api, Brain: &Brain{LLM: &fakeLLM{}, Log: discardLogger()}, Config: config.Default(), Log: discardLogger()}

	err := b.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not a BOT account") {
		t.Fatalf("err = %v", err)
	}
	if len(api.accepted) != 0 {
		t.Fatalf("a challenge was accepted with a non-BOT account: %v", api.accepted)
	}
}

func TestBotRefusesToStartWhenLLMIsDown(t *testing.T) {
	api := newFakeAPI()
	b := &Bot{API: api, Brain: &Brain{LLM: &fakeLLM{down: true}, Log: discardLogger()}, Config: config.Default(), Log: discardLogger()}
	if err := b.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("err = %v", err)
	}
}

func TestBotDeclinesChallengesWhileLLMIsDown(t *testing.T) {
	model := &fakeLLM{}
	brain := &Brain{LLM: model, Log: discardLogger()}
	if !brain.Available() {
		t.Fatal("a fresh brain must be available")
	}
	model.down = true
	brain.Ping(context.Background())
	if brain.Available() {
		t.Fatal("brain must be unavailable after a failure")
	}

	api := newFakeAPI()
	b := &Bot{API: api, Brain: brain, Config: config.Default(), Log: discardLogger(), games: map[string]context.CancelFunc{}}
	b.onChallenge(context.Background(), lichess.Challenge{ID: "c", Variant: lichess.Variant{Key: "standard"},
		TimeControl: lichess.TimeControl{Type: "clock", Limit: 600}, Challenger: lichess.User{ID: "bob"}})
	if api.declined["c"] != "later" {
		t.Fatalf("declined = %v, accepted = %v", api.declined, api.accepted)
	}

	model.down = false
	brain.Ping(context.Background())
	if !brain.Available() {
		t.Fatal("a success must make the brain available again")
	}
}
