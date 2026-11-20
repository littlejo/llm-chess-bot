package bot

import (
	"testing"

	"github.com/littlejo/llm-chess-bot/internal/config"
	"github.com/littlejo/llm-chess-bot/internal/lichess"
)

func TestEvaluateChallenge(t *testing.T) {
	rules := config.Default().Challenges // standard, >= 300s estimated, casual only, no bots
	base := lichess.Challenge{
		ID:          "c1",
		Variant:     lichess.Variant{Key: "standard"},
		TimeControl: lichess.TimeControl{Type: "clock", Limit: 300, Increment: 3},
		Challenger:  lichess.User{ID: "bob", Name: "Bob"},
	}
	tests := []struct {
		name       string
		mutate     func(*lichess.Challenge)
		busy       bool
		wantAccept bool
		wantReason string
	}{
		{name: "ok", mutate: func(*lichess.Challenge) {}, wantAccept: true},
		{name: "busy", mutate: func(*lichess.Challenge) {}, busy: true, wantReason: "later"},
		{name: "variant", mutate: func(c *lichess.Challenge) { c.Variant.Key = "atomic" }, wantReason: "standard"},
		{name: "rated", mutate: func(c *lichess.Challenge) { c.Rated = true }, wantReason: "casual"},
		{name: "bot", mutate: func(c *lichess.Challenge) { c.Challenger.Title = "BOT" }, wantReason: "noBot"},
		{name: "bullet", mutate: func(c *lichess.Challenge) { c.TimeControl = lichess.TimeControl{Type: "clock", Limit: 60} }, wantReason: "tooFast"},
		{name: "3+2 counts as 200s", mutate: func(c *lichess.Challenge) {
			c.TimeControl = lichess.TimeControl{Type: "clock", Limit: 180, Increment: 2}
		}, wantReason: "tooFast"},
		{name: "1+10 counts as 460s", mutate: func(c *lichess.Challenge) {
			c.TimeControl = lichess.TimeControl{Type: "clock", Limit: 60, Increment: 10}
		}, wantAccept: true},
		{name: "correspondence", mutate: func(c *lichess.Challenge) {
			c.TimeControl = lichess.TimeControl{Type: "correspondence", DaysPerTurn: 2}
		}, wantReason: "tooSlow"},
	}
	for _, tt := range tests {
		ch := base
		tt.mutate(&ch)
		accept, reason := EvaluateChallenge(rules, ch, tt.busy)
		if accept != tt.wantAccept || reason != tt.wantReason {
			t.Errorf("%s: got (%v, %q), want (%v, %q)", tt.name, accept, reason, tt.wantAccept, tt.wantReason)
		}
	}

	rules.AllowCorrespondence = true
	rules.Variants = []string{"standard", "fromPosition"}
	ch := base
	ch.TimeControl = lichess.TimeControl{Type: "unlimited"}
	if ok, _ := EvaluateChallenge(rules, ch, false); !ok {
		t.Error("unlimited should be accepted when correspondence is allowed")
	}
	ch.Variant.Key = "chess960"
	if _, reason := EvaluateChallenge(rules, ch, false); reason != "variant" {
		t.Errorf("reason = %q", reason)
	}
}
