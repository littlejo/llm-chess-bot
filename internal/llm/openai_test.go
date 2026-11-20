package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestOpenAICompatibleComplete(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" || r.Header.Get("X-Extra") != "1" {
			t.Errorf("headers = %v", r.Header)
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "gpt-test" || len(req.Messages) != 2 || req.Messages[1].Content != "ping" {
			t.Errorf("request = %+v", req)
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`)
	}))
	defer srv.Close()

	p, err := New("litellm", ProviderConfig{
		NPM:     "@ai-sdk/openai-compatible",
		Options: ProviderOptions{BaseURL: srv.URL + "/v1/", APIKey: "sk-test", Headers: map[string]string{"X-Extra": "1"}},
	}, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Complete(context.Background(), []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "ping"}})
	if err != nil || got != "pong" {
		t.Fatalf("got %q, %v", got, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d (5xx should be retried once)", calls.Load())
	}
}

func TestOpenAICompatibleClientErrorNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := NewOpenAICompatible(srv.URL, "", "m", nil)
	if _, err := p.Complete(context.Background(), nil); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestNewRejectsUnknownProvider(t *testing.T) {
	if _, err := New("x", ProviderConfig{NPM: "@ai-sdk/anthropic", Options: ProviderOptions{BaseURL: "http://x"}}, "m"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := New("x", ProviderConfig{}, "m"); err == nil {
		t.Fatal("expected error for missing baseURL")
	}
}
