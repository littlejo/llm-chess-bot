package board

import (
	"errors"
	"strings"
	"testing"
)

func mustPos(t *testing.T, fen string, moves string) *Position {
	t.Helper()
	p, err := FromMoves(fen, SplitMoves(moves))
	if err != nil {
		t.Fatalf("FromMoves: %v", err)
	}
	return p
}

func TestFromMovesTurnAndPly(t *testing.T) {
	p := mustPos(t, StartPos, "e2e4 e7e5 g1f3")
	if p.Ply() != 3 || p.Turn() != Black {
		t.Fatalf("ply=%d turn=%s", p.Ply(), p.Turn())
	}
	if got := p.History(); got != "1. e4 e5 2. Nf3" {
		t.Fatalf("history = %q", got)
	}
}

func TestClone(t *testing.T) {
	p := mustPos(t, "8/P6k/8/8/8/8/8/K7 w - - 0 1", "a1b1")
	c := p.Clone()
	if c == p || c.FEN() != p.FEN() || c.Ply() != 1 {
		t.Fatalf("clone = %s (ply %d)", c.FEN(), c.Ply())
	}
	if _, err := c.Play("h7g7"); err != nil || p.Ply() != 1 {
		t.Fatal("clone is not independent")
	}
}

func TestFromMovesRejectsIllegal(t *testing.T) {
	if _, err := FromMoves(StartPos, []string{"e2e4", "e2e4"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestCastlingKingTakesRookIsNormalized(t *testing.T) {
	p := mustPos(t, StartPos, "e2e4 e7e5 g1f3 b8c6 f1c4 g8f6 e1h1")
	if got := p.Moves()[6]; got != "e1g1" {
		t.Fatalf("castling normalized to %q", got)
	}
}

func TestParseMove(t *testing.T) {
	p := mustPos(t, StartPos, "e2e4 e7e5")
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "g1f3", want: "g1f3"},
		{in: " G1F3 ", want: "g1f3"},
		{in: "Nf3", want: "g1f3"},
		{in: "Nf3!", want: "g1f3"},
		{in: "Qh5", want: "d1h5"},
		{in: "e4e5", wantErr: true},
		{in: "Ke3", wantErr: true},
		{in: "hello", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		got, err := p.ParseMove(tt.in)
		if tt.wantErr {
			if !errors.Is(err, ErrIllegalMove) {
				t.Errorf("ParseMove(%q) err = %v, want ErrIllegalMove", tt.in, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseMove(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	if p.Ply() != 2 {
		t.Fatal("ParseMove must not modify the position")
	}
}

func TestParseMoveSANWithCheckAndCastling(t *testing.T) {
	// Scholar's mate setup: Qxf7 is mate, written with or without "#".
	p := mustPos(t, StartPos, "e2e4 e7e5 f1c4 b8c6 d1h5 g8f6")
	for _, in := range []string{"Qxf7#", "Qxf7"} {
		if got, err := p.ParseMove(in); err != nil || got != "h5f7" {
			t.Errorf("ParseMove(%q) = %q, %v", in, got, err)
		}
	}
	p = mustPos(t, StartPos, "e2e4 e7e5 g1f3 b8c6 f1c4 g8f6")
	for _, in := range []string{"O-O", "0-0", "e1g1", "e1h1"} {
		if got, err := p.ParseMove(in); err != nil || got != "e1g1" {
			t.Errorf("ParseMove(%q) = %q, %v", in, got, err)
		}
	}
}

func TestPromotion(t *testing.T) {
	p := mustPos(t, "8/P6k/8/8/8/8/8/K7 w - - 0 1", "")
	if got, err := p.ParseMove("a7a8q"); err != nil || got != "a7a8q" {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := p.ParseMove("a8=Q"); err != nil || got != "a7a8q" {
		t.Fatalf("SAN promotion: got %q, %v", got, err)
	}
	if _, err := p.ParseMove("a7a8"); err == nil {
		t.Fatal("promotion without piece must be rejected")
	}
}

func TestOutcome(t *testing.T) {
	tests := []struct {
		name, fen, moves string
		want             Outcome
	}{
		{"ongoing", StartPos, "e2e4", Outcome{}},
		{"checkmate", StartPos, "f2f3 e7e5 g2g4 d8h4", Outcome{Over: true, Winner: Black, Reason: "Checkmate"}},
		{"stalemate", "7k/5Q2/6K1/8/8/8/8/8 b - - 0 1", "", Outcome{Over: true, Reason: "Stalemate"}},
	}
	for _, tt := range tests {
		got := mustPos(t, tt.fen, tt.moves).Outcome()
		if got != tt.want {
			t.Errorf("%s: got %+v want %+v", tt.name, got, tt.want)
		}
	}
}

func TestLegalMovesAndDiagram(t *testing.T) {
	p := mustPos(t, StartPos, "")
	if n := len(p.LegalMoves()); n != 20 {
		t.Fatalf("legal moves = %d", n)
	}
	if n := len(p.LegalMovesSAN()); n != 20 {
		t.Fatalf("legal SAN moves = %d", n)
	}
	d := p.Diagram(White)
	if !strings.HasPrefix(d, "8 r n b q k b n r") || !strings.HasSuffix(d, "a b c d e f g h") {
		t.Fatalf("white diagram:\n%s", d)
	}
	d = p.Diagram(Black)
	if !strings.HasPrefix(d, "1 R N B K Q B N R") || !strings.HasSuffix(d, "h g f e d c b a") {
		t.Fatalf("black diagram:\n%s", d)
	}
}

func TestParseLine(t *testing.T) {
	l, err := ParseLine("Ruy Lopez", "1. e4 e5 2. Nf3 Nc6 3.Bb5")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(l.UCI, " ") != "e2e4 e7e5 g1f3 b8c6 f1b5" {
		t.Fatalf("UCI = %v", l.UCI)
	}
	if l.String() != "1. e4 e5 2. Nf3 Nc6 3. Bb5" {
		t.Fatalf("String = %q", l.String())
	}
	bad, err := ParseLine("bad", "e4 e5 Nf6")
	if err == nil {
		t.Fatal("expected error for illegal line")
	}
	if bad.String() != "1. e4 e5" {
		t.Fatalf("legal prefix = %q", bad.String())
	}
}

func TestFollow(t *testing.T) {
	l, _ := ParseLine("Italian", "e4 e5 Nf3 Nc6 Bc4")
	tests := []struct {
		name   string
		played string
		want   BookStatus
	}{
		{"start", "", BookStatus{State: InBook, NextUCI: "e2e4", NextSAN: "e4"}},
		{"in book", "e2e4 e7e5", BookStatus{State: InBook, NextUCI: "g1f3", NextSAN: "Nf3"}},
		{"deviation", "e2e4 c7c5", BookStatus{State: Deviated, DeviationPly: 1, Expected: "e5", Played: "c7c5"}},
		{"completed", "e2e4 e7e5 g1f3 b8c6 f1c4", BookStatus{State: BookCompleted}},
		{"beyond", "e2e4 e7e5 g1f3 b8c6 f1c4 g8f6", BookStatus{State: BookCompleted}},
	}
	for _, tt := range tests {
		if got := l.Follow(SplitMoves(tt.played)); got != tt.want {
			t.Errorf("%s: got %+v want %+v", tt.name, got, tt.want)
		}
	}
	if got := (Line{}).Follow(nil); got.State != NoBook {
		t.Errorf("empty line: %+v", got)
	}
}

func TestParseMoveLooseSAN(t *testing.T) {
	p := mustMoves(t, "e2e4 d7d6 g1f3 g8f6 d2d4 g7g6 c1f4 f8g7")
	for in, want := range map[string]string{"Bd6": "f4d6", "Bxd6": "f4d6", "Nbd2": "b1d2"} {
		if got, err := p.ParseMove(in); err != nil || got != want {
			t.Errorf("ParseMove(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := p.ParseMove("Nd2"); err == nil {
		t.Error("an ambiguous SAN move must be rejected")
	}
}

func TestExplain(t *testing.T) {
	pirc := "e2e4 d7d6 g1f3 g8f6 d2d4 g7g6 c1f4 f8g7"
	promo, err := FromMoves("8/4P3/8/8/8/8/k7/4K3 w - - 0 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		pos  *Position
		move string
		want []string
	}{
		{mustMoves(t, pirc), "f1g5", []string{"Your bishop on f1 cannot go to g5; its legal moves are Be2, Bd3", "Did you mean Bg5?"}},
		{mustMoves(t, pirc), "O-O", []string{"cannot castle kingside yet", "(bishop on f1)"}},
		{mustMoves(t, pirc), "e1g1", []string{"cannot castle kingside yet"}},
		{mustMoves(t, pirc+" h1g1 e8g8"), "O-O", []string{"no longer castle kingside"}},
		{mustMoves(t, pirc), "Nd2", []string{"Several knights can go to d2", "Nbd2", "Nfd2"}},
		{mustMoves(t, pirc), "Bh7", []string{"None of your bishops can go to h7 (bishop on f1: Be2, Bd3"}},
		{mustMoves(t, pirc), "e2e4", []string{"There is no piece on e2"}},
		{mustMoves(t, pirc), "hello", []string{"not written in SAN"}},
		{mustMoves(t, "e2e4 d7d5 f1b5"), "a7a6", []string{"You are in check", "Your pawn on a7 cannot go to a6"}},
		{promo, "e8", []string{"must promote", "e8=Q"}},
	}
	for _, tt := range tests {
		got := tt.pos.Explain(tt.move)
		for _, w := range tt.want {
			if !strings.Contains(got, w) {
				t.Errorf("Explain(%q) = %q, want it to contain %q", tt.move, got, w)
			}
		}
	}
}

func mustMoves(t *testing.T, moves string) *Position {
	t.Helper()
	p, err := FromMoves(StartPos, SplitMoves(moves))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
