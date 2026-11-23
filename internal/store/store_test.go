package store

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The variables by which the test runs its own binary as the child of TestKill.
// Only TestMain reads them.
const (
	childVar = "KILN_STORE_TEST_CHILD" // what the child does, see asChild
	pathVar  = "KILN_STORE_TEST_PATH"  // the store, or a directory of stores
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(childVar); mode != "" {
		asChild(mode, os.Getenv(pathVar))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The valid pairings, written out here and not read from the code, so that a
// pairing dropped from the code fails the test.
var validPairings = map[State]bool{
	{"backlog", "developer"}: true, {"backlog", "room"}: true,
	{"spec", "run"}: true, {"spec", "approval"}: true, {"spec", "developer"}: true,
	{"implement", "run"}: true, {"implement", "developer"}: true,
	{"review", "run"}: true, {"review", "developer"}: true, {"review", "host"}: true, {"review", "approval"}: true,
	{"done", "nothing"}: true,
}

// open opens a new store in the test's directory.
func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, text string) Card {
	t.Helper()
	c, err := s.Create(text)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustHistory(t *testing.T, s *Store, id int64) []Entry {
	t.Helper()
	h, err := s.History(id)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestCardLife(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	if keep := os.Getenv("KILN_STORE_KEEP"); keep != "" {
		if _, err := os.Stat(keep); err == nil {
			t.Fatalf("KILN_STORE_KEEP: %s exists; remove it first", keep)
		}
		path = keep
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	start := time.Now()
	card := mustCreate(t, s, "a card")
	steps := []struct {
		to    State
		cause Cause
	}{
		{State{Spec, Run}, CauseRule},
		{State{Spec, Approval}, CauseRun},
		{State{Implement, Run}, CauseDeveloper},
		{State{Review, Run}, CauseRun},
		{State{Review, Host}, CauseRule},
		{State{Done, Nothing}, CauseHost},
	}
	want := []Entry{{Card: card.ID, Cause: CauseDeveloper, State: State{Backlog, Developer}}}
	for _, st := range steps {
		got, err := s.Move(card.ID, st.to, st.cause)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != st.to || got.Text != "a card" {
			t.Fatalf("Move returned %+v", got)
		}
		want = append(want, Entry{Card: card.ID, Cause: st.cause, State: st.to})
	}
	end := time.Now()

	got, err := s.Card(card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != (State{Done, Nothing}) {
		t.Errorf("the card is %+v, want done waiting for nothing", got)
	}
	history := mustHistory(t, s, card.ID)
	if len(history) != 7 {
		t.Fatalf("History has %d entries, want 7: %+v", len(history), history)
	}
	var last int64
	for i, e := range history {
		t.Logf("entry %d: %s/%s by %s at %s", e.Number, e.Stage, e.WaitsFor, e.Cause, e.At.Format(time.RFC3339Nano))
		if e.Card != want[i].Card || e.Cause != want[i].Cause || e.State != want[i].State {
			t.Errorf("entry %d is %+v, want the card's %v by %s", i, e, want[i].State, want[i].Cause)
		}
		if e.At.Before(start.Truncate(0)) || e.At.After(end) {
			t.Errorf("entry %d is at %s, outside %s to %s", i, e.At, start, end)
		}
		if e.At.Location() != time.UTC {
			t.Errorf("entry %d is not in UTC: %s", i, e.At.Location())
		}
		if e.Number <= last {
			t.Errorf("entry %d has number %d after %d", i, e.Number, last)
		}
		last = e.Number
	}
}

func TestPairings(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	stages := []Stage{Backlog, Spec, Implement, Review, Done}
	waits := []WaitsFor{Developer, Room, Run, Approval, Host, Nothing}

	combos, written := 0, 0
	for _, stage := range stages {
		for _, w := range waits {
			combos++
			to := State{stage, w}
			beforeCard, err := s.Card(card.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeHistory := mustHistory(t, s, card.ID)

			got, err := s.Move(card.ID, to, CauseRule)
			if validPairings[to] {
				if err != nil {
					t.Errorf("%v: %v", to, err)
					continue
				}
				if got.State != to {
					t.Errorf("%v: Move returned %v", to, got.State)
				}
				if to != beforeCard.State { // the first one is the card's own state
					written++
					if h := mustHistory(t, s, card.ID); len(h) != len(beforeHistory)+1 {
						t.Errorf("%v: %d entries, want %d", to, len(h), len(beforeHistory)+1)
					}
				}
				continue
			}
			if err == nil {
				t.Errorf("%v: no error for a pairing that cannot be written", to)
			}
			afterCard, _ := s.Card(card.ID)
			if afterCard != beforeCard || !reflect.DeepEqual(mustHistory(t, s, card.ID), beforeHistory) {
				t.Errorf("%v: refused, and yet the card or its history changed", to)
			}
		}
	}
	if combos != 30 {
		t.Errorf("tried %d combinations, want 30", combos)
	}
	if h := mustHistory(t, s, card.ID); len(h) != 1+written || written != len(validPairings)-1 {
		t.Errorf("%d entries and %d moves written, want all %d valid pairings written", len(h), written, len(validPairings))
	}
}

func TestCauses(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	for _, cause := range []Cause{"", "someone"} {
		if _, err := s.Move(card.ID, State{Spec, Run}, cause); err == nil {
			t.Errorf("cause %q: no error", cause)
		}
	}
	if got, _ := s.Card(card.ID); got.State != (State{Backlog, Developer}) {
		t.Errorf("a refused move changed the card to %v", got.State)
	}
	if h := mustHistory(t, s, card.ID); len(h) != 1 {
		t.Errorf("a refused move left %d entries, want 1", len(h))
	}
}

func TestCreateEmpty(t *testing.T) {
	s := open(t)
	if _, err := s.Create(""); err == nil {
		t.Fatal("an empty text was accepted")
	}
	if _, err := s.Card(1); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Card(1) after a refused Create: %v", err)
	}
}

func TestRepeatedMove(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	to := State{Spec, Run}
	first, err := s.Move(card.ID, to, CauseRule)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Move(card.ID, to, CauseDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Errorf("the repeat returned %+v, want %+v", again, first)
	}
	h := mustHistory(t, s, card.ID)
	if len(h) != 2 || h[1].Cause != CauseRule {
		t.Errorf("history after a repeat: %+v, want 2 entries, the second by the rule", h)
	}
}

func TestNewerStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 11"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("a store of version 11 was opened")
	}
	if msg := err.Error(); !strings.Contains(msg, "version 11") || !strings.Contains(msg, "version 10") {
		t.Errorf("the message %q does not name both versions", msg)
	}

	db, err = sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v, tables int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if v != 11 || tables != 0 {
		t.Errorf("after the refusal: version %d with %d tables, want 11 and 0", v, tables)
	}
}

func mustAddRun(t *testing.T, s *Store, r RunRow) {
	t.Helper()
	if err := s.AddRun(r, 8); err != nil {
		t.Fatal(err)
	}
}

func theRun(card int64, id string) RunRow {
	return RunRow{ID: id, Card: card, Commit: "c0ffee", Model: "claude-haiku-5-5", Effort: "low"}
}

func TestInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Instance()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(id) {
		t.Errorf("the instance is %q, want eight hex digits", id)
	}
	s.Close()
	s = reopen(t, path)
	if again, err := s.Instance(); err != nil || again != id {
		t.Errorf("after a reopen the instance is %q, %v, want %q", again, err, id)
	}
	if n := countRows(t, s, "instance"); n != 1 {
		t.Errorf("%d rows in instance, want 1", n)
	}
	if other, _ := open(t).Instance(); other == id {
		t.Errorf("two stores have the instance %q", id)
	}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestWaiting lists the cards that wait for a run in the order the tick takes
// them: review, then implement, then spec, and in each stage the card that
// has waited longest first, by when it got its state.
func TestWaiting(t *testing.T) {
	s := open(t)
	a, b, c, d, e := mustCreate(t, s, "a"), mustCreate(t, s, "b"), mustCreate(t, s, "c"), mustCreate(t, s, "d"), mustCreate(t, s, "e")
	if ids, err := s.Waiting(); err != nil || len(ids) != 0 {
		t.Fatalf("Waiting with every card in the backlog: %v, %v", ids, err)
	}
	move := func(card Card, to State) {
		t.Helper()
		if _, err := s.Move(card.ID, to, CauseDeveloper); err != nil {
			t.Fatal(err)
		}
	}
	move(c, State{Spec, Run})
	move(a, State{Spec, Run})
	move(b, State{Spec, Approval})
	move(d, State{Implement, Run})
	move(e, State{Review, Run})
	if ids, err := s.Waiting(); err != nil || !reflect.DeepEqual(ids, []int64{e.ID, d.ID, c.ID, a.ID}) {
		t.Errorf("Waiting is %v, %v, want review, implement, then the spec that waited longest: %v", ids, err, []int64{e.ID, d.ID, c.ID, a.ID})
	}
	// A move to the state a card has writes nothing, and the card keeps its
	// place; one that leaves the state and comes back waits from its return.
	move(c, State{Spec, Run})
	if ids, _ := s.Waiting(); !reflect.DeepEqual(ids, []int64{e.ID, d.ID, c.ID, a.ID}) {
		t.Errorf("after a repeated move Waiting is %v", ids)
	}
	move(c, State{Spec, Developer})
	move(c, State{Spec, Run})
	if ids, _ := s.Waiting(); !reflect.DeepEqual(ids, []int64{e.ID, d.ID, a.ID, c.ID}) {
		t.Errorf("after card %d came back Waiting is %v", c.ID, ids)
	}
	// since is the time of the entry that wrote the state.
	h := mustHistory(t, s, c.ID)
	var since string
	if err := s.db.QueryRow("SELECT since FROM cards WHERE id = ?", c.ID).Scan(&since); err != nil ||
		since != h[len(h)-1].At.Format(time.RFC3339Nano) {
		t.Errorf("card %d has its state since %q (%v), and its last entry is of %s", c.ID, since, err, h[len(h)-1].At)
	}
}

// TestAddRunLimit refuses a run that would make more runs in flight than the
// limit, and writes the same row again at the limit, since it adds none.
func TestAddRunLimit(t *testing.T) {
	s := open(t)
	var cards []Card
	for _, text := range []string{"a", "b", "c"} {
		card := mustCreate(t, s, text)
		if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		cards = append(cards, card)
	}
	for i, card := range cards[:2] {
		if err := s.AddRun(theRun(card.ID, fmt.Sprintf("r%d", i+1)), 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddRun(theRun(cards[2].ID, "r3"), 2); !errors.Is(err, ErrLimit) {
		t.Errorf("a third run under a limit of 2: %v", err)
	}
	if err := s.AddRun(theRun(cards[0].ID, "r1"), 2); err != nil {
		t.Errorf("the same row again at the limit: %v", err)
	}
	if runs, err := s.InFlightRuns(); err != nil || !reflect.DeepEqual(runs, map[int64]string{cards[0].ID: "r1", cards[1].ID: "r2"}) {
		t.Errorf("InFlightRuns is %v, %v", runs, err)
	}
	if _, err := s.EndRun("r1", Finished, "", State{Spec, Approval}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRun(theRun(cards[2].ID, "r3"), 2); err != nil {
		t.Errorf("a run after one has ended: %v", err)
	}
}

func TestRuns(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	other := mustCreate(t, s, "another")
	first := theRun(card.ID, "ab12cd34-1-1")
	// A run is written only for a card that waits for one.
	for _, c := range []Card{card, other} {
		if _, err := s.Move(c.ID, State{Spec, Run}, CauseDeveloper); err != nil {
			t.Fatal(err)
		}
	}

	if r, err := s.InFlight(card.ID); r != nil || err != nil {
		t.Fatalf("InFlight before any run: %v, %v", r, err)
	}
	if n, err := s.RunCount(card.ID); n != 0 || err != nil {
		t.Fatalf("RunCount before any run: %d, %v", n, err)
	}

	mustAddRun(t, s, first)
	mustAddRun(t, s, first) // the same row again writes nothing
	if n, _ := s.RunCount(card.ID); n != 1 {
		t.Errorf("RunCount after a repeated AddRun is %d, want 1", n)
	}
	if r, err := s.InFlight(card.ID); err != nil || r == nil || *r != first {
		t.Errorf("InFlight is %v, %v, want %+v", r, err, first)
	}
	if got, err := s.Run(first.ID); err != nil || got != first {
		t.Errorf("Run is %+v, %v, want %+v", got, err, first)
	}
	if _, err := s.Run("nothing"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Run of an unknown ID: %v", err)
	}

	// Anything else under the same ID is refused, whichever field differs.
	for name, change := range map[string]func(*RunRow){
		"card":   func(r *RunRow) { r.Card = other.ID },
		"commit": func(r *RunRow) { r.Commit = "beef" },
		"model":  func(r *RunRow) { r.Model = "claude-opus-5-5" },
		"effort": func(r *RunRow) { r.Effort = "high" },
	} {
		r := first
		change(&r)
		if err := s.AddRun(r, 8); err == nil {
			t.Errorf("AddRun with another %s under the same ID was accepted", name)
		}
	}
	// A second run in flight for the card is refused, by the store itself.
	if err := s.AddRun(theRun(card.ID, "ab12cd34-1-2"), 8); err == nil {
		t.Error("a second run in flight for one card was accepted")
	}
	if n, _ := s.RunCount(card.ID); n != 1 {
		t.Errorf("RunCount after the refusals is %d, want 1", n)
	}
	// A run is written in flight, with every field.
	for _, r := range []RunRow{{}, {ID: "x", Card: other.ID}, func() RunRow { r := theRun(other.ID, "x"); r.Ending = Finished; return r }()} {
		if err := s.AddRun(r, 8); err == nil {
			t.Errorf("AddRun of %+v was accepted", r)
		}
	}
	// Another card has a run in flight of its own.
	mustAddRun(t, s, theRun(other.ID, "ab12cd34-2-1"))
}

func TestEndRun(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	run := theRun(card.ID, "ab12cd34-1-1")
	mustAddRun(t, s, run)

	// What cannot be written leaves the run in flight and the card as it was.
	for name, call := range map[string]func() error{
		"an unknown ending": func() error { _, err := s.EndRun(run.ID, "withdrawn", "", State{Spec, Approval}, nil); return err },
		"no ending":         func() error { _, err := s.EndRun(run.ID, "", "", State{Spec, Approval}, nil); return err },
		"an invalid state":  func() error { _, err := s.EndRun(run.ID, Finished, "", State{Done, Run}, nil); return err },
		"an unknown run":    func() error { _, err := s.EndRun("nothing", Finished, "", State{Spec, Approval}, nil); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("EndRun with %s was accepted", name)
		}
	}
	if r, _ := s.InFlight(card.ID); r == nil {
		t.Fatal("a refused EndRun ended the run")
	}
	if got, _ := s.Card(card.ID); got.State != (State{Spec, Run}) || len(mustHistory(t, s, card.ID)) != 2 {
		t.Fatalf("a refused EndRun changed the card: %+v", got)
	}

	moved, err := s.EndRun(run.ID, Finished, "saved abc123", State{Spec, Approval}, nil)
	if err != nil || moved.State != (State{Spec, Approval}) || moved.Text != "a card" {
		t.Fatalf("EndRun returned %+v, %v", moved, err)
	}
	if r, err := s.InFlight(card.ID); r != nil || err != nil {
		t.Errorf("after EndRun the run is in flight: %v, %v", r, err)
	}
	ended := run
	ended.Ending = Finished
	if got, _ := s.Run(run.ID); got != ended {
		t.Errorf("the run is %+v, want %+v", got, ended)
	}
	h := mustHistory(t, s, card.ID)
	if len(h) != 3 {
		t.Fatalf("%d entries, want 3: %+v", len(h), h)
	}
	if e := h[2]; e.Cause != CauseRun || e.State != (State{Spec, Approval}) || e.Run != run.ID || e.Reason != "saved abc123" {
		t.Errorf("the entry is %+v", e)
	}
	if h[0].Run != "" || h[0].Reason != "" || h[1].Run != "" || h[1].Reason != "" {
		t.Errorf("an entry that ends no run has a run or a reason: %+v", h[:2])
	}

	// A repeat, even with other arguments, writes nothing and finds the card.
	again, err := s.EndRun(run.ID, Failed, "other", State{Spec, Developer}, nil)
	if err != nil || again.State != (State{Spec, Approval}) {
		t.Errorf("the repeated EndRun returned %+v, %v", again, err)
	}
	if got, _ := s.Run(run.ID); got != ended || len(mustHistory(t, s, card.ID)) != 3 {
		t.Errorf("the repeated EndRun wrote: %+v with %d entries", got, len(mustHistory(t, s, card.ID)))
	}

	// The ended run no longer holds the card's place: the next run can be written.
	if n, _ := s.RunCount(card.ID); n != 1 {
		t.Errorf("RunCount is %d, want 1", n)
	}
	if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, "ab12cd34-1-2"))
	if n, _ := s.RunCount(card.ID); n != 2 {
		t.Errorf("RunCount is %d, want 2", n)
	}

	// A failed run, with no reason, moves a card to needs you; and a card that
	// is in the state already still gets the run's ending and its entry.
	s2 := open(t)
	c2 := mustCreate(t, s2, "b")
	if _, err := s2.Move(c2.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s2, theRun(c2.ID, "r"))
	if _, err := s2.Move(c2.ID, State{Spec, Developer}, CauseRule); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.EndRun("r", Failed, "", State{Spec, Developer}, nil); err != nil {
		t.Fatal(err)
	}
	if r, _ := s2.Run("r"); r.Ending != Failed {
		t.Errorf("the run is %+v, want failed", r)
	}
	if h := mustHistory(t, s2, c2.ID); len(h) != 4 || h[3].Run != "r" || h[3].Reason != "" {
		t.Errorf("history: %+v", h)
	}
}

func specDoc(s string) *Doc { return &Doc{Kind: KindSpec, Text: s} }

func TestSpec(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Spec(card.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Spec of a card without one: %v", err)
	}

	// Kept exactly as written, a byte that is not UTF-8 and an escape included.
	text := "# Spec\n\nNot UTF-8: \xff, an escape: \x1b]0;title\x07, a return: \r.\n"
	first := RunRow{ID: "ab12cd34-1-1", Card: card.ID, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}
	mustAddRun(t, s, first)
	if _, err := s.EndRun(first.ID, Finished, "the reason", State{Spec, Approval}, specDoc(text)); err != nil {
		t.Fatal(err)
	}
	want := SpecRow{Run: first.ID, Commit: "c0ffee", Text: text}
	if got, err := s.Spec(card.ID); err != nil || got != want {
		t.Errorf("Spec is %+v (%v), want %+v", got, err, want)
	}
	h := mustHistory(t, s, card.ID)
	if e := h[len(h)-1]; e.Run != first.ID || e.State != (State{Spec, Approval}) {
		t.Errorf("the entry is %+v", e)
	}

	// A second EndRun, with another spec, leaves the first.
	if _, err := s.EndRun(first.ID, Finished, "the reason", State{Spec, Approval}, specDoc("another")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Spec(card.ID); err != nil || got != want || countRows(t, s, "specs") != 1 {
		t.Errorf("after a second EndRun the spec is %+v (%v), with %d rows", got, err, countRows(t, s, "specs"))
	}

	// A later run's spec is the newest; a run ended without one adds none.
	if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	second := RunRow{ID: "ab12cd34-1-2", Card: card.ID, Commit: "beef", Model: "claude-opus-5-5", Effort: "high"}
	mustAddRun(t, s, second)
	if _, err := s.EndRun(second.ID, Finished, "", State{Spec, Developer}, specDoc("the second")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Spec(card.ID); got != (SpecRow{Run: second.ID, Commit: "beef", Text: "the second"}) {
		t.Errorf("the newest spec is %+v", got)
	}
	if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, "ab12cd34-1-3"))
	if _, err := s.EndRun("ab12cd34-1-3", Failed, "exit 1", State{Spec, Developer}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Spec(card.ID); got.Run != second.ID || countRows(t, s, "specs") != 2 {
		t.Errorf("after a run without a spec the newest is %+v, with %d rows", got, countRows(t, s, "specs"))
	}

	// Another card's spec is its own.
	other := mustCreate(t, s, "another")
	if _, err := s.Spec(other.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Spec of another card: %v", err)
	}
}

// schemaV2 is the store of version 2 as card 8's code on main makes it, with
// two cards, their entries and three runs, one of them in flight.
const schemaV2 = `
CREATE TABLE cards (
	id INTEGER PRIMARY KEY,
	text TEXT NOT NULL,
	stage TEXT NOT NULL,
	waits_for TEXT NOT NULL
) STRICT;
CREATE TABLE entries (
	number INTEGER PRIMARY KEY,
	card INTEGER NOT NULL,
	at TEXT NOT NULL,
	cause TEXT NOT NULL,
	stage TEXT NOT NULL,
	waits_for TEXT NOT NULL
, run TEXT, reason TEXT) STRICT;
CREATE TABLE instance (id TEXT NOT NULL) STRICT;
CREATE TABLE runs (
	id TEXT PRIMARY KEY,
	card INTEGER NOT NULL,
	commit_id TEXT NOT NULL,
	model TEXT NOT NULL,
	effort TEXT NOT NULL,
	ending TEXT
) STRICT;
CREATE UNIQUE INDEX runs_in_flight ON runs(card) WHERE ending IS NULL;
PRAGMA user_version = 2;
INSERT INTO instance (id) VALUES ('ab12cd34');
INSERT INTO cards (text, stage, waits_for) VALUES ('first card', 'spec', 'approval'), ('second card', 'spec', 'run');
INSERT INTO entries (card, at, cause, stage, waits_for, run, reason) VALUES
	(1, '2026-11-18T10:00:00Z', 'developer', 'backlog', 'developer', NULL, NULL),
	(1, '2026-11-18T10:00:01Z', 'developer', 'spec', 'run', NULL, NULL),
	(1, '2026-11-18T10:05:00Z', 'run', 'spec', 'developer', 'ab12cd34-1-1', 'exit 1, no result, no change'),
	(1, '2026-11-18T10:06:00Z', 'developer', 'spec', 'run', NULL, NULL),
	(1, '2026-11-18T10:09:00Z', 'run', 'spec', 'approval', 'ab12cd34-1-2', 'exit 0, saved c0ffee'),
	(2, '2026-11-18T11:00:00Z', 'developer', 'backlog', 'developer', NULL, NULL),
	(2, '2026-11-18T11:00:01Z', 'developer', 'spec', 'run', NULL, NULL);
INSERT INTO runs (id, card, commit_id, model, effort, ending) VALUES
	('ab12cd34-1-1', 1, 'c0ffee', 'claude-opus-5-5', 'high', 'failed'),
	('ab12cd34-1-2', 1, 'c0ffee', 'claude-opus-5-5', 'high', 'finished'),
	('ab12cd34-2-1', 2, 'beef', 'claude-opus-5-5', 'high', NULL);
`

// makeV2 writes a store of version 2 to a new file and returns its path.
func makeV2(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schemaV2); err != nil {
		t.Fatal(err)
	}
	return path
}

// dump is every table of a store, its version and its schema, as text.
func dump(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var b strings.Builder
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, "version %d\n", v)
	for _, q := range []string{
		"SELECT name, sql FROM sqlite_master ORDER BY name",
		"SELECT * FROM instance",
		"SELECT * FROM cards ORDER BY id",
		"SELECT * FROM entries ORDER BY number",
		"SELECT * FROM runs ORDER BY id",
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			// One line per row, the schema's white space collapsed.
			var parts []string
			for _, v := range vals {
				parts = append(parts, strings.Join(strings.Fields(fmt.Sprint(v)), " "))
			}
			fmt.Fprintln(&b, strings.Join(parts, " | "))
		}
		rows.Close()
	}
	return b.String()
}

// withoutSince is a line of a dump without the cards' columns since and
// send_backs, which versions 6 and 10 added, when the dump is of a store moved
// forward: a card's row is the only one of six columns there that does not
// start with a run's ID, and its number of send-backs is 0.
func withoutSince(line string, migrated bool) string {
	if parts := strings.Split(line, " | "); migrated && len(parts) == 6 && !strings.HasPrefix(parts[0], "ab12cd34-") && parts[5] == "0" {
		return strings.Join(parts[:4], " | ")
	}
	return line
}

// assertSince asserts when each card of a store got its state, as the cards'
// column since holds it.
func assertSince(t *testing.T, path string, want map[int64]string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for id, at := range want {
		var since string
		if err := db.QueryRow("SELECT since FROM cards WHERE id = ?", id).Scan(&since); err != nil || since != at {
			t.Errorf("card %d has the state since %q (%v), want %s, the time of its last entry", id, since, err, at)
		}
	}
}

// TestMigrateV2 moves a store of version 2 forward: every card, entry and run
// is kept, and the store has the shape of one made at the current version.
func TestMigrateV2(t *testing.T) {
	path := makeV2(t)

	// schemaV2 is what main's code makes: the shape of a store made by the
	// first two migrations, which are main's and unchanged.
	made := filepath.Join(t.TempDir(), "made.db")
	db, err := sql.Open("sqlite3", made)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, migrate := range migrations[:2] {
		if err := migrate(tx); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	madeDump, v2Dump := dump(t, made), dump(t, path)
	schemaOf := func(d string) string {
		var out []string
		for _, line := range strings.Split(d, "\n") {
			if strings.Contains(line, "CREATE ") {
				out = append(out, line)
			}
		}
		return strings.Join(out, "\n")
	}
	if got, want := schemaOf(v2Dump), schemaOf(madeDump); got != want {
		t.Fatalf("schemaV2 differs from what main's migrations make:\n%s\n%s", got, want)
	}

	before := dump(t, path)
	s := reopen(t, path)
	if v := userVersion(t, s); v != version {
		t.Fatalf("the store is version %d after Open, want %d", v, version)
	}
	after := dump(t, path)
	// Every row is kept, and only the version, the new table and the runs'
	// new column, empty for the runs that were there, differ.
	keep := func(d string, migrated bool) string {
		var out []string
		for _, line := range strings.Split(d, "\n") {
			if migrated && line == "ab12cd34 | <nil>" { // the instance's row, not paused
				line = "ab12cd34"
			}
			if migrated && strings.HasPrefix(line, "ab12cd34-") { // a run's row
				line = strings.TrimSuffix(line, " | <nil>")
			}
			line = withoutSince(line, migrated)
			if !strings.HasPrefix(line, "version ") && !strings.HasPrefix(line, "specs | ") &&
				!strings.HasPrefix(line, "sqlite_autoindex_specs_1 | ") && !strings.HasPrefix(line, "runs | ") &&
				!strings.HasPrefix(line, "cards | ") && !strings.HasPrefix(line, "publishes | ") &&
				!strings.HasPrefix(line, "instance | ") && !strings.HasPrefix(line, "instance_entries | ") {
				out = append(out, line)
			}
		}
		return strings.Join(out, "\n")
	}
	if keep(after, true) != keep(before, false) {
		t.Errorf("the move forward changed what version 2 held:\n%s\nwas\n%s", after, before)
	}
	if id, err := s.Instance(); err != nil || id != "ab12cd34" {
		t.Errorf("the instance is %q, %v", id, err)
	}
	assertSince(t, path, map[int64]string{1: "2026-11-18T10:09:00Z", 2: "2026-11-18T11:00:01Z"})
	if r, err := s.InFlight(2); err != nil || r == nil || r.ID != "ab12cd34-2-1" {
		t.Errorf("the run in flight is %+v, %v", r, err)
	}
	if h := mustHistory(t, s, 1); len(h) != 5 || h[4].Run != "ab12cd34-1-2" || h[4].Reason != "exit 0, saved c0ffee" {
		t.Errorf("the history of card 1: %+v", h)
	}
	if _, err := s.Spec(1); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a card of version 2 has a spec: %v", err)
	}
	// The run in flight ends with its spec.
	if _, err := s.EndRun("ab12cd34-2-1", Finished, "done", State{Spec, Approval}, specDoc("the spec")); err != nil {
		t.Fatal(err)
	}
	if sp, err := s.Spec(2); err != nil || sp != (SpecRow{Run: "ab12cd34-2-1", Commit: "beef", Text: "the spec"}) {
		t.Errorf("the spec is %+v, %v", sp, err)
	}
	if got, want := shape(t, s), shape(t, open(t)); !reflect.DeepEqual(got, want) {
		t.Errorf("a migrated store differs from a new one:\n%v\n%v", got, want)
	}
}

// schemaV3 is the store of version 3 as card 11's code makes it, with a card
// and its spec, a card whose run is in flight, and the spec of a run that
// failed after it, which card 11 never keeps but the shape allows.
const schemaV3 = `
CREATE TABLE cards (
	id INTEGER PRIMARY KEY,
	text TEXT NOT NULL,
	stage TEXT NOT NULL,
	waits_for TEXT NOT NULL
) STRICT;
CREATE TABLE entries (
	number INTEGER PRIMARY KEY,
	card INTEGER NOT NULL,
	at TEXT NOT NULL,
	cause TEXT NOT NULL,
	stage TEXT NOT NULL,
	waits_for TEXT NOT NULL
, run TEXT, reason TEXT) STRICT;
CREATE TABLE instance (id TEXT NOT NULL) STRICT;
CREATE TABLE runs (
	id TEXT PRIMARY KEY,
	card INTEGER NOT NULL,
	commit_id TEXT NOT NULL,
	model TEXT NOT NULL,
	effort TEXT NOT NULL,
	ending TEXT
) STRICT;
CREATE UNIQUE INDEX runs_in_flight ON runs(card) WHERE ending IS NULL;
CREATE TABLE specs (run TEXT PRIMARY KEY, card INTEGER NOT NULL, text TEXT NOT NULL) STRICT;
PRAGMA user_version = 3;
INSERT INTO instance (id) VALUES ('ab12cd34');
INSERT INTO cards (text, stage, waits_for) VALUES ('first card', 'spec', 'approval'), ('second card', 'spec', 'run');
INSERT INTO entries (card, at, cause, stage, waits_for, run, reason) VALUES
	(1, '2026-11-19T10:00:00Z', 'developer', 'backlog', 'developer', NULL, NULL),
	(1, '2026-11-19T10:00:01Z', 'developer', 'spec', 'run', NULL, NULL),
	(1, '2026-11-19T10:05:00Z', 'run', 'spec', 'approval', 'ab12cd34-1-1', 'exit 0, result success, 3 turns, $0.1000, not saved'),
	(2, '2026-11-19T11:00:00Z', 'developer', 'backlog', 'developer', NULL, NULL),
	(2, '2026-11-19T11:00:01Z', 'developer', 'spec', 'run', NULL, NULL);
INSERT INTO runs (id, card, commit_id, model, effort, ending) VALUES
	('ab12cd34-1-1', 1, 'c0ffee', 'claude-opus-5-5', 'high', 'finished'),
	('ab12cd34-2-1', 2, 'beef', 'claude-opus-5-5', 'high', NULL);
INSERT INTO specs (run, card, text) VALUES ('ab12cd34-1-1', 1, '# Spec of the first card' || char(10) || 'Not UTF-8: ' || CAST(X'FF' AS TEXT) || char(10));
`

// TestMigrateV3 moves a store of version 3 forward: every row is kept, its
// specs as specs, and the store has the shape of one made at the current
// version.
func TestMigrateV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV3); err != nil {
		t.Fatal(err)
	}
	var oldSpec string
	if err := db.QueryRow(`SELECT text FROM specs`).Scan(&oldSpec); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// schemaV3 is what card 11's code makes: the shape of a store made by the
	// first three migrations, which are unchanged.
	made := filepath.Join(t.TempDir(), "made.db")
	db, err = sql.Open("sqlite3", made)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, migrate := range migrations[:3] {
		if err := migrate(tx); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	schemaOf := func(d string) string {
		var out []string
		for _, line := range strings.Split(d, "\n") {
			if strings.Contains(line, "CREATE ") {
				out = append(out, line)
			}
		}
		return strings.Join(out, "\n")
	}
	if got, want := schemaOf(dump(t, path)), schemaOf(dump(t, made)); got != want {
		t.Fatalf("schemaV3 differs from what card 11's migrations make:\n%s\n%s", got, want)
	}

	before := dump(t, path)
	s := reopen(t, path)
	if v := userVersion(t, s); v != version {
		t.Fatalf("the store is version %d after Open, want %d", v, version)
	}
	// Every card, entry and run is kept; only the version, the definitions of
	// specs and runs, and the runs' new column, empty for the runs that were
	// there, differ.
	keep := func(d string, migrated bool) string {
		var out []string
		for _, line := range strings.Split(d, "\n") {
			if migrated && line == "ab12cd34 | <nil>" { // the instance's row, not paused
				line = "ab12cd34"
			}
			if migrated && strings.HasPrefix(line, "ab12cd34-") { // a run's row
				line = strings.TrimSuffix(line, " | <nil>")
			}
			line = withoutSince(line, migrated)
			if !strings.HasPrefix(line, "version ") && !strings.HasPrefix(line, "specs | ") && !strings.HasPrefix(line, "runs | ") &&
				!strings.HasPrefix(line, "cards | ") && !strings.HasPrefix(line, "publishes | ") &&
				!strings.HasPrefix(line, "instance | ") && !strings.HasPrefix(line, "instance_entries | ") &&
				!strings.HasPrefix(line, "sqlite_autoindex_specs_1 | ") {
				out = append(out, line)
			}
		}
		return strings.Join(out, "\n")
	}
	if keep(dump(t, path), true) != keep(before, false) {
		t.Errorf("the move forward changed what version 3 held:\n%s\nwas\n%s", dump(t, path), before)
	}
	assertSince(t, path, map[int64]string{1: "2026-11-19T10:05:00Z", 2: "2026-11-19T11:00:01Z"})
	// The spec is kept, byte for byte, as a spec.
	want := SpecRow{Run: "ab12cd34-1-1", Commit: "c0ffee", Text: oldSpec}
	if sp, err := s.Spec(1); err != nil || sp != want || !strings.Contains(sp.Text, "\xff") {
		t.Errorf("the spec of version 3 is %+v (%v), want %+v", sp, err, want)
	}
	if d, err := s.Newest(1, KindSpec, KindReview, KindReport); err != nil || d != (Document{KindSpec, "ab12cd34-1-1", oldSpec, ""}) {
		t.Errorf("the newest document is %+v (%v)", d, err)
	}
	// The run in flight ends with its spec.
	if _, err := s.EndRun("ab12cd34-2-1", Finished, "done", State{Spec, Approval}, specDoc("the spec")); err != nil {
		t.Fatal(err)
	}
	if sp, err := s.Spec(2); err != nil || sp != (SpecRow{Run: "ab12cd34-2-1", Commit: "beef", Text: "the spec"}) {
		t.Errorf("the spec is %+v, %v", sp, err)
	}
	if got, want := shape(t, s), shape(t, open(t)); !reflect.DeepEqual(got, want) {
		t.Errorf("a migrated store differs from a new one:\n%v\n%v", got, want)
	}
}

// TestDocumentKinds keeps a run's document of each kind, reads the newest among
// kinds, and refuses a kind that is none.
func TestDocumentKinds(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	end := func(id string, to State, doc *Doc) {
		t.Helper()
		if _, err := s.Move(card.ID, State{to.Stage, Run}, CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		mustAddRun(t, s, theRun(card.ID, id))
		if _, err := s.EndRun(id, Finished, "", to, doc); err != nil {
			t.Fatal(err)
		}
	}
	end("r1", State{Spec, Approval}, specDoc("the spec"))
	if _, err := s.Newest(card.ID, KindReview); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Newest of a card without a review: %v", err)
	}
	end("r2", State{Review, Run}, &Doc{KindReport, "the report"})
	end("r3", State{Review, Developer}, &Doc{KindReview, "Verdict: goes back\n"})

	if sp, err := s.Spec(card.ID); err != nil || sp.Run != "r1" || sp.Text != "the spec" {
		t.Errorf("Spec after a report and a review is %+v (%v)", sp, err)
	}
	for _, row := range []struct {
		kinds []string
		want  Document
	}{
		{[]string{KindSpec}, Document{KindSpec, "r1", "the spec", ""}},
		{[]string{KindReport}, Document{KindReport, "r2", "the report", ""}},
		{[]string{KindSpec, KindReview}, Document{KindReview, "r3", "Verdict: goes back\n", ""}},
		{[]string{KindSpec, KindReview, KindReport}, Document{KindReview, "r3", "Verdict: goes back\n", ""}},
	} {
		if got, err := s.Newest(card.ID, row.kinds...); err != nil || got != row.want {
			t.Errorf("Newest of %v is %+v (%v), want %+v", row.kinds, got, err, row.want)
		}
	}
	// A spec written after the review is newer.
	end("r4", State{Spec, Approval}, specDoc("a new spec"))
	if got, _ := s.Newest(card.ID, KindSpec, KindReview); got.Run != "r4" {
		t.Errorf("Newest after a new spec is %+v", got)
	}

	if _, err := s.Newest(card.ID); err == nil {
		t.Error("Newest of no kind was accepted")
	}
	// The board names each kind once, in the order of its first document.
	if b, err := s.Board(); err != nil || !reflect.DeepEqual(b[0].Kinds, []string{KindSpec, KindReport, KindReview}) {
		t.Errorf("the board's kinds are %+v (%v)", b, err)
	}
	if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, "r5"))
	if _, err := s.EndRun("r5", Finished, "", State{Spec, Approval}, &Doc{"notes", "x"}); err == nil {
		t.Error("a document of the kind notes was kept")
	}
	if r, _ := s.InFlight(card.ID); r == nil || countRows(t, s, "specs") != 4 {
		t.Errorf("a refused document ended the run or was kept: %+v, %d rows", r, countRows(t, s, "specs"))
	}
}

// TestMoveWithReason writes the reason on the entry of a move that ends no run.
func TestMoveWithReason(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	if _, err := s.Move(card.ID, State{Review, Run}, CauseRule); err != nil {
		t.Fatal(err)
	}
	moved, err := s.MoveWithReason(card.ID, State{Review, Run}, State{Review, Developer}, CauseRule, "the change is too large")
	if err != nil || moved.State != (State{Review, Developer}) {
		t.Fatalf("MoveWithReason returned %+v, %v", moved, err)
	}
	h := mustHistory(t, s, card.ID)
	if e := h[len(h)-1]; e.Run != "" || e.Reason != "the change is too large" || e.Cause != CauseRule {
		t.Errorf("the entry is %+v", e)
	}
	if _, err := s.MoveWithReason(card.ID, State{Review, Developer}, State{Done, Run}, CauseRule, "x"); err == nil {
		t.Error("an invalid pairing was written")
	}
	// A card that is no longer in the state the caller read, such as one
	// dropped meanwhile, is not moved.
	if got, err := s.MoveWithReason(card.ID, State{Review, Run}, State{Review, Developer}, CauseRule, "x"); !errors.Is(err, ErrRefused) || got.State != (State{Review, Developer}) {
		t.Errorf("a card in another state: %+v, %v", got, err)
	}
	if h := mustHistory(t, s, card.ID); len(h) != 3 {
		t.Errorf("%d entries after a refused move, want 3", len(h))
	}
}

// schemaV1 is the store of version 1 as card 6's code on main makes it. The
// migration test makes a store of that shape and fills it.
const schemaV1 = `
CREATE TABLE cards (
	id INTEGER PRIMARY KEY,
	text TEXT NOT NULL,
	stage TEXT NOT NULL,
	waits_for TEXT NOT NULL
) STRICT;
CREATE TABLE entries (
	number INTEGER PRIMARY KEY,
	card INTEGER NOT NULL,
	at TEXT NOT NULL,
	cause TEXT NOT NULL,
	stage TEXT NOT NULL,
	waits_for TEXT NOT NULL
) STRICT;
PRAGMA user_version = 1;
INSERT INTO cards (text, stage, waits_for) VALUES ('old card', 'spec', 'run');
INSERT INTO entries (card, at, cause, stage, waits_for) VALUES
	(1, '2026-11-17T10:00:00Z', 'developer', 'backlog', 'developer'),
	(1, '2026-11-17T10:01:00Z', 'developer', 'spec', 'run');
`

// makeV1 writes a store of version 1 to a new file and returns its path.
func makeV1(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return path
}

// shape is the schema of a store as SQLite keeps it, by name.
func shape(t *testing.T, s *Store) map[string]string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name, sql FROM sqlite_master ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, def sql.NullString
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatal(err)
		}
		out[name.String] = strings.Join(strings.Fields(def.String), " ")
	}
	return out
}

func userVersion(t *testing.T, s *Store) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMigrate(t *testing.T) {
	path := makeV1(t)
	s := reopen(t, path)
	if v := userVersion(t, s); v != version || version != 10 {
		t.Fatalf("the store is version %d after Open, want 10", v)
	}
	// What version 1 held is still there, and has no run or reason, and no
	// send-back.
	card, err := s.Card(1)
	if err != nil || card.Text != "old card" || card.State != (State{Spec, Run}) || card.SendBacks != 0 {
		t.Fatalf("the old card: %+v, %v", card, err)
	}
	h := mustHistory(t, s, 1)
	if len(h) != 2 || h[1].State != (State{Spec, Run}) || h[0].Run != "" || h[1].Reason != "" {
		t.Errorf("the old history: %+v", h)
	}
	if ids, _ := s.Waiting(); !reflect.DeepEqual(ids, []int64{1}) {
		t.Errorf("Waiting after the move forward: %v", ids)
	}
	// The old card takes a run and ends it.
	mustAddRun(t, s, theRun(1, "r1"))
	if _, err := s.EndRun("r1", Finished, "done", State{Spec, Approval}, nil); err != nil {
		t.Fatal(err)
	}

	// The identity is written once, and kept across reopens.
	id, err := s.Instance()
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = reopen(t, path)
	if again, _ := s.Instance(); again != id || countRows(t, s, "instance") != 1 {
		t.Errorf("after a reopen the instance is %q, want %q", again, id)
	}

	// A store moved forward has the shape of one made at the current version.
	fresh := open(t)
	if got, want := shape(t, s), shape(t, fresh); !reflect.DeepEqual(got, want) {
		t.Errorf("a migrated store differs from a new one:\n%v\n%v", got, want)
	}
}

// TestMigrateV9 moves a store of version 9 with a card forward: the card has
// the number of send-backs 0.
func TestMigrateV9(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("a card"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(1, State{Review, Developer}, CauseRun); err != nil {
		t.Fatal(err)
	}
	// Back to version 9: the column did not exist.
	if _, err := s.db.Exec(`ALTER TABLE cards DROP COLUMN send_backs; PRAGMA user_version = 9`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = reopen(t, path)
	if v := userVersion(t, s); v != 10 {
		t.Fatalf("the store is version %d, want 10", v)
	}
	card, err := s.Card(1)
	if err != nil || card.State != (State{Review, Developer}) || card.SendBacks != 0 {
		t.Errorf("the card after the move forward: %+v, %v", card, err)
	}
	if b, err := s.Board(); err != nil || len(b) != 1 || b[0].SendBacks != 0 {
		t.Errorf("the board after the move forward: %+v, %v", b, err)
	}
}

// TestKill is a large test (ADR 0031): it runs its own binary as a child
// process and kills it with SIGKILL, at the points the spec's table names and
// at random moments.
func TestKill(t *testing.T) {
	// A kill at each point of one Create and one Move, and after them.
	t.Run("creating a card", func(t *testing.T) {
		path := killAt(t, "hook:1")
		s := reopen(t, path)
		if _, err := s.Card(1); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("a Create killed before the commit left a card: %v", err)
		}
		if h := mustHistory(t, s, 1); len(h) != 0 {
			t.Fatalf("a Create killed before the commit left entries: %+v", h)
		}
		card := mustCreate(t, s, "again") // repeated, it writes the card
		if card.ID != 1 || len(mustHistory(t, s, 1)) != 1 {
			t.Errorf("the repeated Create made %+v with %d entries", card, len(mustHistory(t, s, 1)))
		}
	})
	t.Run("moving a card, at the hook", func(t *testing.T) {
		path := killAt(t, "hook:2")
		s := reopen(t, path)
		card, err := s.Card(1)
		if err != nil {
			t.Fatal(err)
		}
		if card.State != (State{Backlog, Developer}) || len(mustHistory(t, s, 1)) != 1 {
			t.Fatalf("a Move killed before the commit left %+v with %d entries", card, len(mustHistory(t, s, 1)))
		}
		moved, err := s.Move(1, State{Spec, Run}, CauseRule) // repeated, it moves
		if err != nil || moved.State != (State{Spec, Run}) || len(mustHistory(t, s, 1)) != 2 {
			t.Errorf("the repeated Move: %+v, %v", moved, err)
		}
	})
	t.Run("after the commit", func(t *testing.T) {
		path := killAt(t, "after")
		s := reopen(t, path)
		card, err := s.Card(1)
		if err != nil {
			t.Fatal(err)
		}
		h := mustHistory(t, s, 1)
		if card.State != (State{Spec, Run}) || len(h) != 2 || h[1].State != card.State {
			t.Fatalf("after a kill after the commit: %+v with %+v", card, h)
		}
		// The repeat of the Move finds the state it wanted and writes nothing.
		if _, err := s.Move(1, State{Spec, Run}, CauseRule); err != nil || len(mustHistory(t, s, 1)) != 2 {
			t.Errorf("the repeated Move: %v, %d entries", err, len(mustHistory(t, s, 1)))
		}
	})

	t.Run("while the schema is created", func(t *testing.T) {
		dir := t.TempDir()
		for range 20 {
			cmd := child(t, "open", dir)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Duration(5+rand.IntN(40)) * time.Millisecond)
			cmd.Process.Signal(syscall.SIGKILL)
			cmd.Wait()
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*.db"))
		if len(files) == 0 {
			t.Fatal("the child opened no store before it was killed")
		}
		for _, f := range files {
			s := reopen(t, f)
			if _, err := s.Create("x"); err != nil { // version 0 was created again, a later one opened
				t.Errorf("%s: %v", f, err)
			}
			s.Close()
		}
		t.Logf("%d stores opened by a child killed at random, all usable", len(files))
	})

	t.Run("ending a run, at the hook", func(t *testing.T) {
		path := killAt(t, "endrun-hook")
		s := reopen(t, path)
		card, err := s.Card(1)
		if err != nil {
			t.Fatal(err)
		}
		if card.State != (State{Spec, Run}) || len(mustHistory(t, s, 1)) != 2 {
			t.Fatalf("an EndRun killed before the commit left %+v with %d entries", card, len(mustHistory(t, s, 1)))
		}
		if r, err := s.InFlight(1); err != nil || r == nil || r.ID != "r1" {
			t.Fatalf("an EndRun killed before the commit left the run %v, %v, want it in flight", r, err)
		}
		moved, err := s.EndRun("r1", Finished, "saved", State{Spec, Approval}, nil) // repeated, it ends the run
		if err != nil || moved.State != (State{Spec, Approval}) || len(mustHistory(t, s, 1)) != 3 {
			t.Errorf("the repeated EndRun: %+v, %v", moved, err)
		}
		if r, _ := s.Run("r1"); r.Ending != Finished {
			t.Errorf("the run is %+v after the repeated EndRun", r)
		}
	})
	t.Run("ending a run by the rule, at the hook", func(t *testing.T) {
		path := killAt(t, "endrun-rule-hook")
		s := reopen(t, path)
		card, _ := s.Card(1)
		r, _ := s.Run("r1")
		if card.State != (State{Spec, Run}) || r.Ending != "" || len(mustHistory(t, s, 1)) != 2 || countRows(t, s, "publishes") != 0 {
			t.Fatalf("an EndRunByRule killed before the commit left %+v, the run %+v, %d entries and %d publications, want nothing of it",
				card, r, len(mustHistory(t, s, 1)), countRows(t, s, "publishes"))
		}
	})
	t.Run("a send-back by the rule, at the hook", func(t *testing.T) {
		path := killAt(t, "endrun-sendback-hook")
		s := reopen(t, path)
		card, _ := s.Card(1)
		r, _ := s.Run("r1")
		if card.State != (State{Review, Run}) || card.SendBacks != 0 || r.Ending != "" ||
			len(mustHistory(t, s, 1)) != 2 || countRows(t, s, "specs") != 0 {
			t.Fatalf("a send-back killed before the commit left %+v, the run %+v, %d entries and %d documents, want nothing of it",
				card, r, len(mustHistory(t, s, 1)), countRows(t, s, "specs"))
		}
		// Repeated, it writes the change once.
		card, err := s.EndRunByRule("r1", Finished, "goes back", State{Review, Developer}, sendBackRule, reviewDoc())
		if err != nil || card.State != (State{Implement, Run}) || card.SendBacks != 1 {
			t.Fatalf("the repeat: %+v, %v", card, err)
		}
		if _, err := s.EndRunByRule("r1", Finished, "goes back", State{Review, Developer}, sendBackRule, reviewDoc()); err != nil {
			t.Fatal(err)
		}
		if c, _ := s.Card(1); c.SendBacks != 1 || len(mustHistory(t, s, 1)) != 4 || countRows(t, s, "specs") != 1 {
			t.Errorf("after two repeats: %+v with %d entries and %d documents", c, len(mustHistory(t, s, 1)), countRows(t, s, "specs"))
		}
	})
	t.Run("ending a run, after the commit", func(t *testing.T) {
		path := killAt(t, "endrun-after")
		s := reopen(t, path)
		card, _ := s.Card(1)
		h := mustHistory(t, s, 1)
		r, _ := s.Run("r1")
		if card.State != (State{Spec, Approval}) || r.Ending != Finished || len(h) != 3 || h[2].Run != "r1" {
			t.Fatalf("after a kill after the commit: %+v, %+v, %+v", card, r, h)
		}
		if _, err := s.EndRun("r1", Finished, "saved", State{Spec, Approval}, nil); err != nil || len(mustHistory(t, s, 1)) != 3 {
			t.Errorf("the repeated EndRun: %v, %d entries", err, len(mustHistory(t, s, 1)))
		}
	})
	t.Run("ending a run with a spec, at the hook", func(t *testing.T) {
		path := killAt(t, "endrun-spec-hook")
		s := reopen(t, path)
		card, _ := s.Card(1)
		r, _ := s.Run("r1")
		if card.State != (State{Spec, Run}) || r.Ending != "" || len(mustHistory(t, s, 1)) != 2 || countRows(t, s, "specs") != 0 {
			t.Fatalf("an EndRun killed before the commit left %+v, the run %+v, %d entries and %d specs, want nothing of it",
				card, r, len(mustHistory(t, s, 1)), countRows(t, s, "specs"))
		}
		// Repeated, it ends the run and writes the spec, once.
		if _, err := s.EndRun("r1", Finished, "saved", State{Spec, Approval}, specDoc("the spec")); err != nil {
			t.Fatal(err)
		}
		if sp, err := s.Spec(1); err != nil || sp.Text != "the spec" || sp.Run != "r1" || countRows(t, s, "specs") != 1 {
			t.Errorf("after the repeat the spec is %+v (%v), with %d rows", sp, err, countRows(t, s, "specs"))
		}
	})
	t.Run("moving a store of version 2 forward", func(t *testing.T) {
		path := makeV2(t)
		before := dump(t, path)
		cmd := child(t, "migrate", path)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "stopped\n" {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("the child said %q, %v", line, err)
		}
		cmd.Process.Signal(syscall.SIGKILL)
		cmd.Wait()
		// The store is as version 2 left it: no specs, the old version, every row.
		if got := dump(t, path); got != before {
			t.Fatalf("after a kill in the move forward the store is\n%s\nwas\n%s", got, before)
		}
		s := reopen(t, path) // and it is moved forward when it is opened again
		if userVersion(t, s) != version || countRows(t, s, "specs") != 0 {
			t.Errorf("after the repeat: version %d, %d specs", userVersion(t, s), countRows(t, s, "specs"))
		}
	})
	t.Run("moving a store of version 1 forward", func(t *testing.T) {
		path := makeV1(t)
		cmd := child(t, "migrate", path)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "stopped\n" {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("the child said %q, %v", line, err)
		}
		cmd.Process.Signal(syscall.SIGKILL)
		cmd.Wait()

		// The store is as version 1 left it: no table, no column, the old version.
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		var v, tables, cols int
		db.QueryRow("PRAGMA user_version").Scan(&v)
		db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&tables)
		db.QueryRow("SELECT count(*) FROM pragma_table_info('entries')").Scan(&cols)
		db.Close()
		if v != 1 || tables != 2 || cols != 6 {
			t.Fatalf("after a kill in the move forward: version %d, %d tables, %d columns in entries, want 1, 2, 6", v, tables, cols)
		}
		s := reopen(t, path) // and it is moved forward when it is opened again
		if _, err := s.Instance(); err != nil || userVersion(t, s) != version || len(mustHistory(t, s, 1)) != 2 {
			t.Errorf("after the repeat: version %d, %v", userVersion(t, s), err)
		}
	})

	t.Run("at random moments", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "store.db")
		moves := 0
		for i := range 20 {
			cmd := child(t, "loop", path)
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Duration(20+rand.IntN(101)) * time.Millisecond)
			cmd.Process.Signal(syscall.SIGKILL)
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				moves++
			}
			cmd.Wait()

			s := reopen(t, path)
			var check string
			if err := s.db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
				t.Fatalf("kill %d: integrity_check says %q, %v", i, check, err)
			}
			if card, err := s.Card(1); err == nil {
				h := mustHistory(t, s, 1)
				if len(h) == 0 || h[len(h)-1].State != card.State {
					t.Fatalf("kill %d: the card is %v, and of its %d entries the last is %+v", i, card.State, len(h), h[max(len(h)-1, 0):])
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				t.Fatal(err)
			}
			s.Close()
		}
		if moves == 0 {
			t.Fatal("no Move was committed by any child: the kills prove nothing")
		}
		t.Logf("20 kills, %d moves committed before them", moves)
	})
	t.Run("an approval with a note, at the hook", func(t *testing.T) {
		path := killAt(t, "approve-hook")
		s := reopen(t, path)
		card, _ := s.Card(1)
		sp, err := s.Spec(1)
		if err != nil || card.State != (State{Spec, Approval}) || sp.Note != "" || card.SendBacks != 2 {
			t.Fatalf("an approval killed before the commit left %+v and the spec %+v, %v, want the card waiting with no note and its number 2", card, sp, err)
		}
		if card, err := s.Decide(1, Approve, "the note"); err != nil || card.SendBacks != 0 {
			t.Fatalf("the repeated approval: %+v, %v", card, err)
		}
		if sp, _ := s.Spec(1); sp.Note != "the note" {
			t.Errorf("the repeated approval kept the note %q", sp.Note)
		}
	})
}

// killAt runs a child that stops at the place mode names, kills it there and
// returns its store's path.
func killAt(t *testing.T, mode string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	cmd := child(t, mode, path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "stopped\n" {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("the child said %q, %v", line, err)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	return path
}

func child(t *testing.T, mode, path string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), childVar+"="+mode, pathVar+"="+path)
	cmd.Stderr = os.Stderr
	return cmd
}

// reopen opens a store that a child was killed on.
func reopen(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("opening after a kill: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// asChild is the child of TestKill. It stops, by printing "stopped" and then
// waiting to be killed, where its mode says:
//
//	hook:N  at the Nth call of afterCardWritten (1 is the Create, 2 the Move)
//	after   after the Create and the Move have returned
//	endrun-hook   at the commit of EndRun, after a Create, a Move and an AddRun
//	endrun-after  after EndRun has returned
//	endrun-spec-hook  as endrun-hook, with a spec given to EndRun
//	endrun-rule-hook  at the commit of EndRunByRule, with a publication to write
//	endrun-sendback-hook  at the commit of EndRunByRule with a send-back and a review, from review
//	approve-hook  at the commit of an approval with a note, after a spec waits for approval
//	migrate       in the move forward of the store at path, of version 1 or 2
//	loop    never: it moves card 1 through valid states, printing a line each time
//	open    never: it opens a new store in the directory path, again and again
func asChild(mode, path string) {
	stop := func() {
		fmt.Println("stopped")
		select {}
	}
	if mode == "migrate" {
		afterMigration = stop
		if _, err := Open(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "the move forward did not run")
		os.Exit(1)
	}
	if mode == "open" {
		for i := 0; ; i++ {
			s, err := Open(filepath.Join(path, strconv.Itoa(i)+".db"))
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			s.Close()
		}
	}
	if n, ok := strings.CutPrefix(mode, "hook:"); ok {
		want, _ := strconv.Atoi(n)
		calls := 0
		afterCardWritten = func() error {
			if calls++; calls == want {
				stop()
			}
			return nil
		}
	}
	s, err := Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := s.Card(1); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			fail(err)
		}
		if _, err := s.Create("a card"); err != nil {
			fail(err)
		}
	}
	if mode == "loop" {
		// Neighbours differ, and every one is a valid pairing.
		cycle := []State{{Spec, Run}, {Spec, Approval}, {Implement, Run}, {Review, Run},
			{Review, Host}, {Done, Nothing}, {Backlog, Developer}, {Backlog, Room}}
		all := []Cause{CauseDeveloper, CauseRun, CauseHost, CauseRule}
		for i := 0; ; i++ {
			if _, err := s.Move(1, cycle[i%len(cycle)], all[i%len(all)]); err != nil {
				fail(err)
			}
			fmt.Println("moved")
		}
	}
	first := State{Spec, Run}
	if mode == "endrun-sendback-hook" {
		first = State{Review, Run}
	}
	if _, err := s.Move(1, first, CauseRule); err != nil {
		fail(err)
	}
	if strings.HasPrefix(mode, "endrun-") {
		if err := s.AddRun(theRun(1, "r1"), 8); err != nil {
			fail(err)
		}
		var spec *Doc
		if mode == "endrun-spec-hook" {
			spec = specDoc("the spec")
		}
		if mode == "endrun-hook" || mode == "endrun-spec-hook" || mode == "endrun-rule-hook" || mode == "endrun-sendback-hook" {
			afterCardWritten = func() error { stop(); return nil }
		}
		if mode == "endrun-sendback-hook" {
			if _, err := s.EndRunByRule("r1", Finished, "goes back", State{Review, Developer}, sendBackRule, reviewDoc()); err != nil {
				fail(err)
			}
			stop()
		}
		if mode == "endrun-rule-hook" {
			rule := Rule{To: State{Review, Host}, Reason: "by the rule", Publish: true}
			if _, err := s.EndRunByRule("r1", Finished, "saved", State{Review, Approval}, rule); err != nil {
				fail(err)
			}
			stop()
		}
		if _, err := s.EndRun("r1", Finished, "saved", State{Spec, Approval}, spec); err != nil {
			fail(err)
		}
		stop()
	}
	if mode == "approve-hook" {
		if err := s.AddRun(theRun(1, "r1"), 8); err != nil {
			fail(err)
		}
		if _, err := s.EndRun("r1", Finished, "saved", State{Spec, Approval}, specDoc("the spec")); err != nil {
			fail(err)
		}
		// A card sent back twice before, as one sent back to spec is.
		if _, err := s.db.Exec(`UPDATE cards SET send_backs = 2 WHERE id = 1`); err != nil {
			fail(err)
		}
		afterCardWritten = func() error { stop(); return nil }
		if _, err := s.Decide(1, Approve, "the note"); err != nil {
			fail(err)
		}
		stop()
	}
	if mode == "after" {
		stop()
	}
}

func TestEndRunByRule(t *testing.T) {
	setup := func(t *testing.T) *Store {
		s := open(t)
		if _, err := s.Create("a card"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Move(1, State{Review, Run}, CauseRule); err != nil {
			t.Fatal(err)
		}
		mustAddRun(t, s, theRun(1, "r1"))
		return s
	}
	rule := Rule{To: State{Review, Host}, Reason: "the rule publishes it", Publish: true}

	t.Run("both entries, the card and the publication", func(t *testing.T) {
		s := setup(t)
		// A branch kept from an earlier publishing, and a yes and an address to reset.
		if _, err := s.db.Exec(`INSERT INTO publishes (card, workflow_yes, commit_id, address, branch) VALUES (1, 'abc', 'def', 'https://x/1', 'card/1-x')`); err != nil {
			t.Fatal(err)
		}
		card, err := s.EndRunByRule("r1", Finished, "passes", State{Review, Approval}, rule)
		if err != nil || card.State != rule.To {
			t.Fatalf("EndRunByRule: %+v, %v", card, err)
		}
		if got, _ := s.Card(1); got.State != rule.To {
			t.Errorf("the card is %+v", got)
		}
		h := mustHistory(t, s, 1)
		if len(h) != 4 {
			t.Fatalf("history %+v", h)
		}
		a, b := h[2], h[3]
		if a.Cause != CauseRun || a.State != (State{Review, Approval}) || a.Reason != "passes" || a.Run != "r1" {
			t.Errorf("the run's entry is %+v", a)
		}
		if b.Cause != CauseRule || b.State != rule.To || b.Reason != rule.Reason || b.Run != "" {
			t.Errorf("the rule's entry is %+v", b)
		}
		if !a.At.Equal(b.At) {
			t.Errorf("the entries have the times %v and %v", a.At, b.At)
		}
		var since string
		if err := s.db.QueryRow(`SELECT since FROM cards WHERE id = 1`).Scan(&since); err != nil || since != b.At.Format(time.RFC3339Nano) {
			t.Errorf("since is %q (%v), want %v", since, err, b.At.Format(time.RFC3339Nano))
		}
		if r, _ := s.Run("r1"); r.Ending != Finished {
			t.Errorf("the run is %+v", r)
		}
		if p, err := s.Publication(1); err != nil || p != (Publication{Branch: "card/1-x"}) {
			t.Errorf("the publication is %+v, %v", p, err)
		}
		// A second call for the same run writes nothing.
		if _, err := s.EndRunByRule("r1", Finished, "passes", State{Review, Approval}, rule); err != nil {
			t.Fatal(err)
		}
		if n := len(mustHistory(t, s, 1)); n != 4 {
			t.Errorf("a repeat left %d entries", n)
		}
	})
	t.Run("without Publish no publication is written", func(t *testing.T) {
		s := setup(t)
		r := Rule{To: State{Review, Approval}, Reason: "waits"}
		if _, err := s.EndRunByRule("r1", Finished, "passes", State{Review, Approval}, r); err != nil {
			t.Fatal(err)
		}
		if n := countRows(t, s, "publishes"); n != 0 {
			t.Errorf("%d publications", n)
		}
		if n := len(mustHistory(t, s, 1)); n != 4 {
			t.Errorf("%d entries", n)
		}
	})
	t.Run("refused before anything is written", func(t *testing.T) {
		s := setup(t)
		for name, r := range map[string]Rule{
			"no reason":     {To: State{Review, Host}, Publish: true},
			"a bad pairing": {To: State{Backlog, Host}, Reason: "x"},
		} {
			if _, err := s.EndRunByRule("r1", Finished, "passes", State{Review, Approval}, r); err == nil {
				t.Errorf("%s: no error", name)
			}
		}
		if run, _ := s.Run("r1"); run.Ending != "" || len(mustHistory(t, s, 1)) != 2 || countRows(t, s, "publishes") != 0 {
			t.Errorf("something was written: %+v", run)
		}
	})
}

// sendBackRule is the step of the rule after a review that goes back.
var sendBackRule = Rule{To: State{Implement, Run}, Reason: "sent back by the rule", SendBack: true}

func reviewDoc() *Doc { return &Doc{Kind: KindReview, Text: "Verdict: goes back\n\nA finding.\n"} }

// setSendBacks sets a card's number of send-backs, as the steps that raise it
// would have.
func setSendBacks(t *testing.T, s *Store, card int64, n int) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE cards SET send_backs = ? WHERE id = ?`, n, card); err != nil {
		t.Fatal(err)
	}
}

func TestSendBack(t *testing.T) {
	setup := func(t *testing.T, n int) *Store {
		s := open(t)
		mustCreate(t, s, "a card")
		if _, err := s.Move(1, State{Review, Run}, CauseRule); err != nil {
			t.Fatal(err)
		}
		setSendBacks(t, s, 1, n)
		mustAddRun(t, s, theRun(1, "r1"))
		return s
	}
	t.Run("the step of a send-back", func(t *testing.T) {
		s := setup(t, 0)
		card, err := s.EndRunByRule("r1", Finished, "goes back", State{Review, Developer}, sendBackRule, reviewDoc())
		if err != nil || card.State != (State{Implement, Run}) || card.SendBacks != 1 {
			t.Fatalf("EndRunByRule: %+v, %v", card, err)
		}
		if got, _ := s.Card(1); got.State != (State{Implement, Run}) || got.SendBacks != 1 {
			t.Errorf("the card is %+v", got)
		}
		if r, _ := s.Run("r1"); r.Ending != Finished {
			t.Errorf("the run is %+v", r)
		}
		h := mustHistory(t, s, 1)
		if len(h) != 4 {
			t.Fatalf("history %+v", h)
		}
		if a := h[2]; a.Cause != CauseRun || a.State != (State{Review, Developer}) || a.Run != "r1" {
			t.Errorf("the run's entry is %+v", a)
		}
		if b := h[3]; b.Cause != CauseRule || b.State != (State{Implement, Run}) || b.Reason != sendBackRule.Reason || b.Run != "" {
			t.Errorf("the rule's entry is %+v", b)
		}
		// A repeat writes nothing.
		if _, err := s.EndRunByRule("r1", Finished, "goes back", State{Review, Developer}, sendBackRule, reviewDoc()); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Card(1); got.SendBacks != 1 || len(mustHistory(t, s, 1)) != 4 || countRows(t, s, "specs") != 1 {
			t.Errorf("a repeat left %+v with %d entries", got, len(mustHistory(t, s, 1)))
		}
	})
	t.Run("refused at two", func(t *testing.T) {
		s := setup(t, 2)
		_, err := s.EndRunByRule("r1", Finished, "goes back", State{Review, Developer}, sendBackRule, reviewDoc())
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("a third send-back to implement: %v, want ErrRefused", err)
		}
		card, _ := s.Card(1)
		if r, _ := s.Run("r1"); r.Ending != "" || card.State != (State{Review, Run}) || card.SendBacks != 2 ||
			len(mustHistory(t, s, 1)) != 2 || countRows(t, s, "specs") != 0 {
			t.Errorf("something was written: the run %+v, the card %+v", r, card)
		}
	})
	t.Run("a stop at two is counted", func(t *testing.T) {
		s := setup(t, 2)
		stop := Rule{To: State{Review, Developer}, Reason: "waits for you", SendBack: true}
		card, err := s.EndRunByRule("r1", Finished, "goes back", State{Review, Developer}, stop, reviewDoc())
		if err != nil || card.State != (State{Review, Developer}) || card.SendBacks != 3 {
			t.Fatalf("the stop: %+v, %v", card, err)
		}
	})
	t.Run("a send-back that publishes is refused", func(t *testing.T) {
		s := setup(t, 0)
		r := Rule{To: State{Review, Host}, Reason: "x", SendBack: true, Publish: true}
		if _, err := s.EndRunByRule("r1", Finished, "passes", State{Review, Approval}, r); !errors.Is(err, ErrRefused) {
			t.Errorf("a send-back that publishes: %v", err)
		}
		if c, _ := s.Card(1); c.SendBacks != 0 || len(mustHistory(t, s, 1)) != 2 {
			t.Errorf("something was written: %+v", c)
		}
	})
	t.Run("MoveSentBack", func(t *testing.T) {
		s := open(t)
		mustCreate(t, s, "a card")
		if _, err := s.Move(1, atHost, CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		card, err := s.MoveSentBack(1, atHost, State{Implement, Run}, "main moved")
		if err != nil || card.State != (State{Implement, Run}) || card.SendBacks != 1 {
			t.Fatalf("MoveSentBack: %+v, %v", card, err)
		}
		if h := mustHistory(t, s, 1); h[len(h)-1].Cause != CauseRule || h[len(h)-1].Reason != "main moved" {
			t.Errorf("the entry is %+v", h[len(h)-1])
		}
		// From another state it is refused, and the number stays.
		if _, err := s.MoveSentBack(1, atHost, State{Implement, Run}, "main moved"); !errors.Is(err, ErrRefused) {
			t.Errorf("a repeat: %v", err)
		}
		if c, _ := s.Card(1); c.SendBacks != 1 {
			t.Errorf("after the repeat the number is %d", c.SendBacks)
		}
		// At two, to a run is refused, and to the developer counts.
		if _, err := s.Move(1, atHost, CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		setSendBacks(t, s, 1, 2)
		if _, err := s.MoveSentBack(1, atHost, State{Implement, Run}, "main moved"); !errors.Is(err, ErrRefused) {
			t.Errorf("a third send-back to a run: %v", err)
		}
		card, err = s.MoveSentBack(1, atHost, State{Implement, Developer}, "main moved; waits for you")
		if err != nil || card.State != (State{Implement, Developer}) || card.SendBacks != 3 {
			t.Errorf("a third send-back to the developer: %+v, %v", card, err)
		}
	})
	t.Run("the developer's moves leave the number", func(t *testing.T) {
		s := setup(t, 2)
		stop := Rule{To: State{Review, Developer}, Reason: "waits for you"}
		if _, err := s.EndRunByRule("r1", Finished, "goes back", State{Review, Developer}, stop, reviewDoc()); err != nil {
			t.Fatal(err)
		}
		card, err := s.Decide(1, SendBack, "")
		if err != nil || card.State != (State{Implement, Run}) || card.SendBacks != 2 {
			t.Errorf("the developer's send-back: %+v, %v", card, err)
		}
		if c, _ := s.Card(1); c.SendBacks != 2 {
			t.Errorf("the card is %+v", c)
		}
	})
	t.Run("approval starts the number again", func(t *testing.T) {
		s := setup(t, 2)
		stop := Rule{To: State{Review, Developer}, Reason: "waits for you"}
		if _, err := s.EndRunByRule("r1", Finished, "goes back", State{Review, Developer}, stop, reviewDoc()); err != nil {
			t.Fatal(err)
		}
		card, err := s.Decide(1, ToSpec, "")
		if err != nil || card.SendBacks != 2 {
			t.Fatalf("to-spec: %+v, %v", card, err)
		}
		mustAddRun(t, s, theRun(1, "r2"))
		if _, err := s.EndRun("r2", Finished, "a new spec", State{Spec, Approval}, specDoc("the new spec")); err != nil {
			t.Fatal(err)
		}
		if c, _ := s.Card(1); c.SendBacks != 2 {
			t.Errorf("before the approval the number is %d", c.SendBacks)
		}
		card, err = s.Decide(1, Approve, "")
		if err != nil || card.State != (State{Implement, Run}) || card.SendBacks != 0 {
			t.Errorf("the approval: %+v, %v", card, err)
		}
		if c, _ := s.Card(1); c.SendBacks != 0 {
			t.Errorf("after the approval the number is %d", c.SendBacks)
		}
	})
}
