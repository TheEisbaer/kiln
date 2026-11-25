package store

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// limits are the limits of the tests of this file.
var limits = Limits{Repos: map[string]int{"kiln": 1, "second": 2}}

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
		if c := startIn(t, s, Limits{}, "kiln", "c"); c.State != (State{Spec, Run}) {
			t.Fatalf("card %d is in %v, want spec waiting for a run", i+1, c.State)
		}
	}
	c := startIn(t, s, Limits{}, "kiln", "fourth")
	if e := lastOf(t, s, c.ID); c.State != queued || e.Reason != "waits for room in its repo kiln, whose limit is 3 cards in progress" {
		t.Errorf("the fourth card is in %v with %q, want queued under the limit of 3", c.State, e.Reason)
	}
	if got := (Limits{}).Of("kiln"); got != DefaultLimit {
		t.Errorf("Of the zero Limits is %d, want %d", got, DefaultLimit)
	}
}

// instanceWords is the reason of a card queued for the instance's limit of 2.
const instanceWords = "waits for room in the instance, whose limit is 2 cards in progress that are not ready to merge"

// wantQueued fails the test unless the card is queued with the reason.
func wantQueued(t *testing.T, s *Store, c Card, reason string) {
	t.Helper()
	if e := lastOf(t, s, c.ID); c.State != queued || e.Reason != reason || e.Note() != "" {
		t.Errorf("card %d is in %v with %q, note %q; want queued with %q", c.ID, c.State, e.Reason, e.Note(), reason)
	}
}

// TestLimitInstance has the instance at its limit of 2, with a card of each
// repo in progress and room in both repos: a start of either repo is queued
// for the instance, and once one of the two is merged the rule starts the
// first queued card, which says the instance has room.
func TestLimitInstance(t *testing.T) {
	s := open(t)
	l := Limits{Repos: map[string]int{"kiln": 3, "second": 3}, Instance: 2}
	full := busy(t, s, "kiln", State{Spec, Approval})
	busy(t, s, "second", State{Implement, Run})
	k := startIn(t, s, l, "kiln", "k")
	wantQueued(t, s, k, instanceWords)
	sc := startIn(t, s, l, "second", "s")
	wantQueued(t, s, sc, instanceWords)
	if moved, wrote := releaseWith(t, s, l); moved != nil || wrote != 0 {
		t.Errorf("with the instance full, Release moved %q and wrote %d entries", moved, wrote)
	}
	merged(t, s, full)
	want := fmt.Sprintf("%d spec/run: the instance has room under its limit of 2", k.ID)
	if moved, wrote := releaseWith(t, s, l); !reflect.DeepEqual(moved, []string{want}) || wrote != 1 {
		t.Errorf("Release moved %q and wrote %d entries, want %q", moved, wrote, want)
	}
	wantState(t, s, sc.ID, queued)
}

// TestLimitInstanceReadyToMerge has a kiln card at the Git host, with kiln
// and the instance at a limit of 1: it holds its repo, so a kiln card is
// queued for the repo, but not the instance, so a card of the second repo
// starts.
func TestLimitInstanceReadyToMerge(t *testing.T) {
	s := open(t)
	l := Limits{Repos: map[string]int{"kiln": 1}, Instance: 1}
	busy(t, s, "kiln", atHost)
	k := startIn(t, s, l, "kiln", "k")
	wantQueued(t, s, k, "waits for room in its repo kiln, whose limit is 1 cards in progress")
	if c := startIn(t, s, l, "second", "s"); c.State != (State{Spec, Run}) {
		t.Errorf("the second repo's card is in %v, want spec waiting for a run", c.State)
	}
}

// TestLimitInstanceFullRepo has kiln at its limit, K queued for it: K takes
// no place in the instance, so S of the second repo, started after K, goes
// to spec, by the developer's start and, once it was queued for the
// instance, by the rule.
func TestLimitInstanceFullRepo(t *testing.T) {
	s := open(t)
	l := Limits{Repos: map[string]int{"kiln": 1, "second": 3}, Instance: 2}
	busy(t, s, "kiln", State{Spec, Approval})
	k := startIn(t, s, l, "kiln", "k")
	wantQueued(t, s, k, "waits for room in its repo kiln, whose limit is 1 cards in progress")
	sc := startIn(t, s, l, "second", "s")
	if sc.State != (State{Spec, Run}) {
		t.Fatalf("S is in %v, want spec waiting for a run", sc.State)
	}
	s2 := startIn(t, s, l, "second", "s2")
	wantQueued(t, s, s2, instanceWords)
	if _, err := s.Decide(sc.ID, Drop, ""); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%d spec/run: the instance has room under its limit of 2", s2.ID)
	if moved, wrote := releaseWith(t, s, l); !reflect.DeepEqual(moved, []string{want}) || wrote != 1 {
		t.Errorf("Release moved %q and wrote %d entries, want %q", moved, wrote, want)
	}
	wantState(t, s, k.ID, queued)
}

// TestLimitInstanceStartBehindQueue has a place in the instance free while S
// of the second repo waits for room: a kiln card the developer starts before
// the rule runs is queued behind S, which the rule then starts.
func TestLimitInstanceStartBehindQueue(t *testing.T) {
	s := open(t)
	l := Limits{Repos: map[string]int{"kiln": 3, "second": 3}, Instance: 1}
	full := busy(t, s, "kiln", State{Spec, Approval})
	sc := startIn(t, s, l, "second", "s")
	merged(t, s, full)
	k := startIn(t, s, l, "kiln", "k")
	wantQueued(t, s, k, "waits for room in the instance, whose limit is 1 cards in progress that are not ready to merge")
	want := fmt.Sprintf("%d spec/run: the instance has room under its limit of 1", sc.ID)
	if moved, _ := releaseWith(t, s, l); !reflect.DeepEqual(moved, []string{want}) {
		t.Errorf("Release moved %q, want %q", moved, want)
	}
	wantState(t, s, k.ID, queued)
}

// TestLimitInstanceRenamed has K queued for its repo, and S of the second
// repo queued for the instance after it. Once K's repo has room while the
// instance has none, the rule writes one entry on K that names the instance,
// with K's since as it was, and repeats write nothing. The next place starts
// K, not S.
func TestLimitInstanceRenamed(t *testing.T) {
	s := open(t)
	l := Limits{Repos: map[string]int{"kiln": 1, "second": 3}, Instance: 2}
	free := busy(t, s, "second", State{Spec, Approval})
	busy(t, s, "second", State{Spec, Approval})
	host := busy(t, s, "kiln", atHost)
	k := startIn(t, s, l, "kiln", "k")
	wantQueued(t, s, k, "waits for room in its repo kiln, whose limit is 1 cards in progress")
	sc := startIn(t, s, l, "second", "s")
	wantQueued(t, s, sc, instanceWords)
	since := func() string {
		var at string
		if err := s.db.QueryRow(`SELECT since FROM cards WHERE id = ?`, k.ID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	before := since()

	merged(t, s, host)
	want := fmt.Sprintf("%d backlog/room: %s", k.ID, instanceWords)
	if moved, wrote := releaseWith(t, s, l); !reflect.DeepEqual(moved, []string{want}) || wrote != 1 {
		t.Errorf("Release moved %q and wrote %d entries, want %q", moved, wrote, want)
	}
	if e := lastOf(t, s, k.ID); e.Cause != CauseRule || e.State != queued || e.Note() != "" {
		t.Errorf("K's last entry is %+v, want the rule's, queued", e)
	}
	if after := since(); after != before {
		t.Errorf("K's since is %s, want it kept at %s", after, before)
	}
	for i := 0; i < 2; i++ {
		if moved, wrote := releaseWith(t, s, l); moved != nil || wrote != 0 {
			t.Errorf("repeat %d moved %q and wrote %d entries", i+1, moved, wrote)
		}
	}

	merged(t, s, free)
	want = fmt.Sprintf("%d spec/run: the instance has room under its limit of 2", k.ID)
	if moved, wrote := releaseWith(t, s, l); !reflect.DeepEqual(moved, []string{want}) || wrote != 1 {
		t.Errorf("Release moved %q and wrote %d entries, want %q", moved, wrote, want)
	}
	wantState(t, s, sc.ID, queued)
}

// TestLimitInstanceDefault has the zero Limits and two repos: two cards of
// each are started, and the fourth is queued for the instance's limit of 3.
func TestLimitInstanceDefault(t *testing.T) {
	s := open(t)
	var last Card
	for _, repo := range []string{"kiln", "kiln", "second", "second"} {
		last = startIn(t, s, Limits{}, repo, "c")
	}
	wantQueued(t, s, last, "waits for room in the instance, whose limit is 3 cards in progress that are not ready to merge")
	if got := (Limits{}).OfInstance(); got != DefaultLimit {
		t.Errorf("OfInstance of the zero Limits is %d, want %d", got, DefaultLimit)
	}
}

// TestCountsForInstance counts every state in spec, implement and review
// against the instance but ready to merge, as the SQL condition does.
func TestCountsForInstance(t *testing.T) {
	s := open(t)
	for st := range valid {
		id := createIn(t, s, "kiln", "c").ID
		if _, err := s.db.Exec(`UPDATE cards SET stage = ?, waits_for = ? WHERE id = ?`, st.Stage, st.WaitsFor, id); err != nil {
			t.Fatal(err)
		}
		var sql bool
		if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM cards WHERE id = ? AND `+forInstance+`)`, id).Scan(&sql); err != nil {
			t.Fatal(err)
		}
		want := st.Stage != Backlog && st.Stage != Done && st != atHost
		if got := CountsForInstance(st); got != want || sql != want {
			t.Errorf("%v counts: Go %v, SQL %v; want %v", st, got, sql, want)
		}
	}
}

// TestLimitPastUnreadableSince has a card queued for room whose since cannot
// be read: it takes no place in the queue, and no other card's release, start
// or moves fail for it.
func TestLimitPastUnreadableSince(t *testing.T) {
	s := open(t)
	busy(t, s, "kiln", State{Spec, Run})
	x := startIn(t, s, limits, "kiln", "x")
	wantState(t, s, x.ID, queued)
	p := busy(t, s, "second", State{Spec, Run})
	y := startIn(t, s, limits, "second", "y", p)
	merged(t, s, p)
	badSince(t, s, x.ID)

	got, err := s.Release(limits)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("card %d:", x.ID)) {
		t.Errorf("Release returned the error %v", err)
	}
	if len(got) != 1 || got[0].ID != y.ID || got[0].State != (State{Spec, Run}) {
		t.Errorf("Release returned %+v, want card %d released", got, y.ID)
	}
	n := createIn(t, s, "second", "new")
	if _, err := s.StartWith(n.ID, limits); err != nil {
		t.Errorf("StartWith: %v", err)
	}
	b := createIn(t, s, "second", "backlog")
	if _, err := s.Moves(b, false); err != nil {
		t.Errorf("Moves: %v", err)
	}
}
