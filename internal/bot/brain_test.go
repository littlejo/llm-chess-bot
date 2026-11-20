package bot

import (
	"context"
	"strings"
	"testing"

	"github.com/littlejo/llm-chess-bot/internal/board"
)

func TestParseDecision(t *testing.T) {
	pos, _ := board.FromMoves(board.StartPos, []string{"e2e4", "e7e5"})
	tests := []struct {
		name    string
		raw     string
		want    Decision
		wantErr bool
	}{
		{name: "json", raw: `{"move":"g1f3","chat":" hi ","offer_draw":false}`, want: Decision{Move: "g1f3", Chat: "hi"}},
		{name: "fenced SAN", raw: "Sure!\n```json\n{\"move\": \"Nf3\"}\n```", want: Decision{Move: "g1f3"}},
		{name: "draw", raw: `{"move":"d2d4","offer_draw":true}`, want: Decision{Move: "d2d4", OfferDraw: true}},
		{name: "resign", raw: `{"move":"","resign":true,"chat":"gg"}`, want: Decision{Resign: true, Chat: "gg"}},
		{name: "free text", raw: "I think e7e5 is bad, I play g1f3.", want: Decision{Move: "g1f3"}},
		{name: "illegal json", raw: `{"move":"e2e5"}`, wantErr: true},
		{name: "garbage", raw: "no idea", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseDecision(tt.raw, pos)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("%s: got %+v, %v; want %+v (err %v)", tt.name, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestReplyChatToleratesPlainText(t *testing.T) {
	pos, _ := board.FromMoves(board.StartPos, nil)
	b := &Brain{LLM: &fakeLLM{chat: "Hello! Have a nice game."}, Log: discardLogger()}
	r, err := b.ReplyChat(context.Background(), Snapshot{Color: board.White, Pos: pos}, []string{"hi"})
	if err != nil || r.Reply != "Hello! Have a nice game." || r.Intent != "conversation" {
		t.Fatalf("reply = %+v, %v", r, err)
	}
}

func TestPlanOpeningKeepsLegalPrefix(t *testing.T) {
	pos, _ := board.FromMoves(board.StartPos, nil)
	b := &Brain{LLM: &fakeLLM{opening: `{"name":"Scotch","moves":"1. e4 e5 2. Nf3 Nc6 3. d4 exd4 4. Bxd4","greeting":"Scotch time!"}`}, Log: discardLogger()}
	plan, greeting, err := b.PlanOpening(context.Background(), Snapshot{Color: board.White, Pos: pos})
	if err != nil || greeting != "Scotch time!" || plan.Name != "Scotch" {
		t.Fatalf("plan = %+v, %q, %v", plan, greeting, err)
	}
	if got := plan.Line.String(); got != "1. e4 e5 2. Nf3 Nc6 3. d4 exd4" {
		t.Fatalf("line = %q", got)
	}
}

func TestPlanOpeningKeepsConfiguredLine(t *testing.T) {
	pos, _ := board.FromMoves(board.StartPos, nil)
	line, _ := board.ParseLine("Italian", "e4 e5 Nf3 Nc6 Bc4")
	b := &Brain{LLM: &fakeLLM{opening: `{"name":"Other","moves":"1. d4","greeting":"hi"}`}, Log: discardLogger()}
	plan, _, err := b.PlanOpening(context.Background(), Snapshot{Color: board.White, Pos: pos, Plan: Plan{Name: "Italian", Line: line}})
	if err != nil || plan.Name != "Italian" || plan.Line.String() != line.String() {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
}

func TestSplitChat(t *testing.T) {
	if got := splitChat("  short   message ", 2); len(got) != 1 || got[0] != "short message" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("word ", 80) // 400 chars
	got := splitChat(long, 2)
	if len(got) != 2 {
		t.Fatalf("parts = %d", len(got))
	}
	for _, p := range got {
		if len([]rune(p)) > MaxChatLength || strings.HasPrefix(p, " ") || strings.HasSuffix(p, "wor") {
			t.Fatalf("bad part %q", p)
		}
	}
	noSpace := strings.Repeat("é", 300) // multi-byte runes, no space to split on
	if got := splitChat(noSpace, 3); len(got) != 3 || len([]rune(got[0])) != MaxChatLength {
		t.Fatalf("got %d parts", len(got))
	}
}
