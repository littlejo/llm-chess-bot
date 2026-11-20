package lichess

import "encoding/json"

// Event is a line of the global event stream (/api/stream/event).
type Event struct {
	Type      string     `json:"type"` // challenge, challengeCanceled, challengeDeclined, gameStart, gameFinish
	Challenge *Challenge `json:"challenge,omitempty"`
	Game      *GameInfo  `json:"game,omitempty"`
}

// Challenge is an incoming or outgoing challenge.
type Challenge struct {
	ID          string                `json:"id"`
	Status      string                `json:"status"`
	Challenger  User                  `json:"challenger"`
	DestUser    *User                 `json:"destUser"`
	Variant     Variant               `json:"variant"`
	Rated       bool                  `json:"rated"`
	Speed       string                `json:"speed"`
	TimeControl TimeControl           `json:"timeControl"`
	Color       string                `json:"color"`
	Direction   string                `json:"direction"` // "in" or "out"
	InitialFEN  string                `json:"initialFen"`
	Perf        struct{ Name string } `json:"perf"`
}

// User is a player as described in challenges.
type User struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Rating      int    `json:"rating"`
	Provisional bool   `json:"provisional"`
}

// Variant identifies the chess variant.
type Variant struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// TimeControl of a challenge. Limit and Increment are in seconds.
type TimeControl struct {
	Type        string `json:"type"` // clock, correspondence, unlimited
	Limit       int    `json:"limit"`
	Increment   int    `json:"increment"`
	DaysPerTurn int    `json:"daysPerTurn"`
}

// GameInfo is sent with gameStart and gameFinish events.
type GameInfo struct {
	GameID   string `json:"gameId"`
	FullID   string `json:"fullId"`
	Color    string `json:"color"`
	FEN      string `json:"fen"`
	IsMyTurn bool   `json:"isMyTurn"`
	Opponent struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Rating   int    `json:"rating"`
	} `json:"opponent"`
}

// GameEvent is a line of a bot game stream. Exactly one of the pointers is
// set according to Type.
type GameEvent struct {
	Type         string
	Full         *GameFull
	State        *GameState
	Chat         *ChatLine
	OpponentGone *OpponentGone
}

// GameFull is the first line of a game stream.
type GameFull struct {
	ID         string     `json:"id"`
	Variant    Variant    `json:"variant"`
	Speed      string     `json:"speed"`
	Rated      bool       `json:"rated"`
	Clock      *Clock     `json:"clock"`
	White      GamePlayer `json:"white"`
	Black      GamePlayer `json:"black"`
	InitialFEN string     `json:"initialFen"`
	State      GameState  `json:"state"`
}

// Clock of a real time game, in milliseconds.
type Clock struct {
	Initial   int `json:"initial"`
	Increment int `json:"increment"`
}

// GamePlayer is a player in a game stream.
type GamePlayer struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Title   string `json:"title"`
	Rating  int    `json:"rating"`
	AILevel int    `json:"aiLevel"`
}

// GameState is the current state of a game. Times are in milliseconds.
type GameState struct {
	Moves  string `json:"moves"`
	WTime  int64  `json:"wtime"`
	BTime  int64  `json:"btime"`
	WInc   int64  `json:"winc"`
	BInc   int64  `json:"binc"`
	Status string `json:"status"`
	Winner string `json:"winner"`
	WDraw  bool   `json:"wdraw"`
	BDraw  bool   `json:"bdraw"`
}

// ChatLine is a chat message in a game.
type ChatLine struct {
	Room     string `json:"room"` // player or spectator
	Username string `json:"username"`
	Text     string `json:"text"`
}

// OpponentGone tells that the opponent left the game.
type OpponentGone struct {
	Gone              bool `json:"gone"`
	ClaimWinInSeconds int  `json:"claimWinInSeconds"`
}

// Account is the authenticated user.
type Account struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Title    string `json:"title"`
}

// Finished reports whether status is a terminal game status.
func Finished(status string) bool {
	switch status {
	case "", "created", "started":
		return false
	}
	return true
}

func decodeGameEvent(line []byte) (GameEvent, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return GameEvent{}, err
	}
	ev := GameEvent{Type: head.Type}
	var err error
	switch head.Type {
	case "gameFull":
		ev.Full = &GameFull{}
		err = json.Unmarshal(line, ev.Full)
	case "gameState":
		ev.State = &GameState{}
		err = json.Unmarshal(line, ev.State)
	case "chatLine":
		ev.Chat = &ChatLine{}
		err = json.Unmarshal(line, ev.Chat)
	case "opponentGone":
		ev.OpponentGone = &OpponentGone{}
		err = json.Unmarshal(line, ev.OpponentGone)
	}
	return ev, err
}
