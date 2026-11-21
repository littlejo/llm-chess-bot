// Package lichess is a minimal client for the parts of the Lichess Bot API the
// bot needs: the event and game streams (ndjson), challenges, moves and chat.
package lichess

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultBaseURL is the production Lichess server.
const DefaultBaseURL = "https://lichess.org"

// ErrIdleTimeout is returned when a stream stays silent for too long (no data
// and no keep-alive newline): the connection is considered dead.
var ErrIdleTimeout = errors.New("stream idle timeout")

// APIError is a non-2xx answer from Lichess.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("lichess: HTTP %d: %s", e.Status, e.Body)
}

// IsRateLimited reports whether err is an HTTP 429 answer.
func IsRateLimited(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests
}

// IsNotFound reports whether err is an HTTP 404 answer.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// Client talks to the Lichess API with a bot token.
type Client struct {
	BaseURL   string
	Token     string
	UserAgent string
	HTTP      *http.Client
	Log       *slog.Logger

	// RateLimitWait is how long to wait after a 429 (Lichess asks for one
	// full minute). IdleTimeout closes streams that stay silent.
	RateLimitWait time.Duration
	IdleTimeout   time.Duration
	// Retries is the number of extra attempts for transient errors.
	Retries int

	// Lichess asks clients to make one request at a time.
	mu sync.Mutex
}

// NewClient returns a client with sensible defaults.
func NewClient(baseURL, token, userAgent string, log *slog.Logger) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL:       strings.TrimRight(baseURL, "/"),
		Token:         token,
		UserAgent:     userAgent,
		HTTP:          &http.Client{}, // no global timeout: streams are long lived
		Log:           log,
		RateLimitWait: time.Minute,
		IdleTimeout:   30 * time.Second,
		Retries:       2,
	}
}

// Account returns the authenticated account.
func (c *Client) Account(ctx context.Context) (Account, error) {
	var a Account
	err := c.do(ctx, http.MethodGet, "/api/account", nil, &a)
	return a, err
}

// AcceptChallenge accepts an incoming challenge.
func (c *Client) AcceptChallenge(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/challenge/"+url.PathEscape(id)+"/accept", nil, nil)
}

// DeclineChallenge declines an incoming challenge with a Lichess reason key
// (generic, later, tooFast, tooSlow, timeControl, rated, casual, standard,
// variant, noBot, onlyBot).
func (c *Client) DeclineChallenge(ctx context.Context, id, reason string) error {
	form := url.Values{}
	if reason != "" {
		form.Set("reason", reason)
	}
	return c.do(ctx, http.MethodPost, "/api/challenge/"+url.PathEscape(id)+"/decline", form, nil)
}

// Move plays a UCI move. offeringDraw offers a draw, or accepts the
// opponent's pending offer.
func (c *Client) Move(ctx context.Context, gameID, uci string, offeringDraw bool) error {
	path := "/api/bot/game/" + url.PathEscape(gameID) + "/move/" + url.PathEscape(uci)
	if offeringDraw {
		path += "?offeringDraw=true"
	}
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// Chat writes a message in the game chat ("player" or "spectator" room).
func (c *Client) Chat(ctx context.Context, gameID, room, text string) error {
	form := url.Values{"room": {room}, "text": {text}}
	return c.do(ctx, http.MethodPost, "/api/bot/game/"+url.PathEscape(gameID)+"/chat", form, nil)
}

// Abort aborts the game (only possible before both players have moved).
func (c *Client) Abort(ctx context.Context, gameID string) error {
	return c.do(ctx, http.MethodPost, "/api/bot/game/"+url.PathEscape(gameID)+"/abort", nil, nil)
}

// Resign resigns the game.
func (c *Client) Resign(ctx context.Context, gameID string) error {
	return c.do(ctx, http.MethodPost, "/api/bot/game/"+url.PathEscape(gameID)+"/resign", nil, nil)
}

// StreamEvents reads the global event stream until it ends, ctx is canceled
// or the connection goes idle. fn is called for each event.
func (c *Client) StreamEvents(ctx context.Context, fn func(Event)) error {
	return c.stream(ctx, "/api/stream/event", func(line []byte) {
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			c.Log.Warn("invalid event line", "err", err, "line", string(line))
			return
		}
		fn(ev)
	})
}

// StreamGame reads a bot game stream. It returns nil when Lichess closes the
// stream (usually because the game is over).
func (c *Client) StreamGame(ctx context.Context, gameID string, fn func(GameEvent)) error {
	return c.stream(ctx, "/api/bot/game/stream/"+url.PathEscape(gameID), func(line []byte) {
		ev, err := decodeGameEvent(line)
		if err != nil {
			c.Log.Warn("invalid game line", "game", gameID, "err", err, "line", string(line))
			return
		}
		fn(ev)
	})
}

func (c *Client) newRequest(ctx context.Context, method, path string, form url.Values) (*http.Request, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", c.UserAgent)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return req, nil
}

// do performs a short request, retrying transient failures (network errors,
// 5xx) with a small backoff and waiting RateLimitWait after a 429.
func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			wait := time.Duration(attempt) * time.Second
			if IsRateLimited(lastErr) {
				wait = c.RateLimitWait
			}
			c.Log.Warn("retrying lichess request", "path", path, "attempt", attempt, "wait", wait, "err", lastErr)
			if err := sleep(ctx, wait); err != nil {
				return err
			}
		}
		lastErr = c.doOnce(ctx, method, path, form, out)
		if lastErr == nil || !retryable(lastErr) || ctx.Err() != nil {
			return lastErr
		}
	}
	return lastErr
}

func (c *Client) doOnce(ctx context.Context, method, path string, form url.Values, out any) error {
	req, err := c.newRequest(ctx, method, path, form)
	if err != nil {
		return err
	}
	c.mu.Lock()
	resp, err := c.HTTP.Do(req)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// stream opens an ndjson stream and calls onLine for every non-empty line.
// Keep-alive newlines only reset the idle watchdog.
func (c *Client) stream(ctx context.Context, path string, onLine func([]byte)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}

	var idle atomic.Bool
	watchdog := time.AfterFunc(c.IdleTimeout, func() { idle.Store(true); cancel() })
	defer watchdog.Stop()
	r := bufio.NewReader(&activityReader{r: resp.Body, onRead: func() { watchdog.Reset(c.IdleTimeout) }})

	for {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			onLine(line)
		}
		if err != nil {
			switch {
			case idle.Load():
				return ErrIdleTimeout
			case errors.Is(err, io.EOF):
				return nil
			default:
				return err
			}
		}
	}
}

// activityReader calls onRead every time data is received.
type activityReader struct {
	r      io.Reader
	onRead func()
}

func (a *activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.onRead()
	}
	return n, err
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Backoff computes reconnection delays: exponential, capped, reset after a
// connection that stayed up long enough.
type Backoff struct {
	Min, Max time.Duration
	current  time.Duration
}

// Next returns the next delay to wait before reconnecting.
func (b *Backoff) Next() time.Duration {
	if b.current == 0 {
		b.current = b.Min
	} else {
		b.current *= 2
	}
	if b.current > b.Max {
		b.current = b.Max
	}
	return b.current
}

// Reset restarts the sequence from Min.
func (b *Backoff) Reset() { b.current = 0 }

// Reconnect runs connect until ctx is canceled or connect returns a nil error
// while stopOnNil is true. Errors are logged and followed by a backoff (one
// full minute after a 429).
func Reconnect(ctx context.Context, log *slog.Logger, name string, stopOnNil bool, connect func(context.Context) error) error {
	b := Backoff{Min: time.Second, Max: time.Minute}
	for {
		started := time.Now()
		err := connect(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && stopOnNil {
			return nil
		}
		if time.Since(started) > time.Minute {
			b.Reset()
		}
		wait := b.Next()
		if IsRateLimited(err) {
			wait = time.Minute
		}
		if err != nil && !IsRateLimited(err) {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Status/100 == 4 {
				return err // 4xx other than 429: reconnecting will not help
			}
		}
		log.Warn("stream disconnected, reconnecting", "stream", name, "err", err, "wait", wait.String())
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
}
