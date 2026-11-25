package store

import (
	"fmt"
	"reflect"
	"testing"
)

// limits are the limits of the tests of this file.
var limits = Limits{"kiln": 1, "second": 2}

// createIn writes a card of the repo, with its predecessors.
func createIn(t *testing.T, s *Store, repo, text string, predecessors ...int64) Card {
	t.Helper()
	c, err := s.CreateIn(repo, text, 0, predecessors...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// startIn writes a card of the repo and starts it under the limits.
func startIn(t *testing.T, s *Store, l Limits, repo, text string, predecessors ...int64) Card {
	t.Helper()
	c, err := s.StartWith(createIn(t, s, repo, text, predecessors...).ID, l)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// busy writes a card of the repo in the state, by the rule.
func busy(t *testing.T, s *Store, repo string, st State) int64 {
	t.Helper()
	id := createIn(t, s, repo, "in progress").ID
	mustMove(t, s, id, st)
	return id
}

// wantState fails the test unless the card is in the state.
func wantState(t *testing.T, s *Store, id int64, want State) {
	t.Helper()
	c, err := s.Card(id)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != want {
		t.Errorf("card %d is in %v, want %v", id, c.State, want)
	}
}

// lastOf is a card's last entry.
func lastOf(t *testing.T, s *Store, id int64) Entry {
	t.Helper()
	h := mustHistory(t, s, id)
	return h[len(h)-1]
}

// TestLimitPerRepo has kiln at its limit of 1: a start of a kiln card queues
// it for room, with the store's words and no note, while the second repo
// still starts two cards.
func TestLimitPerRepo(t *testing.T) {
	s := open(t)
	busy(t, s, "kiln", State{Spec, Approval})
	k := startIn(t, s, limits, "kiln", "k")
	if k.State != queued {
		t.Fatalf("the kiln card is in %v, want queued", k.State)
	}
	e := lastOf(t, s, k.ID)
	if want := "waits for room in its repo kiln, whose limit is 1 cards in progress"; e.Cause != CauseDeveloper || e.Reason != want || e.Note() != "" {
		t.Errorf("the start's entry is %+v, with the note %q; want the reason %q", e, e.Note(), want)
	}
	for _, text := range []string{"s1", "s2"} {
		if c := startIn(t, s, limits, "second", text); c.State != (State{Spec, Run}) {
			t.Errorf("the second repo's card %s is in %v, want spec waiting for a run", text, c.State)
		}
	}
}

// TestLimitReadyToMerge counts a card at the Git host against its repo; once
// it is merged, the rule starts the queued card.
func TestLimitReadyToMerge(t *testing.T) {
	s := open(t)
	host := busy(t, s, "kiln", atHost)
	k := startIn(t, s, limits, "kiln", "k")
	if k.State != queued {
		t.Fatalf("with a card ready to merge, the kiln card is in %v, want queued", k.State)
	}
	merged(t, s, host)
	want := fmt.Sprintf("%d spec/run: its repo kiln has room under its limit of 1", k.ID)
	if moved, wrote := releaseWith(t, s, limits); !reflect.DeepEqual(moved, []string{want}) || wrote != 1 {
		t.Errorf("Release moved %q and wrote %d entries, want %q", moved, wrote, want)
	}
	if e := lastOf(t, s, k.ID); e.Cause != CauseRule {
		t.Errorf("the start's entry is %+v, want the rule's", e)
	}
}

// TestLimitFirstWithRoom has both repos at their limits, a kiln card queued
// before a second one: once the second repo has room, the rule starts its
// card and writes nothing on the kiln card, and repeats write nothing.
func TestLimitFirstWithRoom(t *testing.T) {
	s := open(t)
	busy(t, s, "kiln", State{Spec, Run})
	done := busy(t, s, "second", State{Implement, Run})
	busy(t, s, "second", State{Review, Developer})
	k := startIn(t, s, limits, "kiln", "k")
	sc := startIn(t, s, limits, "second", "s")
	if k.State != queued || sc.State != queued {
		t.Fatalf("the cards are in %v and %v, want both queued", k.State, sc.State)
	}
	if moved, wrote := releaseWith(t, s, limits); moved != nil || wrote != 0 {
		t.Errorf("with both repos full, Release moved %q and wrote %d entries", moved, wrote)
	}
	kHistory := len(mustHistory(t, s, k.ID))
	merged(t, s, done)
	want := fmt.Sprintf("%d spec/run: its repo second has room under its limit of 2", sc.ID)
	if moved, wrote := releaseWith(t, s, limits); !reflect.DeepEqual(moved, []string{want}) || wrote != 1 {
		t.Errorf("Release moved %q and wrote %d entries, want %q", moved, wrote, want)
	}
	wantState(t, s, k.ID, queued)
	if n := len(mustHistory(t, s, k.ID)); n != kHistory {
		t.Errorf("the kiln card has %d entries, want %d", n, kHistory)
	}
	for i := 0; i < 2; i++ {
		if moved, wrote := releaseWith(t, s, limits); moved != nil || wrote != 0 {
			t.Errorf("repeat %d moved %q and wrote %d entries", i+1, moved, wrote)
		}
	}
}

// TestLimitOrder has a kiln card P queued for a predecessor before a kiln
// card R is queued for room. Once P's predecessor is merged, P gets the
// rule's entry that it waits for room and joins the queue after R: the next
// place starts R, the one after P. The times are written so that they sort
// the other way as strings.
func TestLimitOrder(t *testing.T) {
	s := open(t)
	full := busy(t, s, "kiln", State{Spec, Approval})
	pred := busy(t, s, "second", State{Spec, Approval})
	p := startIn(t, s, limits, "kiln", "p", pred)
	r := startIn(t, s, limits, "kiln", "r")
	if e := lastOf(t, s, p.ID); p.State != queued || e.Reason != fmt.Sprintf("%s#%d", QueuedPrefix, pred) {
		t.Fatalf("P is in %v with %q, want queued for its predecessor", p.State, e.Reason)
	}
	if e := lastOf(t, s, r.ID); r.State != queued || e.Reason != roomReason("kiln", limits) {
		t.Fatalf("R is in %v with %q, want queued for room", r.State, e.Reason)
	}

	merged(t, s, pred)
	want := fmt.Sprintf("%d backlog/room: waits for room in its repo kiln, whose limit is 1 cards in progress; its predecessors are done: #%d", p.ID, pred)
	if moved, wrote := releaseWith(t, s, limits); !reflect.DeepEqual(moved, []string{want}) || wrote != 1 {
		t.Errorf("Release moved %q and wrote %d entries, want %q", moved, wrote, want)
	}
	if e := lastOf(t, s, p.ID); e.Cause != CauseRule || e.State != queued {
		t.Errorf("P's last entry is %+v, want the rule's, queued", e)
	}
	for i := 0; i < 2; i++ {
		if moved, wrote := releaseWith(t, s, limits); moved != nil || wrote != 0 {
			t.Errorf("repeat %d moved %q and wrote %d entries", i+1, moved, wrote)
		}
	}

	// R before P by time, P before R as strings.
	for id, since := range map[int64]string{r.ID: "2026-11-25T10:00:05.1Z", p.ID: "2026-11-25T10:00:05.12Z"} {
		if _, err := s.db.Exec(`UPDATE cards SET since = ? WHERE id = ?`, since, id); err != nil {
			t.Fatal(err)
		}
	}
	merged(t, s, full)
	want = fmt.Sprintf("%d spec/run: its repo kiln has room under its limit of 1", r.ID)
	if moved, wrote := releaseWith(t, s, limits); !reflect.DeepEqual(moved, []string{want}) || wrote != 1 {
		t.Errorf("Release moved %q and wrote %d entries, want %q", moved, wrote, want)
	}
	wantState(t, s, p.ID, queued)

	merged(t, s, r.ID)
	want = fmt.Sprintf("%d spec/run: its repo kiln has room under its limit of 1", p.ID)
	if moved, wrote := releaseWith(t, s, limits); !reflect.DeepEqual(moved, []string{want}) || wrote != 1 {
		t.Errorf("Release moved %q and wrote %d entries, want %q", moved, wrote, want)
	}
}

// TestLimitStartBehindQueue has a place free while R waits for room: a card
// the developer starts before the rule runs is queued behind R, which the
// rule then starts.
func TestLimitStartBehindQueue(t *testing.T) {
	s := open(t)
	full := busy(t, s, "kiln", State{Spec, Approval})
	r := startIn(t, s, limits, "kiln", "r")
	merged(t, s, full)
	c := startIn(t, s, limits, "kiln", "c")
	if c.State != queued {
		t.Fatalf("C is in %v, want queued behind R", c.State)
	}
	want := fmt.Sprintf("%d spec/run: its repo kiln has room under its limit of 1", r.ID)
	if moved, _ := releaseWith(t, s, limits); !reflect.DeepEqual(moved, []string{want}) {
		t.Errorf("Release moved %q, want %q", moved, want)
	}
	wantState(t, s, c.ID, queued)
}

// TestLimitDroppedPredecessor has a card whose predecessor was dropped: the
// developer starts it without it while its repo is full, and it waits for
// room, not for the developer, until a place frees.
func TestLimitDroppedPredecessor(t *testing.T) {
	s := open(t)
	pred := createIn(t, s, "second", "pred").ID
	d := startIn(t, s, limits, "kiln", "d", pred)
	if _, err := s.Decide(pred, Drop, ""); err != nil {
		t.Fatal(err)
	}
	if releaseWith(t, s, limits); lastOf(t, s, d.ID).State != (State{Backlog, Developer}) {
		t.Fatalf("with its predecessor dropped, D is in %v, want the backlog waiting for the developer", lastOf(t, s, d.ID).State)
	}
	full := busy(t, s, "kiln", State{Spec, Approval})
	if c, err := s.StartWith(d.ID, limits); err != nil || c.State != queued {
		t.Fatalf("the start: %+v, %v; want queued", c, err)
	}
	if moved, wrote := releaseWith(t, s, limits); moved != nil || wrote != 0 {
		t.Errorf("with its repo full, Release moved %q and wrote %d entries", moved, wrote)
	}
	wantState(t, s, d.ID, queued)
	merged(t, s, full)
	want := fmt.Sprintf("%d spec/run: its repo kiln has room under its limit of 1", d.ID)
	if moved, _ := releaseWith(t, s, limits); !reflect.DeepEqual(moved, []string{want}) {
		t.Errorf("Release moved %q, want %q", moved, want)
	}
}

// TestLimitDefault has no limits: three cards start and the fourth is queued
// under the default.
func TestLimitDefault(t *testing.T) {
	s := open(t)
	for i := 0; i < 3; i++ {
		if c := startIn(t, s, nil, "kiln", "c"); c.State != (State{Spec, Run}) {
			t.Fatalf("card %d is in %v, want spec waiting for a run", i+1, c.State)
		}
	}
	c := startIn(t, s, nil, "kiln", "fourth")
	if e := lastOf(t, s, c.ID); c.State != queued || e.Reason != "waits for room in its repo kiln, whose limit is 3 cards in progress" {
		t.Errorf("the fourth card is in %v with %q, want queued under the limit of 3", c.State, e.Reason)
	}
	if got := (Limits(nil)).Of("kiln"); got != DefaultLimit {
		t.Errorf("Of a nil Limits is %d, want %d", got, DefaultLimit)
	}
}
