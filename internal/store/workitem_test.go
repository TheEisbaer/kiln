package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

var specApproval = State{Spec, Approval}

func cardCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM cards`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func wantWorkItem(t *testing.T, s *Store, id, want int64) {
	t.Helper()
	c, err := s.Card(id)
	if err != nil || c.WorkItem != want {
		t.Fatalf("card %d reads work item %d, %v; want %d", id, c.WorkItem, err, want)
	}
}

// TestWorkItemKept: a number given at writing and at approval is kept, a later
// approval changes it, and an approval without one leaves it.
func TestWorkItemKept(t *testing.T) {
	s := open(t)
	written, err := s.CreateWithWorkItem(KilnRepo, "with a number", 0, 42)
	if err != nil || written.WorkItem != 42 {
		t.Fatalf("CreateWithWorkItem: %+v, %v", written, err)
	}
	wantWorkItem(t, s, written.ID, 42)
	if b, err := s.Board(); err != nil || b[0].WorkItem != 42 {
		t.Errorf("the board: %+v, %v", b, err)
	}
	if c := mustCreate(t, s, "without"); c.WorkItem != 0 {
		t.Errorf("a card written without a number has %d", c.WorkItem)
	}

	card := mustCreate(t, s, "approved with one")
	if _, err := s.Move(card.ID, specApproval, CauseRun); err != nil {
		t.Fatal(err)
	}
	got, err := s.DecideWith(card.ID, Approve, "", Options{WorkItem: 7})
	if err != nil || got.WorkItem != 7 {
		t.Fatalf("approve with 7: %+v, %v", got, err)
	}
	wantWorkItem(t, s, card.ID, 7)
	h, _ := s.History(card.ID)
	if last := h[len(h)-1]; last.Reason != "" {
		t.Errorf("the approval's entry has the reason %q", last.Reason)
	}

	// A send-back to spec, a second approval with 9, then one without.
	if _, err := s.Move(card.ID, specApproval, CauseRun); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(card.ID, State{Implement, Run}, CauseRun); err != nil {
		t.Fatal(err)
	}
	wantWorkItem(t, s, card.ID, 7)
	if _, err := s.Move(card.ID, specApproval, CauseRun); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideWith(card.ID, Approve, "", Options{WorkItem: 9}); err != nil {
		t.Fatal(err)
	}
	wantWorkItem(t, s, card.ID, 9)
	if _, err := s.Move(card.ID, specApproval, CauseRun); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideWith(card.ID, Approve, "", Options{}); err != nil {
		t.Fatal(err)
	}
	wantWorkItem(t, s, card.ID, 9)
}

// TestWorkItemRefused: values outside 1 to 2147483647 and moves that take no
// number are refused with ErrWorkItem, and nothing is written.
func TestWorkItemRefused(t *testing.T) {
	s := open(t)
	for _, n := range []int64{-1, 2147483648} {
		before := cardCount(t, s)
		_, err := s.CreateWithWorkItem(KilnRepo, "x", 0, n)
		if !errors.Is(err, ErrWorkItem) || cardCount(t, s) != before {
			t.Errorf("create with %d: %v, cards %d, was %d", n, err, cardCount(t, s), before)
		}
	}
	// 0 is none, not a value.
	if _, err := s.CreateWithWorkItem(KilnRepo, "zero is none", 0, 0); err != nil {
		t.Errorf("create with 0: %v", err)
	}

	card := mustCreate(t, s, "a card")
	for _, a := range []Action{Again, SendBack, MarkDone, Drop, Start} {
		before, _ := s.History(card.ID)
		c, err := s.DecideWith(card.ID, a, "", Options{WorkItem: 3})
		after, _ := s.History(card.ID)
		if !errors.Is(err, ErrWorkItem) || err.Error() != "only approve and publish take a work item" ||
			c.WorkItem != 0 || len(after) != len(before) {
			t.Errorf("%s with a number: %+v, %v", a, c, err)
		}
		if got, _ := s.Card(card.ID); got.State != (State{Backlog, Developer}) {
			t.Errorf("%s with a number moved the card to %v", a, got.State)
		}
	}
	if _, err := s.Move(card.ID, specApproval, CauseRun); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int64{-5, 2147483648} {
		if _, err := s.DecideWith(card.ID, Approve, "", Options{WorkItem: n}); !errors.Is(err, ErrWorkItem) {
			t.Errorf("approve with %d: %v", n, err)
		}
	}
	if got, _ := s.Card(card.ID); got.State != specApproval || got.WorkItem != 0 {
		t.Errorf("a refused approval left %+v", got)
	}
}

// TestPublishWorkItem: publish gives the number to a card without one, repeats
// the card's own, refuses another, and refuses a card of a repo that needs one.
func TestPublishWorkItem(t *testing.T) {
	for _, from := range []State{passed, needsYou} {
		s := open(t)
		card := mustCreate(t, s, "a card")
		if from == needsYou { // after a publishing that failed
			if _, err := s.MoveWithReason(card.ID, State{Backlog, Developer}, from, CauseRule, PublishFailedReason+": x"); err != nil {
				t.Fatal(err)
			}
		} else if _, err := s.Move(card.ID, from, CauseRun); err != nil {
			t.Fatal(err)
		}
		entries, _ := s.History(card.ID)
		need := Options{Publishing: true, NeedsWorkItem: true}
		c, err := s.DecideWith(card.ID, Publish, "", need)
		want := "the repo kiln needs a work item number before publishing; give it with the approval or when you publish"
		if !errors.Is(err, ErrWorkItem) || err.Error() != want {
			t.Fatalf("from %v: publish without a number: %+v, %v", from, c, err)
		}
		if got, _ := s.Card(card.ID); got.State != from {
			t.Errorf("a refused publish moved the card to %v", got.State)
		}
		if after, _ := s.History(card.ID); len(after) != len(entries) {
			t.Errorf("a refused publish wrote an entry")
		}
		if p, _ := s.Publication(card.ID); p != (Publication{}) {
			t.Errorf("a refused publish wrote the publication %+v", p)
		}
		need.WorkItem = 5
		c, err = s.DecideWith(card.ID, Publish, "", need)
		if err != nil || c.State != atTheHost || c.WorkItem != 5 {
			t.Fatalf("from %v: publish with 5: %+v, %v", from, c, err)
		}
		wantWorkItem(t, s, card.ID, 5)
	}

	// A card with a number: repeating it passes, another is refused, and none
	// passes where one is needed since the card has one.
	s := open(t)
	card, err := s.CreateWithWorkItem(KilnRepo, "numbered", 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(card.ID, passed, CauseRun); err != nil {
		t.Fatal(err)
	}
	_, err = s.DecideWith(card.ID, Publish, "", Options{Publishing: true, WorkItem: 9})
	if !errors.Is(err, ErrWorkItem) || err.Error() != "the card's work item is #8; only an approval of its spec changes it" {
		t.Fatalf("publish with another number: %v", err)
	}
	wantWorkItem(t, s, card.ID, 8)
	if got, _ := s.Card(card.ID); got.State != passed {
		t.Errorf("a refused publish moved the card to %v", got.State)
	}
	if _, err := s.DecideWith(card.ID, Publish, "", Options{Publishing: true, NeedsWorkItem: true}); err != nil {
		t.Errorf("publish of a numbered card of a repo that needs one: %v", err)
	}
	wantWorkItem(t, s, card.ID, 8)
}

// TestMigrateV16 moves a store of version 16 forward: its cards have no work
// item, and a kill in the move leaves version 16.
func TestMigrateV16(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, "the first card")
	mustCreate(t, s, "the second card")
	if _, err := s.db.Exec(`ALTER TABLE cards DROP COLUMN work_item; ALTER TABLE publishes DROP COLUMN feedback_since; PRAGMA user_version = 16`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before := dump(t, path)

	func() {
		defer func() { afterMigration = nil }()
		afterMigration = func() { panic("killed") }
		defer func() {
			if r := recover(); r == nil {
				t.Error("the move forward was not stopped")
			}
		}()
		Open(path)
	}()
	if got := dump(t, path); got != before || !strings.HasPrefix(got, "version 16\n") {
		t.Fatalf("after a kill the store is\n%s\nwas\n%s", got, before)
	}

	s = reopen(t, path)
	if v := userVersion(t, s); v != version {
		t.Fatalf("the store is version %d, want %d", v, version)
	}
	for _, id := range []int64{1, 2} {
		wantWorkItem(t, s, id, 0)
	}
}
