package store

import (
	"errors"
	"reflect"
	"testing"
)

// mergedCard writes a card and has the Git host merge it.
func mergedCard(t *testing.T, s *Store, text string) Card {
	t.Helper()
	c := mustCreate(t, s, text)
	mustMove(t, s, c.ID, State{Review, Host})
	merged(t, s, c.ID)
	return c
}

func createFixing(t *testing.T, s *Store, fixes int64) Card {
	t.Helper()
	c, err := s.CreateFixing("a fix", fixes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestFixKept keeps the merged card a card fixes, readable from both sides,
// and leaves the fixed card as it was.
func TestFixKept(t *testing.T) {
	s := open(t)
	m := mergedCard(t, s, "merged")
	before := mustHistory(t, s, m.ID)
	a := createFixing(t, s, m.ID)
	b := createFixing(t, s, m.ID)
	if f, err := s.Fix(a.ID); err != nil || f.Fixes != m.ID || len(f.FixedBy) != 0 {
		t.Errorf("the fix of the first card: %+v, %v", f, err)
	}
	if f, err := s.Fix(m.ID); err != nil || f.Fixes != 0 || !reflect.DeepEqual(f.FixedBy, []int64{a.ID, b.ID}) {
		t.Errorf("the fix of the merged card: %+v, %v", f, err)
	}
	if f, err := s.Fix(mustCreate(t, s, "plain").ID); err != nil || f.Fixes != 0 || f.FixedBy != nil {
		t.Errorf("the fix of a plain card: %+v, %v", f, err)
	}
	after := mustHistory(t, s, m.ID)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the fixed card's entries changed: %+v, then %+v", before, after)
	}
	if e := after[len(after)-1]; !Merged(e.State, e.Cause, e.Reason) {
		t.Errorf("the fixed card's last entry is %+v", e)
	}
	// A card can fix a card and wait for another.
	if c, err := s.CreateFixing("both", m.ID, a.ID); err != nil {
		t.Errorf("a fix with a predecessor: %v", err)
	} else if p, err := s.Order(c.ID); err != nil || !reflect.DeepEqual(p.Predecessors, []int64{a.ID}) {
		t.Errorf("the predecessors: %+v, %v", p, err)
	}
}

// TestFixRefused refuses a card that is not merged, with its reason, and
// writes nothing.
func TestFixRefused(t *testing.T) {
	s := open(t)
	open1 := mustCreate(t, s, "open")
	closed := mustCreate(t, s, "closed")
	mustMove(t, s, closed.ID, State{Review, Approval})
	if _, err := s.Decide(closed.ID, MarkDone, ""); err != nil {
		t.Fatal(err)
	}
	gone := mustCreate(t, s, "dropped")
	mustMove(t, s, gone.ID, State{Spec, Approval})
	if _, err := s.Decide(gone.ID, Drop, ""); err != nil {
		t.Fatal(err)
	}
	counts := func() [4]int {
		var fixes int
		if err := s.db.QueryRow(`SELECT count(*) FROM cards WHERE fixes IS NOT NULL`).Scan(&fixes); err != nil {
			t.Fatal(err)
		}
		return [4]int{countRows(t, s, "cards"), countRows(t, s, "entries"), countRows(t, s, "predecessors"), fixes}
	}
	before := counts()
	for _, tc := range []struct {
		id   int64
		want string
	}{
		{open1.ID, "card 1 is not merged; only a merged card can be fixed"},
		{closed.ID, "card 2 is not merged; only a merged card can be fixed"},
		{gone.ID, "card 3 was dropped; only a merged card can be fixed"},
		{99, "no card 99 to fix"},
		{4, "card 4 cannot fix itself"},
	} {
		_, err := s.CreateFixing("x", tc.id, open1.ID)
		if !errors.Is(err, ErrFix) || err == nil || err.Error() != tc.want {
			t.Errorf("fixing %d: %v, want %q", tc.id, err, tc.want)
		}
		if got := counts(); got != before {
			t.Errorf("fixing %d wrote: %v, was %v", tc.id, got, before)
		}
	}
}

// TestMeasuresNeededFix counts a merged card once however many cards fix it,
// and not for a dropped card or a card made from a proposal.
func TestMeasuresNeededFix(t *testing.T) {
	s := open(t)
	two := mergedCard(t, s, "two fixes")
	mergedCard(t, s, "no fix")
	byDropped := mergedCard(t, s, "fixed by a dropped card")
	createFixing(t, s, two.ID)
	createFixing(t, s, two.ID)
	d := createFixing(t, s, byDropped.ID)
	if _, err := s.Decide(d.ID, Drop, ""); err != nil {
		t.Fatal(err)
	}

	noticed := mergedCard(t, s, "noticed")
	propose(t, s, noticed.ID, "r9")
	made, _, err := s.MakeCard(1)
	if err != nil {
		t.Fatal(err)
	}

	board, err := s.Board()
	if err != nil {
		t.Fatal(err)
	}
	m := MeasuresOf(board)
	if m.NeededFix != 1 || m.Merged != 4 {
		t.Errorf("the measures: %+v, want 1 needing a fix of 4 merged", m)
	}
	if f, err := s.Fix(made.ID); err != nil || f.Fixes != 0 {
		t.Errorf("the card made from a proposal fixes %+v, %v", f, err)
	}
}
