package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// waiter writes a card that names the predecessors and starts it.
func waiter(t *testing.T, s *Store, text string, predecessors ...int64) Card {
	t.Helper()
	c, err := s.Create(text, predecessors...)
	if err != nil {
		t.Fatal(err)
	}
	if c, err = s.Start(c.ID); err != nil {
		t.Fatal(err)
	}
	return c
}

// mustMove moves a card, by the rule.
func mustMove(t *testing.T, s *Store, id int64, to State) {
	t.Helper()
	if _, err := s.Move(id, to, CauseRule); err != nil {
		t.Fatal(err)
	}
}

// merged moves a card to done as the Git host's merge does.
func merged(t *testing.T, s *Store, id int64) {
	t.Helper()
	c, err := s.Card(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveWithReason(id, c.State, State{Done, Nothing}, CauseHost, MergedReason); err != nil {
		t.Fatal(err)
	}
}

// release runs Release and gives the IDs and the reasons of the cards it
// moved, with the number of entries it wrote.
func release(t *testing.T, s *Store) (moved []string, wrote int) {
	t.Helper()
	before := countRows(t, s, "entries")
	got, err := s.Release()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		moved = append(moved, fmt.Sprintf("%d %s/%s: %s", r.ID, r.Stage, r.WaitsFor, r.Reason))
	}
	return moved, countRows(t, s, "entries") - before
}

// summaryOf is a card's summary on the board.
func summaryOf(t *testing.T, s *Store, id int64) Summary {
	t.Helper()
	b, err := s.Board()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range b {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("card %d is not on the board", id)
	return Summary{}
}

// TestRelease has a card with two predecessors, one done: it is queued for
// the other, the rule writes nothing until that one is done, then starts it,
// and a repeat writes nothing. In a chain only the next card starts. A card
// whose predecessor was dropped waits, once none is open, for the developer,
// who can start it.
func TestRelease(t *testing.T) {
	s := open(t)
	a := mustCreate(t, s, "a").ID
	mustMove(t, s, a, State{Review, Approval})
	if _, err := s.Decide(a, MarkDone, ""); err != nil { // closed without publishing
		t.Fatal(err)
	}
	b := mustCreate(t, s, "b").ID
	mustMove(t, s, b, State{Spec, Run})
	c := waiter(t, s, "c", b, a)
	if c.State != (State{Backlog, Room}) {
		t.Fatalf("the started card is in %v, want queued", c.State)
	}
	h := mustHistory(t, s, c.ID)
	if e := h[len(h)-1]; e.Cause != CauseDeveloper || e.Reason != fmt.Sprintf("%s#%d", QueuedPrefix, b) || e.Note() != "" {
		t.Errorf("the start's entry is %+v, with the note %q", e, e.Note())
	}
	if moved, wrote := release(t, s); moved != nil || wrote != 0 {
		t.Errorf("with a predecessor open, Release moved %q and wrote %d entries", moved, wrote)
	}
	if sum := summaryOf(t, s, c.ID); !reflect.DeepEqual(sum.Predecessors, []int64{a, b}) || !reflect.DeepEqual(sum.QueuedFor, []int64{b}) {
		t.Errorf("the board has the predecessors %v and the wait %v", sum.Predecessors, sum.QueuedFor)
	}
	if o, err := s.Order(b); err != nil || len(o.NamedBy) != 1 || o.NamedBy[0].ID != c.ID || o.Predecessors != nil || o.QueuedFor != nil {
		t.Errorf("the order of the predecessor: %+v, %v", o, err)
	}
	bHistory := len(mustHistory(t, s, b))

	merged(t, s, b)
	want := fmt.Sprintf("%d spec/run: %s#%d, #%d", c.ID, ReleasedPrefix, a, b)
	if moved, wrote := release(t, s); !reflect.DeepEqual(moved, []string{want}) || wrote != 1 {
		t.Errorf("Release moved %q and wrote %d entries, want %q", moved, wrote, want)
	}
	h = mustHistory(t, s, c.ID)
	if e := h[len(h)-1]; e.Cause != CauseRule || e.State != (State{Spec, Run}) || e.Note() != "" {
		t.Errorf("the rule's entry is %+v", e)
	}
	if moved, wrote := release(t, s); moved != nil || wrote != 0 {
		t.Errorf("a repeat moved %q and wrote %d entries", moved, wrote)
	}
	if n := len(mustHistory(t, s, b)); n != bHistory+1 { // the merge alone
		t.Errorf("the predecessor has %d entries, want %d", n, bHistory+1)
	}
	if sum := summaryOf(t, s, c.ID); sum.QueuedFor != nil {
		t.Errorf("a released card waits for %v", sum.QueuedFor)
	}
	if o, err := s.Order(b); err != nil || len(o.NamedBy) != 1 {
		t.Errorf("a released card is no longer named: %+v, %v", o, err)
	}
	merged(t, s, c.ID)
	if o, err := s.Order(b); err != nil || o.NamedBy != nil {
		t.Errorf("a done card is still named: %+v, %v", o, err)
	}

	t.Run("a chain", func(t *testing.T) {
		x := mustCreate(t, s, "x").ID
		mustMove(t, s, x, State{Spec, Run})
		y := waiter(t, s, "y", x).ID
		z := waiter(t, s, "z", y).ID
		merged(t, s, x)
		if moved, _ := release(t, s); !reflect.DeepEqual(moved, []string{fmt.Sprintf("%d spec/run: %s#%d", y, ReleasedPrefix, x)}) {
			t.Errorf("Release moved %q, want only %d", moved, y)
		}
		if zc, _ := s.Card(z); zc.State != (State{Backlog, Room}) {
			t.Errorf("the last of the chain is in %v", zc.State)
		}
	})

	t.Run("a dropped predecessor", func(t *testing.T) {
		p := mustCreate(t, s, "p").ID
		q := mustCreate(t, s, "q").ID
		mustMove(t, s, q, State{Spec, Approval})
		d := waiter(t, s, "d", p, q).ID
		if _, err := s.Decide(p, Drop, ""); err != nil {
			t.Fatal(err)
		}
		if moved, wrote := release(t, s); moved != nil || wrote != 0 {
			t.Errorf("with a second predecessor open, Release moved %q and wrote %d", moved, wrote)
		}
		if sum := summaryOf(t, s, d); !reflect.DeepEqual(sum.QueuedFor, []int64{q}) {
			t.Errorf("the card waits for %v, want [%d]", sum.QueuedFor, q)
		}
		merged(t, s, q)
		want := fmt.Sprintf("%d backlog/developer: its predecessor #%d was dropped; start it without it, or drop it", d, p)
		if moved, _ := release(t, s); !reflect.DeepEqual(moved, []string{want}) {
			t.Errorf("Release moved %q, want %q", moved, want)
		}
		if moved, wrote := release(t, s); moved != nil || wrote != 0 {
			t.Errorf("a repeat moved %q and wrote %d", moved, wrote)
		}
		dc, _ := s.Card(d)
		if m, err := s.Moves(dc, false); err != nil || !reflect.DeepEqual(m, []Allowed{{Start, NoNote}, {Drop, OptionalNote}}) {
			t.Errorf("the moves are %v, %v", m, err)
		}
		if got, err := s.Start(d); err != nil || got.State != (State{Spec, Run}) {
			t.Fatalf("the start: %+v, %v", got, err)
		}
		h := mustHistory(t, s, d)
		if e := h[len(h)-1]; e.Cause != CauseDeveloper || e.Reason != "" {
			t.Errorf("the start's entry is %+v", e)
		}
		if _, err := s.Start(d); !errors.Is(err, ErrRefused) {
			t.Errorf("a second start: %v", err)
		}

		// Two dropped.
		u, v := mustCreate(t, s, "u").ID, mustCreate(t, s, "v").ID
		w := waiter(t, s, "w", u, v).ID
		for _, id := range []int64{u, v} {
			if _, err := s.Decide(id, Drop, "no"); err != nil {
				t.Fatal(err)
			}
		}
		want = fmt.Sprintf("%d backlog/developer: its predecessors #%d, #%d were dropped; start it without them, or drop them", w, u, v)
		if moved, _ := release(t, s); !reflect.DeepEqual(moved, []string{want}) {
			t.Errorf("Release moved %q, want %q", moved, want)
		}
	})
}

// TestStartWithout starts a card with no predecessor, or with a done one: it
// goes to spec, waiting for a run, with no reason.
func TestStartWithout(t *testing.T) {
	s := open(t)
	a := mustCreate(t, s, "a").ID
	merged(t, s, a)
	for _, c := range []Card{mustCreate(t, s, "b"), func() Card { c, _ := s.Create("c", a); return c }()} {
		got, err := s.Start(c.ID)
		h := mustHistory(t, s, c.ID)
		if err != nil || got.State != (State{Spec, Run}) || h[len(h)-1].Reason != "" {
			t.Errorf("card %d started: %+v, %v, %+v", c.ID, got, err, h[len(h)-1])
		}
	}
}

// TestPredecessorsRefused refuses a card that names itself, a card that does
// not exist or a dropped card, and writes nothing; a card named twice is
// written once.
func TestPredecessorsRefused(t *testing.T) {
	s := open(t)
	mustCreate(t, s, "one")
	two := mustCreate(t, s, "two").ID
	if _, err := s.Decide(two, Drop, ""); err != nil {
		t.Fatal(err)
	}
	entries := countRows(t, s, "entries")
	for _, tc := range []struct {
		id   int64
		want string
	}{
		{3, "a card cannot be its own predecessor"},
		{9, "no card 9 to wait for"},
		{2, "card 2 was dropped, so no card can wait for it"},
	} {
		_, err := s.Create("x", 1, tc.id)
		if !errors.Is(err, ErrPredecessor) || err.Error() != tc.want {
			t.Errorf("naming %d: %v, want %q", tc.id, err, tc.want)
		}
		if b, _ := s.Board(); len(b) != 2 || countRows(t, s, "entries") != entries || countRows(t, s, "predecessors") != 0 {
			t.Errorf("naming %d wrote %d cards, %d entries, %d predecessors", tc.id, len(b), countRows(t, s, "entries"), countRows(t, s, "predecessors"))
		}
	}
	c, err := s.Create("x", 1, 1)
	if err != nil || c.ID != 3 || countRows(t, s, "predecessors") != 1 {
		t.Errorf("a predecessor named twice: %+v, %v, %d rows", c, err, countRows(t, s, "predecessors"))
	}
}
