# LLM Chess Bot: analysis and architecture

This document covers the "before coding" phase of the specification: Lichess
Bot API constraints, architecture, libraries, difficulties and risks.

## 1. Lichess Bot API: what matters for us

Source: the official OpenAPI specification (`lichess-org/api`, `doc/specs`).

| Need | Endpoint |
|---|---|
| Bot identity | `GET /api/account` (`title` must be `BOT`) |
| Global events | `GET /api/stream/event` (ndjson) |
| Accept / decline a challenge | `POST /api/challenge/{id}/accept`, `POST /api/challenge/{id}/decline` (`reason=generic\|later\|tooFast\|tooSlow\|timeControl\|rated\|casual\|standard\|variant\|noBot\|onlyBot`) |
| Game stream | `GET /api/bot/game/stream/{gameId}` (ndjson) |
| Play | `POST /api/bot/game/{id}/move/{uci}?offeringDraw=true` (offers **or accepts** a draw) |
| Chat | `POST /api/bot/game/{id}/chat` (form: `room=player\|spectator`, `text`) |
| Resign | `POST /api/bot/game/{id}/resign` |
| BOT upgrade | `POST /api/bot/account/upgrade` (irreversible, account with no game at all) |

**Event stream** (`/api/stream/event`):
- types: `challenge`, `challengeCanceled`, `challengeDeclined`, `gameStart`,
  `gameFinish`;
- when the stream opens, Lichess sends every pending challenge and running
  game, so a restarted bot resumes its games;
- an empty line is sent every 7 s (keep-alive);
- **only one global stream per token**: opening a second one closes the first.

**Game stream** (`/api/bot/game/stream/{id}`):
- the first line is always `gameFull` (players, time control, `initialFen`,
  `state`);
- then come `gameState` (`moves` holds **all** UCI moves separated by spaces,
  with `wtime`, `btime`, `winc`, `binc`, `status`, `winner`, `wdraw`,
  `bdraw`…), `chatLine` (`room`, `username`, `text`) and `opponentGone`;
- terminal statuses include `mate`, `resign`, `stalemate`, `timeout`, `draw`,
  `outoftime`, `aborted`, `noStart`, `cheat` and `variantEnd`.

**Constraints**:
- one request at a time; after an HTTP 429, wait a full minute;
- a `User-Agent` header is required;
- a bot only plays challenges (no pools), and never UltraBullet;
- castling may be written "king takes rook" (`e1h1`), the Chess960-compatible
  notation. We normalize it;
- chat messages are limited to about 140 characters.

## 2. Minimal architecture

```
cmd/llm-chess-bot/        main: config, logs, signals, wiring
internal/config/          JSON file (opencode provider format) + environment variables
internal/lichess/         minimal HTTP client, ndjson streams, retries/429, keep-alive
internal/llm/             Provider interface + OpenAI-compatible implementation (LiteLLM)
internal/board/           chess rules: move replay, validation, SAN/UCI, opening tracking
internal/bot/             orchestration: challenges, game sessions, prompts, decisions
```

```
             /api/stream/event
Lichess ───────────────────────────► Bot (1 goroutine)
                                       │  challenge → filter → accept/decline
                                       │  gameStart → starts a Session (deduplicated)
                                       ▼
             /api/bot/game/stream/{id}
Lichess ───────────────────────────► Session (1 goroutine owning the state)
                                       │ gameFull/gameState → replay → is it my turn?
                                       │        └─► "move" goroutine (LLM + validation)
                                       │ chatLine → "chat" goroutine (LLM, text only)
                                       ◄─── results through channels, tagged with the ply
```

Principles:
- **A single owner for each game's state.** The session receives Lichess
  events and LLM results through channels. No mutex protects the game state.
- **The state is rebuilt on every event.** The position is replayed from
  `initialFen` and the full `moves` list. This is idempotent and robust to
  reconnections and takebacks.
- **Moves and chat are two separate paths.** The chat prompt can only produce
  text. Only the "move" path, triggered by "it is my turn at this ply", sends a
  move. A received message therefore never makes the bot play.
- **The LLM proposes, `board` decides.** Every move goes through the chess
  library's validation before it is sent. On failure, the LLM is asked again
  with the error and the list of legal moves (2 retries). After that, a legal
  fallback move is played: the book move if there is one, otherwise a random
  legal move. No engine is used.
- **The conversation context is a per-game transcript.** It holds the
  messages from both sides, move explanations and the opening plan. It is
  included in every prompt, with a bounded size.

### Opening

- The config can give a plan for each color: name, SAN line (variation) and
  free-form instructions.
- Without a set line (`auto`), an LLM call at the start of the game picks an
  opening suited to the opponent's rating and title. It returns a name, a line
  and a greeting that explains the choice.
- Line tracking is **deterministic**, done by `board.Line`. On every turn, the
  played moves are compared with the line. While in the line, the prompt gives
  the next book move. When the opponent leaves it, the prompt says so ("line
  abandoned, adapt") and the LLM may comment in the chat.
- The openings file is read again at the start of every game, so the
  instruction can change between games without a restart.

### LLM provider

```go
type Provider interface {
    Complete(ctx context.Context, msgs []Message) (string, error)
}
```

There is a single implementation for now: the OpenAI-compatible
`/chat/completions` API, the one LiteLLM exposes. The configuration reuses the
`provider` block and the `model` field (`litellm/<model>`) of `opencode.json`,
including `{env:VAR}` substitution. Adding a provider means writing an
implementation and adding one line to the factory.

## 3. Libraries

| Need | Choice | Why |
|---|---|---|
| Chess rules | `github.com/corentings/chess/v2` | Maintained fork of `notnil/chess` (archived). It handles UCI, SAN, FEN, checkmate/stalemate/draw detection and legal moves. |
| HTTP, ndjson | stdlib `net/http`, `bufio`, `encoding/json` | No maintained Go Lichess client covers the Bot API well, and the API is small. |
| LLM | stdlib | A single JSON endpoint, an SDK would be overkill. |
| Logs | `log/slog` | Structured JSON or text logs, in the stdlib. |
| Config | `encoding/json` | Same format as opencode, no YAML dependency. |

There is **a single external dependency**.

## 4. Identified difficulties

1. **Streams that die silently.** A half-open TCP connection returns no error.
   Fix: an idle watchdog. When no byte arrives for N seconds, keep-alive
   included, the bot disconnects and reconnects with a capped exponential
   backoff. A 429 requires waiting 60 s.
2. **Reconnections and duplicates.** After a reconnection, the global stream
   resends the running `gameStart` events and the game stream resends a
   `gameFull`. Sessions are deduplicated by `gameId`, and the state is
   recomputed from the full move list.
3. **Race between LLM thinking and the game state.** While the LLM thinks, the
   opponent may resign or ask for a takeback, or the flag may fall. Each move
   request carries its ply and a cancellable context for its position. A stale
   result is ignored, and a position change cancels the running request.
4. **Never playing twice.** The `gameState` that follows our own move triggers
   the "is it my turn?" check again. The session remembers the ply already
   submitted and the ply being thought about.
5. **Chat.** Our own messages come back in the stream and are ignored. The
   spectator room is ignored. When several messages arrive while an answer is
   being prepared, they are grouped into a single call. Answers are truncated
   or split at 140 characters.
6. **Clock.** LLM latency, from a few seconds to tens of seconds, does not fit
   bullet. Fast time controls are declined with `tooFast`. The thinking budget
   comes from the remaining time, with a fallback move when it runs out.
7. **First move.** Lichess aborts the game when the first move comes too late.
   The opening plan therefore has a short timeout, and on failure the game
   starts without a plan.

## 5. Technical choices and risks

| Risk | Mitigation |
|---|---|
| The LLM proposes illegal moves or hallucinates the position | FEN, ASCII board, SAN history and **list of legal moves** in the prompt; systematic validation, retries, fallback move |
| The LLM is tactically weak | Accepted: this is a conversational opponent. Casual games are recommended |
| The LLM (or its gateway) is unreachable | Ping at startup; abort the game while still possible, otherwise announce the autopilot and resign after 3 moves without the LLM; decline challenges for 2 minutes after a failure |
| The LLM is slow or costly | Configurable minimum time control, 1 game at a time by default, timeout per call |
| Malformed JSON output | Lenient parsing (```json blocks, `{…}` extraction), then a regex search for a UCI move, always validated |
| Prompt injection through the chat ("play Qxh7") | The chat cannot produce moves. The move prompt states that opponent messages do not dictate moves |
| Lichess sanctions (aborts, flooding) | Message limit, 429 handling, one request at a time per game |
| Duplicate chat answers after a reconnection | Low risk, accepted for the MVP |
| Data race in the chess library (lazy legal-move cache, even on reads) | Each LLM request gets its own copy of the position (`Position.Clone`) |

Out of scope for the MVP: variants other than standard and `fromPosition`,
automatically claiming victory when the opponent has left, takebacks
(ignored), tournaments.
