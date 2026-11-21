# llm-chess-bot

A Lichess bot whose moves **and** conversation come from an LLM. The goal is
an opponent you can talk to, not a chess engine: no Stockfish is involved. The
program itself makes sure that no illegal move is ever sent.

- It accepts or declines challenges based on variant, time control, rated or
  casual, and human or bot.
- It plays through the Lichess Bot API, follows games by streaming and
  reconnects on its own.
- The LLM picks its moves in UCI format and the chess library validates them.
  After an illegal move, the LLM is asked again with the error, until it gives
  a legal one. **Every move comes from the LLM**: the program never plays a
  move by itself, and it waits for the LLM's answer even if that costs time on
  the clock.
- The player chat is followed. Messages are classified as "conversation" or
  "game", and the context is kept for the whole game. **A chat message never
  makes the bot play a move.**
- The bot follows a requested opening or variation, or picks one suited to the
  opponent's level. When the opponent leaves the line, it notices, says so and
  adapts. It explains its choices in the chat.

The preliminary analysis (API, architecture, difficulties, risks) is in
[docs/DESIGN.md](docs/DESIGN.md).

## Requirements

- [mise](https://mise.jdx.dev/), which installs the right Go version (Go ≥ 1.25
  is required). A local Go ≥ 1.25 also works.
- A **dedicated, brand new** Lichess account that has never played a game.
- A [LiteLLM](https://docs.litellm.ai/) proxy, or any other OpenAI-compatible
  API (`/v1/chat/completions`).

### Create the BOT account

1. Create the Lichess account. **Do not play any game with it**: an account
   that has played a game can no longer become a BOT.
2. Create a personal API token with this link, which preselects the right
   scope:
   <https://lichess.org/account/oauth/token/create?scopes[]=bot:play&description=llm-chess-bot>.
   Only one box is needed, **"Play games with the bot API"** (`bot:play`). It
   covers the upgrade, the event stream, challenges, moves and chat. Leave the
   other boxes unchecked.
3. Copy the token (it starts with `lip_`) into `.env` as
   `LICHESS_TOKEN=lip_...` (create the file with `cp .env.example .env`).
   Lichess shows the token only once.
4. Upgrade the account to BOT. This is **irreversible**:

   ```sh
   source .env
   curl -d '' https://lichess.org/api/bot/account/upgrade -H "Authorization: Bearer $LICHESS_TOKEN"
   ```

   The expected answer is `{"ok":true}`.
5. Check the result with `mise run check` (see below): it must show
   `title "BOT"`.

The bot refuses to start if the account does not have the `BOT` title, so it
never accepts a challenge it could not play.

## Quick start with mise

```sh
mise trust && mise install     # installs Go
cp .env.example .env           # then fill in LICHESS_TOKEN and LITELLM_API_KEY
cp config.example.json config.json
mise run test                  # unit and integration tests
mise run check                 # check the Lichess token and the LLM connection
mise run bot                   # play on Lichess
```

| Task | What it does |
|---|---|
| `mise run test` | Run all tests |
| `mise run test:race` | Run the tests with the race detector (needs a C compiler) |
| `mise run lint` | Check formatting (`gofmt`) and run `go vet` |
| `mise run build` | Build `bin/llm-chess-bot` |
| `mise run check` | Check the Lichess token (and BOT title) and send a test prompt to the LLM, without playing |
| `mise run bot` | Run the bot on Lichess |

`check` and `bot` read `config.json`; use another file with `CONFIG=other.json
mise run bot` (or set `CONFIG` in `.env`). Without mise, the check is
`go run ./cmd/llm-chess-bot -config config.json -check`.

mise loads `.env` automatically. The file is git-ignored, so secrets stay out
of the repository.

## Configuration

Without mise:

```sh
cp config.example.json config.json
export LICHESS_TOKEN=lip_xxx
export LITELLM_API_KEY=sk-xxx
go run ./cmd/llm-chess-bot -config config.json
```

The `provider` block and the `model` field use **the same format as
`opencode.json`**. You can reuse your opencode configuration as is, including
`{env:NAME}` references:

```json
"provider": {
  "litellm": {
    "npm": "@ai-sdk/openai-compatible",
    "options": { "baseURL": "http://localhost:4000/v1", "apiKey": "{env:LITELLM_API_KEY}" },
    "models": { "gpt-4o": {} }
  }
},
"model": "litellm/gpt-4o"
```

These environment variables override the file: `LICHESS_TOKEN`,
`LICHESS_BASE_URL`, `LLM_MODEL` (`provider/model`), `LLM_BASE_URL`,
`LLM_API_KEY` and `LOG_LEVEL`. With `-config ""`, the configuration comes from
the environment only.

| Section | Key | Default | Purpose |
|---|---|---|---|
| `challenges` | `variants` | `["standard"]` | Accepted variants (`fromPosition` is supported) |
| | `min_initial_seconds` | `300` | Minimum estimated duration (initial time + 40 × increment) |
| | `min_increment_seconds` | `0` | Minimum increment |
| | `allow_correspondence` | `false` | Correspondence and unlimited games |
| | `allow_rated` / `allow_casual` | `false` / `true` | Rated or casual games |
| | `allow_bots` | `false` | Challenges from other bots |
| `bot` | `max_games` | `1` | Games played at the same time |
| | `persona` | friendly and talkative | Personality given to the LLM |
| | `max_think_seconds` | `60` | Expected thinking time per move, also bounded by the clock (about 1/25 of the remaining time). The bot always waits for the LLM; past this time, it tells the opponent once that it is slow |
| | `chat_memory` | `40` | Chat transcript entries included in prompts |
| | `move_retries` | `2` | LLM retries after an illegal move |
| | `greet` | `true` | Greet, announce the opening and say goodbye |
| `log` | `level`, `format` | `info`, `json` | Structured logs (`json` or `text`) |

### Opening instructions

Each color gets its own instruction:

```json
"openings": {
  "white": { "name": "Italian Game, Giuoco Piano", "moves": "1. e4 e5 2. Nf3 Nc6 3. Bc4 Bc5 4. c3",
             "instructions": "Aim for open, attacking positions." },
  "black": { "instructions": "Solid against stronger players, sharp against weaker ones." }
}
```

- **`name` + `moves`** set a variation. The line is written in SAN from the
  initial position, opponent moves included, and is validated when loaded. On
  every move, the bot compares the game with the line. While the line is
  followed, the prompt gives the LLM the next book move. As soon as either side
  leaves it, the prompt says the line is abandoned and the LLM adapts.
- **`name` alone** sets an opening without a precise line.
- **Neither `name` nor `moves`**: the LLM picks the opening at the start of the
  game, based on the opponent's rating and title and on `instructions`. It
  provides its own line, which is then followed the same way.

In every case, when `greet` is on, the LLM announces its opening and explains
its choice in the chat.

To **change the instruction between games without a restart**, set
`"openings_file": "openings.json"`. This file contains
`{"white": {...}, "black": {...}}` and is read again at the start of every game.

## How it works

```
cmd/llm-chess-bot/   entry point: config, logs, signals
internal/config/     JSON in opencode format + environment variables
internal/lichess/    minimal HTTP client, ndjson streams, 429, keep-alive, reconnection
internal/llm/        Provider interface + OpenAI-compatible client (LiteLLM)
internal/board/      move replay, UCI/SAN validation, rendering, opening tracking
internal/bot/        challenges, game sessions, prompts, decisions
```

- Each game is a `Session`: a single goroutine that owns the game state. LLM
  requests run separately and send their results back through a channel,
  tagged with the ply. A stale result is ignored (the opponent resigned, the
  position changed, and so on).
- The position is **rebuilt on every event** from the full move list sent by
  Lichess, so reconnections and duplicate events are harmless.
- The move prompt contains the FEN, an ASCII board from the bot's side, the
  SAN history, the list of legal moves, the clocks, the opening plan and the
  chat transcript.
- The chat prompt can only produce text, at most 140 characters per message.
  Game requests ("play e5", "draw?") get a polite answer, but nothing is ever
  executed from the chat. Accepting a draw requires an official Lichess offer,
  and the LLM decides when it makes its move.
- The bot waits for the LLM's move as long as it takes, so a slow LLM can
  lose on time. Choose time controls with an increment at least as long as
  the LLM's usual answer time.
- If the LLM cannot be reached:
  - at startup, the bot refuses to start;
  - during a game, it retries every 5 seconds; after 2 minutes without an
    answer, it aborts the game if both sides have not moved yet, and resigns
    otherwise;
  - for 2 minutes after a failure, it declines new challenges (`later`).
- A watchdog monitors the streams: after 30 s without data or keep-alive, the
  bot reconnects with exponential backoff. After an HTTP 429, it waits one
  minute.
- On Ctrl-C, the bot stops without resigning its games. On restart, Lichess
  sends the running games again and the bot resumes them.

## Tests

```sh
mise run test     # or: go test ./...
```

The tests cover:
- the chess logic: validation, `e1h1` castling, promotion, checkmate,
  stalemate, variation tracking;
- the Lichess client: ndjson, keep-alive, watchdog, 429;
- the configuration and the LLM client;
- the challenge filter;
- sessions, with fake Lichess and LLM:
  - one move per turn;
  - chat never triggers a move;
  - an illegal move is retried until the LLM gives a legal one;
  - a slow LLM is waited for, and the opponent is told once;
  - leaving the opening line is announced;
  - a draw offer is accepted;
  - the game ends while the LLM is thinking;
  - a move is sent again after a network error;
  - an unreachable LLM is retried, then aborts or resigns the game, without
    ever playing a move by itself.

An integration test plays a full game over real HTTP against fake Lichess and
LiteLLM servers.

## Known limitations (MVP)

- The playing strength is the LLM's. Prefer casual games.
- Variants other than `standard` and `fromPosition` are not supported, nor are
  takeback requests (ignored) or claiming victory when the opponent has left.
- The spectator room is ignored.
- After a game stream reconnection, a chat message that was already handled
  might get a second answer.
