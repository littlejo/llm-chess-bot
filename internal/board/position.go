// Package board wraps the chess rules library: it rebuilds positions from the
// move list sent by Lichess, validates moves proposed by the LLM and renders
// the position in formats the LLM can read.
package board

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/corentings/chess/v2"
)

// StartPos is the value Lichess uses for the standard initial position.
const StartPos = "startpos"

// Color is the side to move.
type Color string

const (
	White Color = "white"
	Black Color = "black"
)

// Opposite returns the other color.
func (c Color) Opposite() Color {
	if c == White {
		return Black
	}
	return White
}

// ErrIllegalMove is returned when a move is not legal in the position.
var ErrIllegalMove = errors.New("illegal move")

// Position is a game replayed from its initial position and move list.
//
// A Position is not safe for concurrent use, even for reads (the chess
// library caches legal moves lazily): give each goroutine its own Clone.
type Position struct {
	game       *chess.Game
	initialFEN string
	moves      []string // normalized UCI moves
}

// FromMoves replays uciMoves from initialFEN ("" or "startpos" for the
// standard position). Castling written king-takes-rook (e1h1) is accepted.
func FromMoves(initialFEN string, uciMoves []string) (*Position, error) {
	var game *chess.Game
	if initialFEN == "" || initialFEN == StartPos {
		game = chess.NewGame()
	} else {
		opt, err := chess.FEN(initialFEN)
		if err != nil {
			return nil, fmt.Errorf("invalid initial FEN %q: %w", initialFEN, err)
		}
		game = chess.NewGame(opt)
	}
	p := &Position{game: game, initialFEN: initialFEN}
	for i, m := range uciMoves {
		if m == "" {
			continue
		}
		if _, err := p.Play(m); err != nil {
			return nil, fmt.Errorf("replaying move %d (%s): %w", i+1, m, err)
		}
	}
	return p, nil
}

// Clone returns an independent copy of the position.
func (p *Position) Clone() *Position {
	c, err := FromMoves(p.initialFEN, p.moves)
	if err != nil { // cannot happen: the moves were validated when played
		panic(err)
	}
	return c
}

// SplitMoves splits the space separated move list sent by Lichess.
func SplitMoves(moves string) []string {
	return strings.Fields(moves)
}

// Play validates and plays a move given in UCI or SAN. It returns the
// normalized UCI form of the move.
func (p *Position) Play(move string) (string, error) {
	uci, err := p.ParseMove(move)
	if err != nil {
		return "", err
	}
	if err := p.game.PushNotationMove(uci, chess.UCINotation{}, nil); err != nil {
		return "", fmt.Errorf("%w: %s", ErrIllegalMove, move)
	}
	p.moves = append(p.moves, uci)
	return uci, nil
}

// ParseMove checks that move (UCI or SAN) is legal in the current position and
// returns it in normalized UCI notation. It never modifies the position.
func (p *Position) ParseMove(move string) (string, error) {
	move = strings.TrimSpace(move)
	if move == "" {
		return "", fmt.Errorf("%w: empty move", ErrIllegalMove)
	}
	pos := p.game.Position()
	legal := pos.ValidMoves()

	if uciPattern.MatchString(strings.ToLower(move)) {
		uci := normalizeCastling(pos, strings.ToLower(move))
		for i := range legal {
			if legal[i].String() == uci {
				return uci, nil
			}
		}
		return "", fmt.Errorf("%w: %s", ErrIllegalMove, move)
	}

	// SAN is compared loosely ("Bd6" for "Bxd6", "e8Q" for "e8=Q"), as long
	// as a single legal move matches.
	san := looseSAN(move)
	var found []string
	for i := range legal {
		if looseSAN(chess.AlgebraicNotation{}.Encode(pos, &legal[i])) == san {
			found = append(found, legal[i].String())
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	return "", fmt.Errorf("%w: %s", ErrIllegalMove, move)
}

var uciPattern = regexp.MustCompile(`^[a-h][1-8][a-h][1-8][qrbn]?$`)

// looseSAN removes from a SAN move what the LLM often gets wrong without
// changing its meaning: annotations, capture and promotion signs.
func looseSAN(s string) string {
	return strings.NewReplacer("x", "", "=", "").Replace(cleanSAN(s))
}

// cleanSAN removes annotations the LLM may add (check marks, "!?") and
// accepts zeros for castling. Checks are compared without "+"/"#".
func cleanSAN(s string) string {
	s = strings.TrimRight(s, "!?+#")
	s = strings.ReplaceAll(s, "0-0-0", "O-O-O")
	s = strings.ReplaceAll(s, "0-0", "O-O")
	return s
}

// normalizeCastling converts king-takes-rook castling (e1h1) into the
// standard UCI form (e1g1) when that is what the move means.
func normalizeCastling(pos *chess.Position, uci string) string {
	from := chess.SquareFromString(uci[0:2])
	to := chess.SquareFromString(uci[2:4])
	b := pos.Board()
	king := b.Piece(from)
	rook := b.Piece(to)
	if king.Type() != chess.King || rook.Type() != chess.Rook || king.Color() != rook.Color() {
		return uci
	}
	rank := uci[1:2]
	switch uci[2] {
	case 'h':
		return uci[0:2] + "g" + rank
	case 'a':
		return uci[0:2] + "c" + rank
	}
	return uci
}

// Moves returns the normalized UCI moves played so far.
func (p *Position) Moves() []string { return append([]string(nil), p.moves...) }

// Ply returns the number of half-moves played since the initial position.
func (p *Position) Ply() int { return len(p.moves) }

// Turn returns the side to move.
func (p *Position) Turn() Color {
	if p.game.Position().Turn() == chess.White {
		return White
	}
	return Black
}

// FEN returns the current position in FEN.
func (p *Position) FEN() string { return p.game.FEN() }

// LegalMoves returns all legal moves in UCI notation.
func (p *Position) LegalMoves() []string {
	legal := p.game.Position().ValidMoves()
	out := make([]string, len(legal))
	for i := range legal {
		out[i] = legal[i].String()
	}
	return out
}

// LegalMovesSAN returns all legal moves in SAN.
func (p *Position) LegalMovesSAN() []string {
	pos := p.game.Position()
	legal := pos.ValidMoves()
	out := make([]string, len(legal))
	for i := range legal {
		out[i] = chess.AlgebraicNotation{}.Encode(pos, &legal[i])
	}
	return out
}

// SAN converts a legal UCI move into SAN for the current position.
func (p *Position) SAN(uci string) string {
	pos := p.game.Position()
	legal := pos.ValidMoves()
	for i := range legal {
		if legal[i].String() == uci {
			return chess.AlgebraicNotation{}.Encode(pos, &legal[i])
		}
	}
	return uci
}

// History returns the moves played in numbered SAN, e.g. "1. e4 e5 2. Nf3".
func (p *Position) History() string {
	positions := p.game.Positions()
	moves := p.game.Moves()
	var sb strings.Builder
	for i, m := range moves {
		pos := positions[i]
		number := pos.Ply()/2 + 1
		if pos.Turn() == chess.White {
			fmt.Fprintf(&sb, "%d. ", number)
		} else if i == 0 {
			fmt.Fprintf(&sb, "%d... ", number)
		}
		sb.WriteString(chess.AlgebraicNotation{}.Encode(pos, m))
		if i < len(moves)-1 {
			sb.WriteByte(' ')
		}
	}
	return sb.String()
}

// Diagram returns an ASCII board seen from the given side (uppercase = white).
func (p *Position) Diagram(perspective Color) string {
	b := p.game.Position().Board()
	ranks := []int{7, 6, 5, 4, 3, 2, 1, 0}
	files := []int{0, 1, 2, 3, 4, 5, 6, 7}
	if perspective == Black {
		ranks = []int{0, 1, 2, 3, 4, 5, 6, 7}
		files = []int{7, 6, 5, 4, 3, 2, 1, 0}
	}
	var sb strings.Builder
	for _, r := range ranks {
		fmt.Fprintf(&sb, "%d ", r+1)
		for _, f := range files {
			pc := b.Piece(chess.NewSquare(chess.File(f), chess.Rank(r)))
			sb.WriteString(pieceLetter(pc))
			sb.WriteByte(' ')
		}
		sb.WriteByte('\n')
	}
	sb.WriteString("  ")
	for _, f := range files {
		sb.WriteByte(byte('a' + f))
		sb.WriteByte(' ')
	}
	return strings.TrimRight(sb.String(), " ")
}

func pieceLetter(pc chess.Piece) string {
	if pc == chess.NoPiece {
		return "."
	}
	letter := map[chess.PieceType]string{
		chess.King: "k", chess.Queen: "q", chess.Rook: "r",
		chess.Bishop: "b", chess.Knight: "n", chess.Pawn: "p",
	}[pc.Type()]
	if pc.Color() == chess.White {
		return strings.ToUpper(letter)
	}
	return letter
}

// Outcome describes the result of the game according to the board alone
// (checkmate, stalemate, insufficient material...). Over is false while the
// game can continue.
type Outcome struct {
	Over   bool
	Winner Color // empty for a draw
	Reason string
}

// Outcome returns the board outcome of the current position.
func (p *Position) Outcome() Outcome {
	o := p.game.Outcome()
	if o == chess.NoOutcome || o == chess.UnknownOutcome {
		return Outcome{}
	}
	out := Outcome{Over: true, Reason: p.game.Method().String()}
	switch o {
	case chess.WhiteWon:
		out.Winner = White
	case chess.BlackWon:
		out.Winner = Black
	}
	return out
}
