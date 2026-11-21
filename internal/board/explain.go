package board

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/corentings/chess/v2"
)

// sanPattern splits a SAN move (without check marks) into piece, origin file,
// origin rank, target square and promotion.
var sanPattern = regexp.MustCompile(`^([KQRBN])?([a-h])?([1-8])?x?([a-h][1-8])=?([QRBN])?$`)

var pieceNames = map[chess.PieceType]string{
	chess.King: "king", chess.Queen: "queen", chess.Rook: "rook",
	chess.Bishop: "bishop", chess.Knight: "knight", chess.Pawn: "pawn",
}

var pieceFromLetter = map[string]chess.PieceType{
	"K": chess.King, "Q": chess.Queen, "R": chess.Rook, "B": chess.Bishop, "N": chess.Knight, "": chess.Pawn,
}

// Explain tells, in plain English, why move (SAN or UCI) is not legal in the
// position and which legal moves come closest to it. It is meant for the
// LLM after ParseMove rejected its move.
func (p *Position) Explain(move string) string {
	var out []string
	if p.inCheck() {
		out = append(out, "You are in check: only moves that get your king out of check are legal.")
	}
	m := strings.TrimRight(strings.TrimSpace(move), "!?+#")
	switch castle := strings.ReplaceAll(m, "0", "O"); {
	case m == "":
		out = append(out, "The move is empty.")
	case castle == "O-O" || castle == "O-O-O":
		out = append(out, p.explainCastling(castle == "O-O-O"))
	case uciPattern.MatchString(strings.ToLower(m)):
		out = append(out, p.explainUCI(strings.ToLower(m)))
	case sanPattern.MatchString(cleanSAN(m)):
		out = append(out, p.explainSAN(sanPattern.FindStringSubmatch(cleanSAN(m))))
	default:
		out = append(out, fmt.Sprintf("%q is not written in SAN (examples: e4, Nf3, exd5, O-O, e8=Q).", move))
	}
	return strings.Join(out, " ")
}

func (p *Position) explainUCI(m string) string {
	pos := p.game.Position()
	from, to := chess.SquareFromString(m[0:2]), chess.SquareFromString(m[2:4])
	pc := pos.Board().Piece(from)
	me := pos.Turn()
	if pc.Type() == chess.King && pc.Color() == me && from.File() == chess.FileE && (m[2] == 'g' || m[2] == 'c') && m[1] == m[3] {
		return p.explainCastling(m[2] == 'c')
	}
	var msg string
	switch {
	case pc == chess.NoPiece:
		msg = fmt.Sprintf("There is no piece on %s.", from)
	case pc.Color() != me:
		msg = fmt.Sprintf("The %s on %s belongs to your opponent.", pieceNames[pc.Type()], from)
	default:
		msg = fmt.Sprintf("Your %s on %s cannot go to %s; %s.", pieceNames[pc.Type()], from, to, describeMoves(p.movesFrom(from)))
	}
	pt := chess.NoPieceType
	if pc != chess.NoPiece && pc.Color() == me {
		pt = pc.Type()
	}
	if alt := p.movesTo(pt, to); len(alt) > 0 {
		msg += fmt.Sprintf(" Did you mean %s?", strings.Join(alt, " or "))
	}
	return msg
}

func (p *Position) explainSAN(parts []string) string {
	pos := p.game.Position()
	pt, file, rank, target, promo := pieceFromLetter[parts[1]], parts[2], parts[3], parts[4], parts[5]
	to := chess.SquareFromString(target)
	name := pieceNames[pt]

	var candidates []string // legal moves of that piece type to the target square
	var matching []string   // those that also match the origin given in the move
	for _, mv := range pos.ValidMoves() {
		if mv.S2() != to || pos.Board().Piece(mv.S1()).Type() != pt {
			continue
		}
		san := chess.AlgebraicNotation{}.Encode(pos, &mv)
		candidates = append(candidates, san)
		origin := mv.S1().String()
		if (file == "" || origin[0:1] == file) && (rank == "" || origin[1:2] == rank) {
			matching = append(matching, san)
		}
	}
	switch {
	case len(candidates) == 0:
		mine := p.squaresOf(pt)
		if len(mine) == 0 {
			return fmt.Sprintf("You have no %s left.", name)
		}
		var lines []string
		for _, sq := range mine {
			moves := p.movesFrom(sq)
			if len(moves) == 0 {
				moves = []string{"no legal move"}
			}
			lines = append(lines, fmt.Sprintf("%s on %s: %s", name, sq, strings.Join(moves, ", ")))
		}
		return fmt.Sprintf("None of your %ss can go to %s (%s).", name, to, strings.Join(lines, "; "))
	case pt == chess.Pawn && promo == "" && (to.Rank() == chess.Rank8 || to.Rank() == chess.Rank1):
		return fmt.Sprintf("A pawn reaching the last rank must promote: write one of %s.", strings.Join(candidates, ", "))
	case len(matching) == 0:
		return fmt.Sprintf("No %s from that square can go to %s. Did you mean %s?", name, to, strings.Join(candidates, " or "))
	case len(matching) > 1:
		return fmt.Sprintf("Several %ss can go to %s: write exactly one of %s.", name, to, strings.Join(matching, ", "))
	default:
		return fmt.Sprintf("Write it exactly as %s.", matching[0])
	}
}

// explainCastling tells why castling on the given side is not legal.
func (p *Position) explainCastling(queenSide bool) string {
	pos := p.game.Position()
	me := pos.Turn()
	side, sideName := chess.KingSide, "kingside"
	between, path := []string{"f", "g"}, []string{"f", "g"}
	if queenSide {
		side, sideName = chess.QueenSide, "queenside"
		between, path = []string{"b", "c", "d"}, []string{"d", "c"}
	}
	rank := "1"
	if me == chess.Black {
		rank = "8"
	}
	if !pos.CastleRights().CanCastle(me, side) {
		return fmt.Sprintf("You can no longer castle %s: your king or that rook has already moved.", sideName)
	}
	var blocked []string
	for _, f := range between {
		sq := chess.SquareFromString(f + rank)
		if pc := pos.Board().Piece(sq); pc != chess.NoPiece {
			blocked = append(blocked, fmt.Sprintf("%s on %s", pieceNames[pc.Type()], sq))
		}
	}
	if len(blocked) > 0 {
		return fmt.Sprintf("You cannot castle %s yet: first move the pieces between your king and rook (%s).", sideName, strings.Join(blocked, ", "))
	}
	if p.inCheck() {
		return "You cannot castle while in check."
	}
	for _, f := range path {
		if sq := chess.SquareFromString(f + rank); p.attacked(sq, me.Other()) {
			return fmt.Sprintf("You cannot castle %s now: your king would cross or land on %s, which is attacked.", sideName, sq)
		}
	}
	return fmt.Sprintf("You cannot castle %s right now.", sideName)
}

// movesFrom lists, in SAN, the legal moves of the piece on sq.
func (p *Position) movesFrom(sq chess.Square) []string {
	pos := p.game.Position()
	var moves []string
	for _, mv := range pos.ValidMoves() {
		if mv.S1() == sq {
			moves = append(moves, chess.AlgebraicNotation{}.Encode(pos, &mv))
		}
	}
	return moves
}

func describeMoves(moves []string) string {
	if len(moves) == 0 {
		return "it has no legal move (it is blocked or pinned)"
	}
	return "its legal moves are " + strings.Join(moves, ", ")
}

// movesTo lists the legal moves to sq, of a given piece type unless pt is
// NoPieceType.
func (p *Position) movesTo(pt chess.PieceType, sq chess.Square) []string {
	pos := p.game.Position()
	var moves []string
	for _, mv := range pos.ValidMoves() {
		if mv.S2() == sq && (pt == chess.NoPieceType || pos.Board().Piece(mv.S1()).Type() == pt) {
			moves = append(moves, chess.AlgebraicNotation{}.Encode(pos, &mv))
		}
	}
	return moves
}

// squaresOf returns the squares of the side to move's pieces of type pt.
func (p *Position) squaresOf(pt chess.PieceType) []chess.Square {
	pos := p.game.Position()
	var squares []chess.Square
	for sq, pc := range pos.Board().SquareMap() {
		if pc.Type() == pt && pc.Color() == pos.Turn() {
			squares = append(squares, sq)
		}
	}
	slices.Sort(squares)
	return squares
}

func (p *Position) inCheck() bool {
	pos := p.game.Position()
	for sq, pc := range pos.Board().SquareMap() {
		if pc.Type() == chess.King && pc.Color() == pos.Turn() {
			return p.attacked(sq, pos.Turn().Other())
		}
	}
	return false
}

// attacked reports whether a piece of color by attacks sq.
func (p *Position) attacked(sq chess.Square, by chess.Color) bool {
	b := p.game.Position().Board()
	for from, pc := range b.SquareMap() {
		if pc.Color() == by && slices.Contains(b.AttacksFrom(from), sq) {
			return true
		}
	}
	return false
}
