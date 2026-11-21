package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/littlejo/llm-chess-bot/internal/board"
	"github.com/littlejo/llm-chess-bot/internal/llm"
)

// Snapshot is a view of a game, built by the session and handed to the
// Brain. It owns its Position, so it can be used from another goroutine.
type Snapshot struct {
	Color              board.Color
	Opponent           Player
	Rated              bool
	TimeControl        string // e.g. "5+3" or "correspondence"
	HasClock           bool
	MyTime             time.Duration
	OpponentTime       time.Duration
	Increment          time.Duration // our increment
	Pos                *board.Position
	Plan               Plan
	Book               board.BookStatus
	OpponentOffersDraw bool
	Transcript         []Entry
}

// Player describes the opponent.
type Player struct {
	Name   string
	Title  string
	Rating int
}

// Plan is the opening plan of a game.
type Plan struct {
	Name         string
	Line         board.Line
	Instructions string
}

// Entry is a line of the game transcript kept as conversation memory.
type Entry struct {
	Ply     int
	Speaker string // "opponent", "you" or "note"
	Text    string
}

// Decision is the move chosen by the LLM for a position. Move is always
// legal; it is empty when the LLM resigns or could not be reached.
type Decision struct {
	Move      string // UCI
	Chat      string
	OfferDraw bool
	Resign    bool
	// LLMDown is true when there is no move because the LLM could not be
	// reached for longer than the Brain's MaxOutage.
	LLMDown bool
}

// ChatReply is the answer to opponent messages.
type ChatReply struct {
	Intent string // "conversation" or "game"
	Reply  string
}

// Brain turns game snapshots into LLM requests and validates the answers.
type Brain struct {
	LLM         llm.Provider
	Persona     string
	MoveRetries int
	Log         *slog.Logger
	// RetryPause is the pause before asking again after a failed request
	// (default 5 s). MaxOutage is how long a move request keeps retrying an
	// unreachable LLM before giving up (default llmCooldown).
	RetryPause time.Duration
	MaxOutage  time.Duration

	// Unix nanoseconds of the last LLM failure and success, shared by all
	// games to know whether the LLM is currently usable.
	lastFailure atomic.Int64
	lastSuccess atomic.Int64
}

// llmCooldown is how long the LLM is considered down after a failure that no
// success followed.
const llmCooldown = 2 * time.Minute

// complete calls the LLM and records whether it answered. Canceled or timed
// out requests do not count: a slow answer is not an outage.
func (b *Brain) complete(ctx context.Context, msgs []llm.Message) (string, error) {
	raw, err := b.LLM.Complete(ctx, msgs)
	switch {
	case err == nil:
		b.lastSuccess.Store(time.Now().UnixNano())
	case ctx.Err() == nil:
		b.lastFailure.Store(time.Now().UnixNano())
	}
	return raw, err
}

// Available reports whether the LLM can be relied on: it is not, for
// llmCooldown, after a failure that was not followed by a success.
func (b *Brain) Available() bool {
	failure := b.lastFailure.Load()
	return failure == 0 || failure < b.lastSuccess.Load() || time.Since(time.Unix(0, failure)) > llmCooldown
}

// Ping checks that the LLM answers at all.
func (b *Brain) Ping(ctx context.Context) error {
	_, err := b.complete(ctx, []llm.Message{{Role: "user", Content: "Reply with the single word: pong"}})
	return err
}

// DecideMove asks the LLM for a move and waits for it as long as it takes:
// the program never chooses a move by itself. Illegal or unreadable answers
// are sent back to the LLM with the error, and failed requests are retried
// after a pause. It returns without a move when ctx is canceled (position
// changed, game over) or when the LLM stays unreachable for MaxOutage.
func (b *Brain) DecideMove(ctx context.Context, s Snapshot) Decision {
	fresh := func() []llm.Message {
		return []llm.Message{
			{Role: "system", Content: systemPrompt(b.Persona, s)},
			{Role: "user", Content: movePrompt(s)},
		}
	}
	msgs := fresh()
	var downSince time.Time
	for unusable := 0; ctx.Err() == nil; {
		raw, err := b.complete(ctx, msgs)
		if ctx.Err() != nil {
			b.Log.Debug("llm move request canceled (position changed or game over)")
			break
		}
		if err != nil {
			if downSince.IsZero() {
				downSince = time.Now()
			}
			if time.Since(downSince) >= b.maxOutage() {
				b.Log.Error("llm unreachable for too long, giving up", "err", err, "since", downSince)
				return Decision{LLMDown: true}
			}
			b.Log.Warn("llm move request failed, retrying", "err", err)
			pause(ctx, b.retryPause())
			continue
		}
		downSince = time.Time{}
		d, err := parseDecision(raw, s.Pos)
		if err == nil {
			return d
		}
		unusable++
		b.Log.Warn("llm proposed an unusable move, asking again", "err", err, "answer", truncate(raw, 300), "attempt", unusable)
		if unusable%(b.MoveRetries+1) == 0 {
			msgs = fresh() // start over instead of growing the conversation forever
			continue
		}
		msgs = append(msgs,
			llm.Message{Role: "assistant", Content: raw},
			llm.Message{Role: "user", Content: retryPrompt(err.Error(), s)})
	}
	return Decision{}
}

func (b *Brain) retryPause() time.Duration {
	if b.RetryPause > 0 {
		return b.RetryPause
	}
	return 5 * time.Second
}

func (b *Brain) maxOutage() time.Duration {
	if b.MaxOutage > 0 {
		return b.MaxOutage
	}
	return llmCooldown
}

// pause waits for d or until ctx is canceled.
func pause(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

var uciToken = regexp.MustCompile(`\b[a-h][1-8][a-h][1-8][qrbn]?\b`)

// parseDecision reads the LLM answer. It accepts the requested JSON object
// (possibly inside a code block); if no usable JSON is found, the first legal
// UCI move found in the text is used.
func parseDecision(raw string, pos *board.Position) (Decision, error) {
	var out struct {
		Move      string `json:"move"`
		Chat      string `json:"chat"`
		OfferDraw bool   `json:"offer_draw"`
		Resign    bool   `json:"resign"`
	}
	if err := decodeJSON(raw, &out); err == nil && (out.Move != "" || out.Resign) {
		d := Decision{Chat: strings.TrimSpace(out.Chat), OfferDraw: out.OfferDraw, Resign: out.Resign}
		if out.Move == "" { // resigning without a move
			return d, nil
		}
		uci, err := pos.ParseMove(out.Move)
		if err != nil {
			return Decision{}, fmt.Errorf("%q is not a legal move in this position", out.Move)
		}
		d.Move = uci
		return d, nil
	}
	for _, tok := range uciToken.FindAllString(strings.ToLower(raw), -1) {
		if uci, err := pos.ParseMove(tok); err == nil {
			return Decision{Move: uci}, nil
		}
	}
	return Decision{}, errors.New("no JSON object with a legal \"move\" found")
}

// ReplyChat answers opponent messages. It can only produce text.
func (b *Brain) ReplyChat(ctx context.Context, s Snapshot, messages []string) (ChatReply, error) {
	raw, err := b.complete(ctx, []llm.Message{
		{Role: "system", Content: systemPrompt(b.Persona, s)},
		{Role: "user", Content: chatPrompt(s, messages)},
	})
	if err != nil {
		return ChatReply{}, err
	}
	var out ChatReply
	var data struct {
		Intent string `json:"intent"`
		Reply  string `json:"reply"`
	}
	if err := decodeJSON(raw, &data); err != nil {
		// A model that forgot the JSON format still wrote a usable reply.
		out = ChatReply{Intent: "conversation", Reply: strings.TrimSpace(raw)}
	} else {
		out = ChatReply{Intent: data.Intent, Reply: strings.TrimSpace(data.Reply)}
	}
	if out.Intent != "game" {
		out.Intent = "conversation"
	}
	return out, nil
}

// PlanOpening asks the LLM to confirm or choose the opening and to write a
// greeting. When the configured plan already has a line, it is kept as is.
func (b *Brain) PlanOpening(ctx context.Context, s Snapshot) (Plan, string, error) {
	raw, err := b.complete(ctx, []llm.Message{
		{Role: "system", Content: systemPrompt(b.Persona, s)},
		{Role: "user", Content: openingPrompt(s)},
	})
	if err != nil {
		return s.Plan, "", err
	}
	var out struct {
		Name     string `json:"name"`
		Moves    string `json:"moves"`
		Greeting string `json:"greeting"`
	}
	if err := decodeJSON(raw, &out); err != nil {
		return s.Plan, "", fmt.Errorf("opening answer: %w", err)
	}
	plan := s.Plan
	if plan.Name == "" {
		plan.Name = out.Name
	}
	if plan.Line.Empty() && out.Moves != "" {
		line, err := board.ParseLine(plan.Name, out.Moves)
		if err != nil {
			b.Log.Warn("llm opening line partially invalid, keeping the legal prefix", "err", err, "kept", line.String())
		}
		plan.Line = line
	}
	return plan, strings.TrimSpace(out.Greeting), nil
}

// Farewell asks the LLM for an end of game message.
func (b *Brain) Farewell(ctx context.Context, s Snapshot, result string) (string, error) {
	raw, err := b.complete(ctx, []llm.Message{
		{Role: "system", Content: systemPrompt(b.Persona, s)},
		{Role: "user", Content: farewellPrompt(s, result)},
	})
	if err != nil {
		return "", err
	}
	var out struct {
		Reply string `json:"reply"`
	}
	if err := decodeJSON(raw, &out); err != nil {
		return strings.TrimSpace(raw), nil
	}
	return strings.TrimSpace(out.Reply), nil
}

// decodeJSON extracts the outermost JSON object of an LLM answer, ignoring
// code fences or text around it.
func decodeJSON(raw string, v any) error {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return errors.New("no JSON object in answer")
	}
	return json.Unmarshal([]byte(raw[start:end+1]), v)
}

// splitChat cuts a message into Lichess sized chunks on word boundaries,
// keeping at most maxParts chunks.
func splitChat(text string, maxParts int) []string {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	var parts []string
	for len(runes) > 0 && len(parts) < maxParts {
		if len(runes) <= MaxChatLength {
			parts = append(parts, string(runes))
			break
		}
		cut := MaxChatLength
		for i := MaxChatLength; i > 0; i-- { // last space that keeps the chunk short enough
			if runes[i] == ' ' {
				cut = i
				break
			}
		}
		parts = append(parts, strings.TrimSpace(string(runes[:cut])))
		runes = []rune(strings.TrimSpace(string(runes[cut:])))
	}
	return parts
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
