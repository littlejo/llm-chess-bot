package lichess

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(url string) *Client {
	c := NewClient(url, "tok", "test-agent", slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.RateLimitWait = 10 * time.Millisecond
	c.IdleTimeout = 200 * time.Millisecond
	return c
}

func TestStreamGameDecodesEventsAndSkipsKeepAlive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bot/game/stream/abc" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("User-Agent") != "test-agent" {
			t.Errorf("missing headers: %v", r.Header)
		}
		io.WriteString(w, `{"type":"gameFull","id":"abc","white":{"id":"bot","name":"Bot"},"black":{"id":"bob","name":"Bob","rating":1500},"initialFen":"startpos","clock":{"initial":300000,"increment":2000},"state":{"type":"gameState","moves":"e2e4","wtime":300000,"btime":300000,"winc":2000,"binc":2000,"status":"started"}}`+"\n")
		io.WriteString(w, "\n")
		io.WriteString(w, `{"type":"chatLine","room":"player","username":"bob","text":"hi"}`+"\n")
		io.WriteString(w, `{"type":"gameState","moves":"e2e4 e7e5","wtime":1,"btime":2,"winc":0,"binc":0,"status":"started","bdraw":true}`+"\n")
		io.WriteString(w, `{"type":"opponentGone","gone":true,"claimWinInSeconds":10}`+"\n")
	}))
	defer srv.Close()

	var got []GameEvent
	err := testClient(srv.URL).StreamGame(context.Background(), "abc", func(ev GameEvent) { got = append(got, ev) })
	if err != nil {
		t.Fatalf("StreamGame: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d events", len(got))
	}
	if got[0].Full == nil || got[0].Full.Black.Rating != 1500 || got[0].Full.State.Moves != "e2e4" || got[0].Full.Clock.Increment != 2000 {
		t.Errorf("gameFull = %+v", got[0].Full)
	}
	if got[1].Chat == nil || got[1].Chat.Text != "hi" {
		t.Errorf("chat = %+v", got[1].Chat)
	}
	if got[2].State == nil || !got[2].State.BDraw || got[2].State.Moves != "e2e4 e7e5" {
		t.Errorf("state = %+v", got[2].State)
	}
	if got[3].OpponentGone == nil || got[3].OpponentGone.ClaimWinInSeconds != 10 {
		t.Errorf("gone = %+v", got[3].OpponentGone)
	}
}

func TestStreamIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"type":"challengeCanceled","challenge":{"id":"x"}}`+"\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // then stay silent
	}))
	defer srv.Close()

	var n int
	start := time.Now()
	err := testClient(srv.URL).StreamEvents(context.Background(), func(ev Event) { n++ })
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout", err)
	}
	if n != 1 {
		t.Fatalf("events = %d", n)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("watchdog too slow")
	}
}

func TestStreamKeepAliveResetsWatchdog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 5; i++ { // 5 × 100ms > 200ms idle timeout
			io.WriteString(w, "\n")
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()
	if err := testClient(srv.URL).StreamEvents(context.Background(), func(Event) {}); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestDoRetriesAfterRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.FormValue("room") != "player" || r.FormValue("text") != "gg" {
			t.Errorf("form = %v", r.Form)
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	if err := testClient(srv.URL).Chat(context.Background(), "g1", "player", "gg"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestDoDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/bot/game/g1/move/e2e4" || r.URL.Query().Get("offeringDraw") != "true" {
			t.Errorf("url = %s", r.URL)
		}
		http.Error(w, `{"error":"Not your turn"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	err := testClient(srv.URL).Move(context.Background(), "g1", "e2e4", true)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestReconnect(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var n int
	err := Reconnect(context.Background(), log, "t", true, func(context.Context) error {
		n++
		if n < 2 {
			return errors.New("boom")
		}
		return nil
	})
	if err != nil || n != 2 {
		t.Fatalf("err=%v n=%d", err, n)
	}

	// A 4xx other than 429 is permanent.
	err = Reconnect(context.Background(), log, "t", true, func(context.Context) error {
		return &APIError{Status: 401}
	})
	if err == nil {
		t.Fatal("expected permanent error")
	}
}

func TestBackoff(t *testing.T) {
	b := Backoff{Min: time.Second, Max: 4 * time.Second}
	for _, want := range []time.Duration{1, 2, 4, 4} {
		if got := b.Next(); got != want*time.Second {
			t.Fatalf("got %v want %v", got, want*time.Second)
		}
	}
	b.Reset()
	if b.Next() != time.Second {
		t.Fatal("reset failed")
	}
}

func TestFinished(t *testing.T) {
	for s, want := range map[string]bool{"started": false, "created": false, "mate": true, "resign": true, "draw": true, "aborted": true, "outoftime": true} {
		if Finished(s) != want {
			t.Errorf("Finished(%q) != %v", s, want)
		}
	}
}
