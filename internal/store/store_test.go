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
	if _, err := db.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("a store of version 2 was opened")
	}
	if msg := err.Error(); !strings.Contains(msg, "version 2") || !strings.Contains(msg, "version 1") {
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
	if v != 2 || tables != 0 {
		t.Errorf("after the refusal: version %d with %d tables, want 2 and 0", v, tables)
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
			if _, err := s.Create("x"); err != nil { // version 0 was created again, version 1 opened
				t.Errorf("%s: %v", f, err)
			}
			s.Close()
		}
		t.Logf("%d stores opened by a child killed at random, all usable", len(files))
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
//	loop    never: it moves card 1 through valid states, printing a line each time
//	open    never: it opens a new store in the directory path, again and again
func asChild(mode, path string) {
	stop := func() {
		fmt.Println("stopped")
		select {}
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
	if mode == "after" {
		stop()
	}
}
