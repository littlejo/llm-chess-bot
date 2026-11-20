package board

import (
	"fmt"
	"regexp"
	"strings"
)

// Line is an opening line (a variation) played from the standard initial
// position, stored both in SAN (for the LLM) and UCI (for comparisons).
type Line struct {
	Name string
	SAN  []string
	UCI  []string
}

var moveNumber = regexp.MustCompile(`^\d+\.+`)

// ParseLine parses a SAN (or UCI) line such as "1. e4 e5 2. Nf3 Nc6 3. Bb5"
// and checks that every move is legal. On error, the returned line holds the
// legal prefix parsed before the faulty move.
func ParseLine(name, moves string) (Line, error) {
	line := Line{Name: name}
	p, _ := FromMoves(StartPos, nil)
	for _, tok := range strings.Fields(moves) {
		tok = moveNumber.ReplaceAllString(tok, "")
		if tok == "" || tok == "*" {
			continue
		}
		san := ""
		uci, err := p.ParseMove(tok)
		if err == nil {
			san = p.SAN(uci)
			_, err = p.Play(uci)
		}
		if err != nil {
			return line, fmt.Errorf("opening %q, move %d (%s): %w", name, len(line.UCI)+1, tok, err)
		}
		line.SAN = append(line.SAN, san)
		line.UCI = append(line.UCI, uci)
	}
	return line, nil
}

// Empty reports whether the line contains no moves.
func (l Line) Empty() bool { return len(l.UCI) == 0 }

// BookState tells where the game stands relative to an opening line.
type BookState int

const (
	// NoBook: there is no line to follow.
	NoBook BookState = iota
	// InBook: every move played so far follows the line and it continues.
	InBook
	// BookCompleted: the whole line has been played.
	BookCompleted
	// Deviated: a move left the line; the plan must be adapted.
	Deviated
)

// BookStatus is the result of comparing played moves with a Line.
type BookStatus struct {
	State BookState
	// NextUCI/NextSAN: the next move of the line when State == InBook.
	NextUCI string
	NextSAN string
	// DeviationPly (0-based), Expected and Played describe the first move
	// that left the line when State == Deviated.
	DeviationPly int
	Expected     string
	Played       string
}

// Follow compares the moves played (normalized UCI) with the line.
func (l Line) Follow(played []string) BookStatus {
	if l.Empty() {
		return BookStatus{State: NoBook}
	}
	for i, m := range played {
		if i >= len(l.UCI) {
			return BookStatus{State: BookCompleted}
		}
		if m != l.UCI[i] {
			return BookStatus{State: Deviated, DeviationPly: i, Expected: l.SAN[i], Played: m}
		}
	}
	if len(played) >= len(l.UCI) {
		return BookStatus{State: BookCompleted}
	}
	return BookStatus{State: InBook, NextUCI: l.UCI[len(played)], NextSAN: l.SAN[len(played)]}
}

// String returns the line in numbered SAN.
func (l Line) String() string {
	var sb strings.Builder
	for i, san := range l.SAN {
		if i%2 == 0 {
			fmt.Fprintf(&sb, "%d. ", i/2+1)
		}
		sb.WriteString(san)
		if i < len(l.SAN)-1 {
			sb.WriteByte(' ')
		}
	}
	return sb.String()
}
