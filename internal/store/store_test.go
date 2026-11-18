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
	{"review", "run"}: true, {"review", "developer"}: true, {"review", "host"}: true,
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
	if _, err := db.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("a store of version 3 was opened")
	}
	if msg := err.Error(); !strings.Contains(msg, "version 3") || !strings.Contains(msg, "version 2") {
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
	if v != 3 || tables != 0 {
		t.Errorf("after the refusal: version %d with %d tables, want 3 and 0", v, tables)
	}
}

func mustAddRun(t *testing.T, s *Store, r RunRow) {
	t.Helper()
	if err := s.AddRun(r); err != nil {
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

func TestWaiting(t *testing.T) {
	s := open(t)
	a, b, c := mustCreate(t, s, "a"), mustCreate(t, s, "b"), mustCreate(t, s, "c")
	if ids, err := s.Waiting(); err != nil || len(ids) != 0 {
		t.Fatalf("Waiting with every card in the backlog: %v, %v", ids, err)
	}
	for _, card := range []Card{c, a} {
		if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Move(b.ID, State{Spec, Approval}, CauseRun); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.Waiting(); err != nil || !reflect.DeepEqual(ids, []int64{a.ID, c.ID}) {
		t.Errorf("Waiting is %v, %v, want cards %d and %d by ID", ids, err, a.ID, c.ID)
	}
}

func TestRuns(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	other := mustCreate(t, s, "another")
	first := theRun(card.ID, "ab12cd34-1-1")

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
		if err := s.AddRun(r); err == nil {
			t.Errorf("AddRun with another %s under the same ID was accepted", name)
		}
	}
	// A second run in flight for the card is refused, by the store itself.
	if err := s.AddRun(theRun(card.ID, "ab12cd34-1-2")); err == nil {
		t.Error("a second run in flight for one card was accepted")
	}
	if n, _ := s.RunCount(card.ID); n != 1 {
		t.Errorf("RunCount after the refusals is %d, want 1", n)
	}
	// A run is written in flight, with every field.
	for _, r := range []RunRow{{}, {ID: "x", Card: other.ID}, func() RunRow { r := theRun(other.ID, "x"); r.Ending = Finished; return r }()} {
		if err := s.AddRun(r); err == nil {
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
		"an unknown ending": func() error { _, err := s.EndRun(run.ID, "withdrawn", "", State{Spec, Approval}); return err },
		"no ending":         func() error { _, err := s.EndRun(run.ID, "", "", State{Spec, Approval}); return err },
		"an invalid state":  func() error { _, err := s.EndRun(run.ID, Finished, "", State{Done, Run}); return err },
		"an unknown run":    func() error { _, err := s.EndRun("nothing", Finished, "", State{Spec, Approval}); return err },
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

	moved, err := s.EndRun(run.ID, Finished, "saved abc123", State{Spec, Approval})
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
	again, err := s.EndRun(run.ID, Failed, "other", State{Spec, Developer})
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
	mustAddRun(t, s, theRun(card.ID, "ab12cd34-1-2"))
	if n, _ := s.RunCount(card.ID); n != 2 {
		t.Errorf("RunCount is %d, want 2", n)
	}

	// A failed run, with no reason, moves a card to needs you; and a card that
	// is in the state already still gets the run's ending and its entry.
	s2 := open(t)
	c2 := mustCreate(t, s2, "b")
	if _, err := s2.Move(c2.ID, State{Spec, Developer}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s2, theRun(c2.ID, "r"))
	if _, err := s2.EndRun("r", Failed, "", State{Spec, Developer}); err != nil {
		t.Fatal(err)
	}
	if r, _ := s2.Run("r"); r.Ending != Failed {
		t.Errorf("the run is %+v, want failed", r)
	}
	if h := mustHistory(t, s2, c2.ID); len(h) != 3 || h[2].Run != "r" || h[2].Reason != "" {
		t.Errorf("history: %+v", h)
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
	if v := userVersion(t, s); v != version || version != 2 {
		t.Fatalf("the store is version %d after Open, want 2", v)
	}
	// What version 1 held is still there, and has no run or reason.
	card, err := s.Card(1)
	if err != nil || card.Text != "old card" || card.State != (State{Spec, Run}) {
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
	if _, err := s.EndRun("r1", Finished, "done", State{Spec, Approval}); err != nil {
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

	// A store moved forward has the shape of one made at version 2.
	fresh := open(t)
	if got, want := shape(t, s), shape(t, fresh); !reflect.DeepEqual(got, want) {
		t.Errorf("a migrated store differs from a new one:\n%v\n%v", got, want)
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
			if _, err := s.Create("x"); err != nil { // version 0 was created again, version 2 opened
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
		moved, err := s.EndRun("r1", Finished, "saved", State{Spec, Approval}) // repeated, it ends the run
		if err != nil || moved.State != (State{Spec, Approval}) || len(mustHistory(t, s, 1)) != 3 {
			t.Errorf("the repeated EndRun: %+v, %v", moved, err)
		}
		if r, _ := s.Run("r1"); r.Ending != Finished {
			t.Errorf("the run is %+v after the repeated EndRun", r)
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
		if _, err := s.EndRun("r1", Finished, "saved", State{Spec, Approval}); err != nil || len(mustHistory(t, s, 1)) != 3 {
			t.Errorf("the repeated EndRun: %v, %d entries", err, len(mustHistory(t, s, 1)))
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
		if _, err := s.Instance(); err != nil || userVersion(t, s) != 2 || len(mustHistory(t, s, 1)) != 2 {
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
//	migrate       in the move forward of the store at path, which is of version 1
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
	if _, err := s.Move(1, State{Spec, Run}, CauseRule); err != nil {
		fail(err)
	}
	if strings.HasPrefix(mode, "endrun-") {
		if err := s.AddRun(theRun(1, "r1")); err != nil {
			fail(err)
		}
		if mode == "endrun-hook" {
			afterCardWritten = func() error { stop(); return nil }
		}
		if _, err := s.EndRun("r1", Finished, "saved", State{Spec, Approval}); err != nil {
			fail(err)
		}
		stop()
	}
	if mode == "after" {
		stop()
	}
}
