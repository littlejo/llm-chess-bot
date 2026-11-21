package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/littlejo/llm-chess-bot/internal/board"
	"github.com/littlejo/llm-chess-bot/internal/config"
	"github.com/littlejo/llm-chess-bot/internal/lichess"
)

// API is the part of the Lichess client used by the bot.
type API interface {
	Account(ctx context.Context) (lichess.Account, error)
	StreamEvents(ctx context.Context, fn func(lichess.Event)) error
	AcceptChallenge(ctx context.Context, id string) error
	DeclineChallenge(ctx context.Context, id, reason string) error
	StreamGame(ctx context.Context, gameID string, fn func(lichess.GameEvent)) error
	Move(ctx context.Context, gameID, uci string, offeringDraw bool) error
	Chat(ctx context.Context, gameID, room, text string) error
	Abort(ctx context.Context, gameID string) error
	Resign(ctx context.Context, gameID string) error
}

const (
	maxPostFailures = 3 // attempts to send a move for the same ply
	// Lichess aborts games whose first moves come too late: opening planning
	// must not delay the first move for long.
	planTimeout     = 10 * time.Second
	chatTimeout     = 45 * time.Second
	farewellTimeout = 20 * time.Second
)

var errStreamClosed = errors.New("game stream closed before the end of the game")

type moveResult struct {
	seq      int
	ply      int
	decision Decision
}

type chatResult struct {
	reply ChatReply
	err   error
}

// Session plays one game. Its Run loop is the only goroutine touching the
// game state; LLM requests run in separate goroutines and send their results
// back through channels.
type Session struct {
	id       string
	me       string // our Lichess id (lowercase)
	api      API
	brain    *Brain
	cfg      config.Bot
	openings config.Openings
	log      *slog.Logger

	// retryDelay is the pause before sending a move again after an error.
	retryDelay time.Duration

	events chan lichess.GameEvent
	moves  chan moveResult
	chats  chan chatResult
	retry  chan struct{}
	slow   chan int    // thinkSeq of a move request that exceeds its expected time
	ended  atomic.Bool // read by the stream goroutine

	full     *lichess.GameFull
	state    lichess.GameState
	pos      *board.Position
	color    board.Color
	opponent Player
	plan     Plan

	transcript     []Entry
	deviationNoted bool

	thinkSeq     int
	thinkStarted time.Time
	thinkingPly  int
	cancelThink  context.CancelFunc
	slowTimer    *time.Timer
	submittedPly int
	postFailures map[int]int

	chatBusy    bool
	pendingChat []string
	finished    bool

	slowAnnounced bool
}

// Messages sent when the LLM is unreachable or slow.
const (
	msgAbortLLMDown  = "Sorry, my AI brain is unreachable right now, so I'm aborting this game. Please try again later!"
	msgResignLLMDown = "My AI brain has been unreachable for a while, so I resign rather than play random moves. Sorry, and well played!"
	msgTooSlow       = "Sorry, my AI brain is slow: I always wait for its answer, even if it costs me time on the clock. Thanks for your patience!"
)

// NewSession prepares a session for a game.
func NewSession(id, me string, api API, brain *Brain, cfg config.Bot, openings config.Openings, log *slog.Logger) *Session {
	return &Session{
		id: id, me: strings.ToLower(me), api: api, brain: brain, cfg: cfg, openings: openings, log: log,
		retryDelay:   2 * time.Second,
		events:       make(chan lichess.GameEvent, 64),
		moves:        make(chan moveResult, 1),
		chats:        make(chan chatResult, 1),
		retry:        make(chan struct{}, 1),
		slow:         make(chan int, 1),
		thinkingPly:  -1,
		submittedPly: -1,
		postFailures: map[int]int{},
	}
}

// Run follows the game until it ends or ctx is canceled.
func (s *Session) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer s.stopThinking()

	streamDone := make(chan error, 1)
	go func() {
		streamDone <- lichess.Reconnect(ctx, s.log, "game", true, func(ctx context.Context) error {
			err := s.api.StreamGame(ctx, s.id, func(ev lichess.GameEvent) {
				// Mark the end here, not in the Run loop: Lichess closes the
				// stream right after the final state, possibly before the
				// loop has processed it.
				if isFinal(ev) {
					s.ended.Store(true)
				}
				select {
				case s.events <- ev:
				case <-ctx.Done():
				}
			})
			if err == nil && !s.ended.Load() {
				return errStreamClosed
			}
			return err
		})
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case err := <-streamDone:
			// Events received before the end of the stream (such as the
			// final state) are still buffered: handle them first.
			for len(s.events) > 0 && !s.finished {
				s.handleEvent(ctx, <-s.events)
			}
			if !s.finished {
				if err != nil && ctx.Err() == nil {
					s.log.Error("game stream failed, leaving the game", "err", err)
				}
				return
			}
		case ev := <-s.events:
			s.handleEvent(ctx, ev)
		case r := <-s.moves:
			s.handleMove(ctx, r)
		case r := <-s.chats:
			s.handleChat(ctx, r)
		case <-s.retry:
			s.maybeThink(ctx)
		case seq := <-s.slow:
			s.onSlow(ctx, seq)
		}
		if s.finished {
			s.finish(ctx)
			return
		}
	}
}

// isFinal reports whether ev carries a terminal game status.
func isFinal(ev lichess.GameEvent) bool {
	return (ev.State != nil && lichess.Finished(ev.State.Status)) ||
		(ev.Full != nil && lichess.Finished(ev.Full.State.Status))
}

func (s *Session) handleEvent(ctx context.Context, ev lichess.GameEvent) {
	switch {
	case ev.Full != nil:
		first := s.full == nil
		s.full = ev.Full
		s.setPlayers()
		s.applyState(ctx, ev.Full.State, first)
	case ev.State != nil:
		if s.full == nil {
			s.log.Warn("gameState before gameFull, ignored")
			return
		}
		s.applyState(ctx, *ev.State, false)
	case ev.Chat != nil:
		s.onChat(ctx, *ev.Chat)
	case ev.OpponentGone != nil:
		s.log.Info("opponent gone", "gone", ev.OpponentGone.Gone, "claim_win_in_s", ev.OpponentGone.ClaimWinInSeconds)
	}
}

func (s *Session) setPlayers() {
	white, black := s.full.White, s.full.Black
	s.color, s.opponent = board.White, toPlayer(black)
	if strings.ToLower(black.ID) == s.me {
		s.color, s.opponent = board.Black, toPlayer(white)
	}
}

func toPlayer(p lichess.GamePlayer) Player {
	name := p.Name
	if name == "" && p.AILevel > 0 {
		name = fmt.Sprintf("Stockfish level %d", p.AILevel)
	}
	return Player{Name: name, Title: p.Title, Rating: p.Rating}
}

// applyState rebuilds the position from the full move list: this is
// idempotent, so reconnections and repeated states are harmless.
func (s *Session) applyState(ctx context.Context, st lichess.GameState, first bool) {
	pos, err := board.FromMoves(s.full.InitialFEN, board.SplitMoves(st.Moves))
	if err != nil {
		s.log.Error("cannot replay the moves sent by lichess", "err", err, "moves", st.Moves)
		return
	}
	s.state, s.pos = st, pos

	if lichess.Finished(st.Status) {
		s.finished = true
		s.ended.Store(true)
		s.stopThinking()
		return
	}
	if s.cancelThink != nil && s.thinkingPly != pos.Ply() {
		s.log.Info("position changed while thinking, restarting", "ply", pos.Ply())
		s.stopThinking()
	}
	if first {
		s.log.Info("game started", "color", s.color, "opponent", s.opponent.Name, "rating", s.opponent.Rating, "ply", pos.Ply())
		s.prepare(ctx)
	}
	if book := s.book(); book.State == board.Deviated && !s.deviationNoted {
		s.deviationNoted = true
		s.log.Info("left the opening line", "ply", book.DeviationPly+1, "played", book.Played, "expected", book.Expected)
		s.addEntry("note", fmt.Sprintf("the game left the planned line (%s played instead of %s); the line is abandoned", book.Played, book.Expected))
	}
	s.maybeThink(ctx)
}

// prepare sets the opening plan and, for a new game, lets the LLM choose or
// confirm the opening and greet the opponent.
func (s *Session) prepare(ctx context.Context) {
	o := s.openings.White
	if s.color == board.Black {
		o = s.openings.Black
	}
	s.plan = Plan{Name: o.Name, Instructions: o.Instructions}
	if o.Moves != "" && s.fromStart() {
		line, err := board.ParseLine(o.Name, o.Moves)
		if err != nil {
			s.log.Warn("configured opening line is invalid, keeping its legal prefix", "err", err)
		}
		s.plan.Line = line
	}
	resumed := s.pos.Ply() > 1
	if resumed || !s.fromStart() || (!s.cfg.Greet && !o.Auto()) {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	plan, greeting, err := s.brain.PlanOpening(pctx, s.snapshot())
	if err != nil {
		s.log.Warn("opening planning failed, playing without a plan", "err", err)
		return
	}
	s.plan = plan
	s.log.Info("opening plan", "name", plan.Name, "line", plan.Line.String())
	if s.cfg.Greet && greeting != "" {
		s.say(ctx, greeting)
	}
}

func (s *Session) fromStart() bool {
	return s.full.InitialFEN == "" || s.full.InitialFEN == board.StartPos
}

func (s *Session) book() board.BookStatus {
	if !s.fromStart() {
		return board.BookStatus{State: board.NoBook}
	}
	return s.plan.Line.Follow(s.pos.Moves())
}

// maybeThink starts an LLM move request when it is our turn and no request
// is running or was already sent for this ply. The request has no deadline:
// the bot waits for the LLM, whatever the clock says.
func (s *Session) maybeThink(ctx context.Context) {
	if s.finished || s.pos == nil || s.pos.Turn() != s.color {
		return
	}
	ply := s.pos.Ply()
	if ply == s.thinkingPly || ply == s.submittedPly || s.postFailures[ply] >= maxPostFailures {
		return
	}
	s.stopThinking()
	snap := s.snapshot()
	expected := expectedThinkTime(snap, time.Duration(s.cfg.MaxThinkSeconds)*time.Second)
	tctx, cancel := context.WithCancel(ctx)
	s.thinkSeq++
	seq := s.thinkSeq
	s.thinkingPly, s.cancelThink, s.thinkStarted = ply, cancel, time.Now()
	s.slowTimer = time.AfterFunc(expected, func() {
		select {
		case s.slow <- seq:
		default:
		}
	})
	s.log.Info("thinking", "ply", ply, "expected", expected.String())
	go func() {
		d := s.brain.DecideMove(tctx, snap)
		select {
		case s.moves <- moveResult{seq: seq, ply: ply, decision: d}:
		case <-ctx.Done():
		}
	}()
}

func (s *Session) stopThinking() {
	if s.cancelThink != nil {
		s.cancelThink()
	}
	if s.slowTimer != nil {
		s.slowTimer.Stop()
	}
	s.cancelThink, s.slowTimer, s.thinkingPly = nil, nil, -1
}

// expectedThinkTime is the time the LLM should ideally take for a move:
// about 1/25 of the remaining time plus most of the increment, capped by max.
// The bot still waits past it, but tells the opponent that it is slow.
func expectedThinkTime(s Snapshot, max time.Duration) time.Duration {
	if !s.HasClock {
		return max
	}
	b := s.MyTime/25 + s.Increment*3/4 - 300*time.Millisecond
	if b < 500*time.Millisecond {
		b = 500 * time.Millisecond
	}
	if b > max {
		b = max
	}
	return b
}

// onSlow tells the opponent, once per game, that the LLM takes longer than
// expected and that the bot waits for it.
func (s *Session) onSlow(ctx context.Context, seq int) {
	if seq != s.thinkSeq || s.cancelThink == nil || s.slowAnnounced || s.finished {
		return
	}
	s.slowAnnounced = true
	s.log.Info("llm slower than expected, still waiting", "ply", s.thinkingPly,
		"waited_s", time.Since(s.thinkStarted).Round(100*time.Millisecond).Seconds())
	s.say(ctx, msgTooSlow)
}

func (s *Session) handleMove(ctx context.Context, r moveResult) {
	if r.seq != s.thinkSeq || s.cancelThink == nil {
		return // stale result
	}
	s.stopThinking()
	if s.finished || s.pos.Ply() != r.ply || s.pos.Turn() != s.color {
		return
	}
	d := r.decision
	if d.LLMDown {
		s.giveUp(ctx)
		return
	}
	if d.Resign {
		s.log.Info("resigning")
		if d.Chat != "" {
			s.say(ctx, d.Chat)
		}
		if err := s.api.Resign(ctx, s.id); err != nil {
			s.log.Error("resign failed", "err", err)
		}
		return
	}
	// Last safety net: never send an illegal move.
	uci, err := s.pos.ParseMove(d.Move)
	if err != nil {
		s.log.Error("refusing to send an illegal move", "move", d.Move, "err", err)
		s.postFailures[r.ply]++
		s.scheduleRetry()
		return
	}
	san := s.pos.SAN(uci)
	if err := s.api.Move(ctx, s.id, uci, d.OfferDraw); err != nil {
		s.postFailures[r.ply]++
		s.log.Error("sending move failed", "move", uci, "err", err, "failures", s.postFailures[r.ply])
		s.scheduleRetry()
		return
	}
	s.submittedPly = r.ply
	s.log.Info("move played", "ply", r.ply, "move", uci, "san", san, "offer_draw", d.OfferDraw,
		"think_s", time.Since(s.thinkStarted).Round(100*time.Millisecond).Seconds())
	if d.Chat != "" {
		s.say(ctx, d.Chat)
	}
}

// giveUp ends the game because the LLM stayed unreachable: the bot never
// plays a move the LLM did not choose. It aborts while Lichess allows it,
// and resigns otherwise.
func (s *Session) giveUp(ctx context.Context) {
	if s.pos.Ply() < 2 {
		s.log.Warn("llm unreachable at the start of the game, aborting")
		s.say(ctx, msgAbortLLMDown)
		err := s.api.Abort(ctx, s.id)
		if err == nil {
			return
		}
		s.log.Error("abort failed, resigning", "err", err)
	} else {
		s.log.Warn("llm unreachable for too long, resigning")
		s.say(ctx, msgResignLLMDown)
	}
	if err := s.api.Resign(ctx, s.id); err != nil {
		s.log.Error("resign failed", "err", err)
	}
}

func (s *Session) scheduleRetry() {
	time.AfterFunc(s.retryDelay, func() {
		select {
		case s.retry <- struct{}{}:
		default:
		}
	})
}

// onChat records an opponent message and asks the LLM for an answer. It
// never triggers a move.
func (s *Session) onChat(ctx context.Context, line lichess.ChatLine) {
	user := strings.ToLower(line.Username)
	if line.Room != "player" || user == s.me || user == "lichess" {
		return
	}
	s.log.Info("chat received", "user", line.Username, "text", line.Text)
	s.addEntry("opponent", line.Text)
	s.pendingChat = append(s.pendingChat, line.Text)
	s.maybeReply(ctx)
}

// maybeReply runs one chat request at a time; messages received meanwhile
// are answered together by the next request.
func (s *Session) maybeReply(ctx context.Context) {
	if s.chatBusy || len(s.pendingChat) == 0 || s.finished {
		return
	}
	batch := s.pendingChat
	s.pendingChat = nil
	s.chatBusy = true
	snap := s.snapshot()
	go func() {
		cctx, cancel := context.WithTimeout(ctx, chatTimeout)
		defer cancel()
		reply, err := s.brain.ReplyChat(cctx, snap, batch)
		select {
		case s.chats <- chatResult{reply: reply, err: err}:
		case <-ctx.Done():
		}
	}()
}

func (s *Session) handleChat(ctx context.Context, r chatResult) {
	s.chatBusy = false
	switch {
	case r.err != nil:
		s.log.Warn("chat request failed", "err", r.err)
	case r.reply.Reply == "":
		s.log.Info("chat: staying silent", "intent", r.reply.Intent)
	default:
		s.log.Info("chat reply", "intent", r.reply.Intent, "text", r.reply.Reply)
		s.say(ctx, r.reply.Reply)
	}
	s.maybeReply(ctx)
}

// say posts a message in the player chat and remembers it.
func (s *Session) say(ctx context.Context, text string) {
	for _, part := range splitChat(text, 2) {
		if err := s.api.Chat(ctx, s.id, "player", part); err != nil {
			s.log.Warn("sending chat failed", "err", err)
			return
		}
	}
	s.addEntry("you", text)
}

func (s *Session) addEntry(speaker, text string) {
	ply := 0
	if s.pos != nil {
		ply = s.pos.Ply()
	}
	s.transcript = append(s.transcript, Entry{Ply: ply, Speaker: speaker, Text: text})
}

// finish handles the end of the game: logs the result and says goodbye.
func (s *Session) finish(ctx context.Context) {
	result := describeResult(s.state, s.color)
	s.log.Info("game finished", "status", s.state.Status, "winner", s.state.Winner, "result", result, "moves", s.pos.Ply())
	if !s.cfg.Greet || s.state.Status == "aborted" || s.state.Status == "noStart" {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, farewellTimeout)
	defer cancel()
	msg, err := s.brain.Farewell(fctx, s.snapshot(), result)
	if err != nil {
		s.log.Warn("farewell failed", "err", err)
		return
	}
	if msg != "" {
		s.say(ctx, msg)
	}
}

// describeResult explains the end of the game from our point of view.
func describeResult(st lichess.GameState, me board.Color) string {
	outcome := "draw"
	switch st.Winner {
	case "":
	case string(me):
		outcome = "you won"
	default:
		outcome = "you lost"
	}
	reasons := map[string]string{
		"mate":                      "checkmate",
		"resign":                    "resignation",
		"stalemate":                 "stalemate",
		"timeout":                   "the opponent left the game",
		"outoftime":                 "time forfeit",
		"draw":                      "agreement or draw rule",
		"insufficientMaterialClaim": "insufficient material",
		"aborted":                   "the game was aborted",
	}
	reason, ok := reasons[st.Status]
	if !ok {
		reason = st.Status
	}
	return fmt.Sprintf("%s (%s)", outcome, reason)
}

// snapshot builds the read-only view handed to the Brain.
func (s *Session) snapshot() Snapshot {
	snap := Snapshot{
		Color:       s.color,
		Opponent:    s.opponent,
		Rated:       s.full.Rated,
		TimeControl: "correspondence",
		Pos:         s.pos.Clone(), // used by other goroutines
		Plan:        s.plan,
		Book:        s.book(),
	}
	if c := s.full.Clock; c != nil {
		snap.HasClock = true
		snap.TimeControl = fmt.Sprintf("%g+%d", float64(c.Initial)/60000, c.Increment/1000)
		my, opp, inc := s.state.WTime, s.state.BTime, s.state.WInc
		if s.color == board.Black {
			my, opp, inc = s.state.BTime, s.state.WTime, s.state.BInc
		}
		snap.MyTime = time.Duration(my) * time.Millisecond
		snap.OpponentTime = time.Duration(opp) * time.Millisecond
		snap.Increment = time.Duration(inc) * time.Millisecond
	}
	if s.color == board.White {
		snap.OpponentOffersDraw = s.state.BDraw
	} else {
		snap.OpponentOffersDraw = s.state.WDraw
	}
	n := s.cfg.ChatMemory
	if n <= 0 || n > len(s.transcript) {
		n = len(s.transcript)
	}
	snap.Transcript = append([]Entry(nil), s.transcript[len(s.transcript)-n:]...)
	return snap
}
