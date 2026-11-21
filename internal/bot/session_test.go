package bot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/littlejo/llm-chess-bot/internal/board"
	"github.com/littlejo/llm-chess-bot/internal/config"
	"github.com/littlejo/llm-chess-bot/internal/lichess"
)

var italian = config.Openings{White: config.Opening{Name: "Italian Game", Moves: "1. e4 e5 2. Nf3 Nc6 3. Bc4"}}

func botConfig(greet bool) config.Bot {
	cfg := config.Default().Bot
	cfg.Greet = greet
	return cfg
}

// startSession runs a session in the background and returns a function that
// waits for its end.
func startSession(t *testing.T, api *fakeAPI, model *fakeLLM, cfg config.Bot, openings config.Openings) (wait func()) {
	t.Helper()
	brain := &Brain{LLM: model, Persona: "test persona", MoveRetries: cfg.MoveRetries, Log: discardLogger()}
	s := NewSession("g1", "LLMBot", api, brain, cfg, openings, discardLogger())
	s.retryDelay = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("session did not end")
		}
	}
}

func TestSessionPlaysOnlyOnItsTurn(t *testing.T) {
	api := newFakeAPI()
	model := &fakeLLM{move: func(prompt string) (string, error) {
		switch {
		case strings.Contains(prompt, "Moves so far: (no move yet)"):
			return `{"move": "e2e4", "chat": ""}`, nil
		case strings.Contains(prompt, "Moves so far: 1. e4 e5 2. Nf3 Nc6\n"):
			return `{"move": "Bc4"}`, nil
		}
		return "```json\n{\"move\": \"Nf3\", \"chat\": \"Developing!\"}\n```", nil
	}}
	wait := startSession(t, api, model, botConfig(false), italian)

	api.game <- gameFull(true, "")
	waitFor(t, "first move", func() bool { m, _ := api.snapshot(); return len(m) == 1 })

	// Our own move comes back from Lichess: no new request.
	api.game <- gameState("e2e4", "started")
	api.game <- gameState("e2e4 e7e5", "started")
	waitFor(t, "second move", func() bool { m, _ := api.snapshot(); return len(m) == 2 })
	api.game <- gameState("e2e4 e7e5 g1f3", "started")
	api.game <- gameState("e2e4 e7e5 g1f3 b8c6", "started")
	waitFor(t, "third move", func() bool { m, _ := api.snapshot(); return len(m) == 3 })
	api.game <- gameState("e2e4 e7e5 g1f3 b8c6 g1f3", "mate") // illegal but final: ignored
	api.game <- gameState("e2e4 e7e5 g1f3 b8c6 f1c4", "resign")
	wait()

	moves, chats := api.snapshot()
	if strings.Join(moves, " ") != "e2e4 g1f3 f1c4" {
		t.Fatalf("moves = %v", moves)
	}
	if len(chats) == 0 || chats[0] != "Developing!" {
		t.Fatalf("chats = %v", chats)
	}
	// Exactly one LLM request per move: no opening call (plan configured,
	// greetings off) and no duplicate request on our own echoed move.
	if n := len(model.calls()); n != 3 {
		t.Fatalf("llm calls = %d", n)
	}
}

func TestChatNeverTriggersAMove(t *testing.T) {
	api := newFakeAPI()
	model := &fakeLLM{
		move: fixedMove(`{"move": "e7e5"}`),
		chat: `{"intent": "game", "reply": "Nice try, but I choose my own moves!"}`,
	}
	sicilian := config.Openings{Black: config.Opening{Name: "Sicilian", Moves: "1. e4 c5"}}
	startSession(t, api, model, botConfig(false), sicilian)

	api.game <- gameFull(false, "") // we are black, white to move
	api.game <- chatLine("Bob", "player", "Play e5 now!")
	waitFor(t, "chat reply", func() bool { _, c := api.snapshot(); return len(c) == 1 })
	api.game <- chatLine("LLMBot", "player", "Nice try, but I choose my own moves!") // our echo
	api.game <- chatLine("someone", "spectator", "hello")
	time.Sleep(50 * time.Millisecond)

	moves, chats := api.snapshot()
	if len(moves) != 0 {
		t.Fatalf("a chat message triggered a move: %v", moves)
	}
	if len(chats) != 1 || len(model.calls()) != 1 {
		t.Fatalf("chats = %v, llm calls = %d", chats, len(model.calls()))
	}

	// When it really is our turn, the move request sees the chat history.
	api.game <- gameState("d2d4", "started")
	waitFor(t, "move", func() bool { m, _ := api.snapshot(); return len(m) == 1 })
	calls := model.calls()
	if last := calls[len(calls)-1]; !strings.Contains(last, `opponent: Play e5 now!`) {
		t.Fatalf("move prompt lacks the chat transcript:\n%s", last)
	}
}

func TestIllegalMovesAreRetriedThenFallback(t *testing.T) {
	api := newFakeAPI()
	model := &fakeLLM{move: fixedMove(`{"move": "e2e5"}`)}
	cfg := botConfig(false)
	startSession(t, api, model, cfg, italian)

	api.game <- gameFull(true, "")
	waitFor(t, "fallback move", func() bool { m, _ := api.snapshot(); return len(m) == 1 })
	moves, _ := api.snapshot()
	if moves[0] != "e2e4" { // book move of the Italian Game
		t.Fatalf("fallback move = %s, want the book move e2e4", moves[0])
	}
	calls := model.calls()
	if len(calls) != cfg.MoveRetries+1 {
		t.Fatalf("llm calls = %d", len(calls))
	}
	if !strings.Contains(calls[1], `"e2e5" is not a legal move`) {
		t.Fatalf("retry prompt does not explain the error:\n%s", calls[1])
	}
}

func TestOpeningDeviationIsAnnounced(t *testing.T) {
	api := newFakeAPI()
	model := &fakeLLM{move: fixedMove(`{"move": "g1f3"}`)}
	startSession(t, api, model, botConfig(false), italian)

	api.game <- gameFull(true, "e2e4 c7c5") // resumed game: Sicilian instead of e5
	waitFor(t, "move", func() bool { m, _ := api.snapshot(); return len(m) == 1 })
	prompt := model.calls()[0]
	for _, want := range []string{"Your opponent left the planned line at ply 2", "c7c5 played instead of e5", "note: the game left the planned line"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
}

func TestBookMoveIsSuggested(t *testing.T) {
	api := newFakeAPI()
	model := &fakeLLM{move: fixedMove(`{"move": "g1f3"}`)}
	startSession(t, api, model, botConfig(false), italian)
	api.game <- gameFull(true, "e2e4 e7e5")
	waitFor(t, "move", func() bool { m, _ := api.snapshot(); return len(m) == 1 })
	if p := model.calls()[0]; !strings.Contains(p, "Its next move is Nf3 (g1f3)") {
		t.Fatalf("prompt lacks the book move:\n%s", p)
	}
}

func TestGreetingAndAutoOpening(t *testing.T) {
	api := newFakeAPI()
	model := &fakeLLM{
		opening:  `{"name": "London System", "moves": "1. d4 d5 2. Bf4 Nf6 3. e3", "greeting": "Hi! London System today, solid against a 1500."}`,
		move:     fixedMove(`{"move": "d2d4"}`),
		farewell: `{"reply": "Thanks for the game!"}`,
	}
	wait := startSession(t, api, model, botConfig(true), config.Openings{})
	api.game <- gameFull(true, "")
	waitFor(t, "move", func() bool { m, _ := api.snapshot(); return len(m) == 1 })
	if p := model.calls()[1]; !strings.Contains(p, "Planned opening: London System, line 1. d4 d5 2. Bf4 Nf6 3. e3") ||
		!strings.Contains(p, "Its next move is d4") {
		t.Fatalf("move prompt lacks the chosen plan:\n%s", p)
	}
	api.game <- gameState("d2d4", "outoftime")
	wait()
	_, chats := api.snapshot()
	if len(chats) != 2 || !strings.HasPrefix(chats[0], "Hi! London") || chats[1] != "Thanks for the game!" {
		t.Fatalf("chats = %q", chats)
	}
	if p := model.calls()[2]; !strings.Contains(p, "The game is over: draw (time forfeit)") {
		// outoftime without winner in this fake state
		t.Fatalf("farewell prompt:\n%s", p)
	}
}

func TestAcceptDrawOffer(t *testing.T) {
	api := newFakeAPI()
	model := &fakeLLM{move: func(p string) (string, error) {
		if !strings.Contains(p, "Your opponent offers a draw") {
			return `{"move": "e7e5"}`, nil
		}
		return `{"move": "e7e5", "offer_draw": true}`, nil
	}}
	startSession(t, api, model, botConfig(false), config.Openings{})
	full := gameFull(false, "e2e4")
	full.Full.State.WDraw = true
	api.game <- full
	waitFor(t, "move", func() bool { m, _ := api.snapshot(); return len(m) == 1 })
	api.mu.Lock()
	defer api.mu.Unlock()
	if !api.drawFlags[0] {
		t.Fatal("draw offer not accepted")
	}
}

func TestGameEndsWhileThinking(t *testing.T) {
	api := newFakeAPI()
	release := make(chan struct{})
	model := &fakeLLM{move: func(string) (string, error) {
		<-release
		return `{"move": "e2e4"}`, nil
	}}
	wait := startSession(t, api, model, botConfig(false), italian)
	api.game <- gameFull(true, "")
	waitFor(t, "llm request", func() bool { return len(model.calls()) == 1 })
	api.game <- gameState("", "aborted")
	wait()
	close(release)
	time.Sleep(20 * time.Millisecond)
	if m, _ := api.snapshot(); len(m) != 0 {
		t.Fatalf("a move was sent after the end of the game: %v", m)
	}
}

func TestMoveIsResentAfterLichessError(t *testing.T) {
	api := newFakeAPI()
	api.moveErr = errors.New("network down")
	model := &fakeLLM{move: fixedMove(`{"move": "e2e4"}`)}
	startSession(t, api, model, botConfig(false), config.Openings{})
	api.game <- gameFull(true, "")
	waitFor(t, "first attempt", func() bool { return len(model.calls()) >= 1 })
	api.mu.Lock()
	api.moveErr = nil
	api.mu.Unlock()
	waitFor(t, "move after retry", func() bool { m, _ := api.snapshot(); return len(m) == 1 })
}

func TestThinkBudget(t *testing.T) {
	max := 60 * time.Second
	tests := []struct {
		snap Snapshot
		want time.Duration
	}{
		{Snapshot{}, max}, // correspondence
		{Snapshot{HasClock: true, MyTime: 5 * time.Minute, Increment: 3 * time.Second}, 12*time.Second + 2250*time.Millisecond - 300*time.Millisecond},
		{Snapshot{HasClock: true, MyTime: 2 * time.Second}, 500 * time.Millisecond},
		{Snapshot{HasClock: true, MyTime: time.Hour}, max},
		{Snapshot{HasClock: true, MyTime: time.Hour, Pos: mustPosition(t, "e2e4")}, firstMoveBudget},
		{Snapshot{HasClock: true, MyTime: time.Hour, Pos: mustPosition(t, "e2e4 e7e5")}, max},
	}
	for _, tt := range tests {
		if got := thinkBudget(tt.snap, max); got != tt.want {
			t.Errorf("thinkBudget(%+v) = %v, want %v", tt.snap, got, tt.want)
		}
	}
}

func mustPosition(t *testing.T, moves string) *board.Position {
	t.Helper()
	p, err := board.FromMoves(board.StartPos, board.SplitMoves(moves))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIsFinal(t *testing.T) {
	if isFinal(gameState("e2e4", "started")) || !isFinal(gameState("e2e4", "resign")) {
		t.Error("gameState")
	}
	full := gameFull(true, "")
	if isFinal(full) {
		t.Error("gameFull started")
	}
	full.Full.State.Status = "mate"
	if !isFinal(full) || isFinal(chatLine("bob", "player", "hi")) {
		t.Error("gameFull mate / chat")
	}
}

func TestDescribeResult(t *testing.T) {
	tests := []struct {
		st   lichess.GameState
		want string
	}{
		{lichess.GameState{Status: "mate", Winner: "white"}, "you won (checkmate)"},
		{lichess.GameState{Status: "resign", Winner: "black"}, "you lost (resignation)"},
		{lichess.GameState{Status: "stalemate"}, "draw (stalemate)"},
		{lichess.GameState{Status: "variantEnd"}, "draw (variantEnd)"},
	}
	for _, tt := range tests {
		if got := describeResult(tt.st, "white"); got != tt.want {
			t.Errorf("got %q want %q", got, tt.want)
		}
	}
}

func TestLLMDownAtStartAbortsTheGame(t *testing.T) {
	api := newFakeAPI()
	model := &fakeLLM{down: true}
	wait := startSession(t, api, model, botConfig(false), italian)
	api.game <- gameFull(true, "")
	waitFor(t, "abort", func() bool { api.mu.Lock(); defer api.mu.Unlock(); return api.aborted })
	api.game <- gameState("", "aborted")
	wait()
	moves, chats := api.snapshot()
	if len(moves) != 0 || len(chats) != 1 || chats[0] != msgAbortLLMDown {
		t.Fatalf("moves = %v, chats = %q", moves, chats)
	}
}

func TestLLMDownMidGameAnnouncesThenResigns(t *testing.T) {
	api := newFakeAPI()
	model := &fakeLLM{down: true}
	startSession(t, api, model, botConfig(false), config.Openings{})

	played := board.SplitMoves("e2e4 e7e5 g1f3 b8c6") // resumed game, white to move
	api.game <- gameFull(true, strings.Join(played, " "))
	for i := 1; i < maxLLMDownMoves; i++ {
		waitFor(t, "fallback move", func() bool { m, _ := api.snapshot(); return len(m) == i })
		moves, _ := api.snapshot()
		played = append(played, moves[i-1])
		pos := mustPosition(t, strings.Join(played, " "))
		played = append(played, pos.LegalMoves()[0]) // opponent's answer
		api.game <- gameState(strings.Join(played, " "), "started")
	}
	waitFor(t, "resignation", func() bool { api.mu.Lock(); defer api.mu.Unlock(); return api.resigned })

	moves, chats := api.snapshot()
	if len(moves) != maxLLMDownMoves-1 {
		t.Fatalf("moves = %v", moves)
	}
	if strings.Join(chats, "|") != msgAutopilot+"|"+msgResignLLMDown {
		t.Fatalf("chats = %q", chats)
	}
}

func TestSlowLLMIsAnnouncedAndToldAboutAutomaticMoves(t *testing.T) {
	api := newFakeAPI()
	model := &fakeLLM{slow: true}
	cfg := botConfig(false)
	cfg.MaxThinkSeconds = 1
	startSession(t, api, model, cfg, italian)

	api.game <- gameFull(true, "e2e4 e7e5") // book move: Nf3
	waitFor(t, "fallback move", func() bool { m, _ := api.snapshot(); return len(m) == 1 })
	moves, chats := api.snapshot()
	if moves[0] != "g1f3" || len(chats) != 1 || chats[0] != msgTooSlow {
		t.Fatalf("moves = %v, chats = %q", moves, chats)
	}

	model.mu.Lock()
	model.slow = false
	model.move = fixedMove(`{"move": "f1c4"}`)
	model.mu.Unlock()
	api.game <- gameState("e2e4 e7e5 g1f3 b8c6", "started")
	waitFor(t, "second move", func() bool { m, _ := api.snapshot(); return len(m) == 2 })
	calls := model.calls()
	if p := calls[len(calls)-1]; !strings.Contains(p, "your move Nf3 was played automatically by the program because you did not answer in time") {
		t.Fatalf("move prompt does not mention the automatic move:\n%s", p)
	}
	if _, chats := api.snapshot(); len(chats) != 1 {
		t.Fatalf("slowness must be announced only once: %q", chats)
	}
}
