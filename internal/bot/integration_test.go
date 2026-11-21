package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/littlejo/llm-chess-bot/internal/board"
	"github.com/littlejo/llm-chess-bot/internal/config"
	"github.com/littlejo/llm-chess-bot/internal/lichess"
	"github.com/littlejo/llm-chess-bot/internal/llm"
)

// fakeServer imitates both Lichess and a LiteLLM proxy over real HTTP. The
// bot plays white; the fake opponent answers with its first legal move,
// chats once, and resigns after the bot's third move.
type fakeServer struct {
	t        *testing.T
	mu       sync.Mutex
	moves    []string
	botMoves int
	chats    []string
	accepted chan struct{}
	updates  chan string
}

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, line string) {
		io.WriteString(w, line+"\n")
		w.(http.Flusher).Flush()
	}
	mux.HandleFunc("GET /api/account", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"llmbot","username":"LLMBot","title":"BOT"}`)
	})
	mux.HandleFunc("GET /api/stream/event", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"type":"challenge","challenge":{"id":"c1","challenger":{"id":"bob","name":"Bob","rating":1450},"variant":{"key":"standard"},"rated":false,"speed":"rapid","timeControl":{"type":"clock","limit":600,"increment":5},"color":"random","direction":"in"}}`)
		select {
		case <-f.accepted:
			write(w, `{"type":"gameStart","game":{"gameId":"g1","color":"white"}}`)
		case <-r.Context().Done():
			return
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
				write(w, "") // keep-alive
			}
		}
	})
	mux.HandleFunc("POST /api/challenge/c1/accept", func(w http.ResponseWriter, r *http.Request) {
		close(f.accepted)
	})
	mux.HandleFunc("GET /api/bot/game/stream/g1", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"type":"gameFull","id":"g1","rated":false,"clock":{"initial":600000,"increment":5000},"white":{"id":"llmbot","name":"LLMBot","title":"BOT"},"black":{"id":"bob","name":"Bob","rating":1450},"initialFen":"startpos","state":{"type":"gameState","moves":"","wtime":600000,"btime":600000,"winc":5000,"binc":5000,"status":"started"}}`)
		for {
			select {
			case <-r.Context().Done():
				return
			case line := <-f.updates:
				write(w, line)
				if !strings.Contains(line, `"started"`) && strings.Contains(line, "gameState") {
					return // game over: Lichess closes the stream
				}
			}
		}
	})
	mux.HandleFunc("POST /api/bot/game/g1/move/{uci}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.moves = append(f.moves, r.PathValue("uci"))
		f.botMoves++
		pos, err := board.FromMoves(board.StartPos, f.moves)
		if err != nil {
			f.t.Errorf("bot sent an illegal move: %v", err)
			http.Error(w, "illegal", http.StatusBadRequest)
			return
		}
		f.updates <- state(f.moves, "started", "")
		if f.botMoves == 3 {
			f.updates <- state(f.moves, "resign", "white")
			return
		}
		f.moves = append(f.moves, pos.LegalMoves()[0])
		f.updates <- state(f.moves, "started", "")
		if f.botMoves == 1 {
			f.updates <- `{"type":"chatLine","room":"player","username":"Bob","text":"Why that opening?"}`
		}
	})
	mux.HandleFunc("POST /api/bot/game/g1/chat", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.chats = append(f.chats, r.FormValue("text"))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []llm.Message }
		json.NewDecoder(r.Body).Decode(&req)
		prompt := req.Messages[len(req.Messages)-1].Content
		answer := `{"reply": "Good game, Bob!"}`
		switch {
		case strings.Contains(prompt, "The game is starting"):
			answer = `{"name": "Ruy Lopez", "moves": "1. e4 e5 2. Nf3 Nc6 3. Bb5", "greeting": "Hello Bob! I will play the Ruy Lopez."}`
		case strings.Contains(prompt, "It is your turn"):
			uci := regexp.MustCompile(`- UCI: (\S+)`).FindStringSubmatch(prompt)[1]
			answer = fmt.Sprintf(`{"move": %q}`, uci)
		case strings.Contains(prompt, "New messages"):
			answer = `{"intent": "game", "reply": "It leads to rich positions!"}`
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": answer}}}})
	})
	return mux
}

func state(moves []string, status, winner string) string {
	return fmt.Sprintf(`{"type":"gameState","moves":%q,"wtime":590000,"btime":590000,"winc":5000,"binc":5000,"status":%q,"winner":%q}`,
		strings.Join(moves, " "), status, winner)
}

func TestIntegrationFullGame(t *testing.T) {
	f := &fakeServer{t: t, accepted: make(chan struct{}), updates: make(chan string, 16)}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	cfg := config.Default()
	cfg.Challenges.MinInitialSeconds = 60
	provider, err := llm.New("litellm", llm.ProviderConfig{Options: llm.ProviderOptions{BaseURL: srv.URL + "/v1"}}, "test-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{
		API:    lichess.NewClient(srv.URL, "tok", "test", discardLogger()),
		Brain:  &Brain{LLM: provider, Persona: "p", MoveRetries: 1, Log: discardLogger()},
		Config: cfg,
		Log:    discardLogger(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error)
	go func() { done <- b.Run(ctx) }()

	waitForLong := func(what string, cond func() bool) {
		t.Helper()
		for !cond() {
			if ctx.Err() != nil {
				t.Fatalf("timeout waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitForLong("farewell", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.chats) > 0 && f.chats[len(f.chats)-1] == "Good game, Bob!"
	})
	cancel()
	<-done

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.botMoves != 3 {
		t.Errorf("bot moves = %d", f.botMoves)
	}
	want := []string{"Hello Bob! I will play the Ruy Lopez.", "It leads to rich positions!", "Good game, Bob!"}
	if strings.Join(f.chats, "|") != strings.Join(want, "|") {
		t.Errorf("chats = %q", f.chats)
	}
}
