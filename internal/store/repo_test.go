package store

import (
	"bufio"
	"path/filepath"
	"syscall"
	"testing"
)

// makeV15 writes a store of version 15, before cards named their repo, with
// two cards in it.
func makeV15(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, "the first card")
	mustCreate(t, s, "the second card")
	if _, err := s.db.Exec(`ALTER TABLE cards DROP COLUMN work_item;
ALTER TABLE cards DROP COLUMN repo;
ALTER TABLE publishes DROP COLUMN feedback_since;
PRAGMA user_version = 15`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	return path
}

// TestMigrateV15 gives every card of a store of version 15 the repo kiln, and
// a kill in the move forward leaves version 15 for the next Open to move.
func TestMigrateV15(t *testing.T) {
	path := makeV15(t)
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
	if got := dump(t, path); got != before {
		t.Fatalf("after a kill in the move forward the store is\n%s\nwas\n%s", got, before)
	}

	s := reopen(t, path)
	if v := userVersion(t, s); v != version {
		t.Fatalf("the store is version %d, want %d", v, version)
	}
	for _, id := range []int64{1, 2} {
		if c, err := s.Card(id); err != nil || c.Repo != KilnRepo {
			t.Errorf("card %d after the move forward: %+v, %v, want the repo %q", id, c, err, KilnRepo)
		}
	}
	b, err := s.Board()
	if err != nil || len(b) != 2 {
		t.Fatalf("the board: %+v, %v", b, err)
	}
	for _, sum := range b {
		if sum.Repo != KilnRepo {
			t.Errorf("the board's card %d is in %q, want %q", sum.ID, sum.Repo, KilnRepo)
		}
	}
}

// TestCardKeepsItsRepo writes a card in the repo it is given, and keeps it
// through its moves.
func TestCardKeepsItsRepo(t *testing.T) {
	s := open(t)
	c, err := s.CreateIn("second", "a card of the second repo", 0)
	if err != nil || c.Repo != "second" {
		t.Fatalf("CreateIn: %+v, %v", c, err)
	}
	if c, err := s.Card(c.ID); err != nil || c.Repo != "second" {
		t.Errorf("the card as read: %+v, %v", c, err)
	}
	if _, err := s.Start(c.ID); err != nil {
		t.Fatal(err)
	}
	mustMove(t, s, c.ID, State{Implement, Run})
	mustMove(t, s, c.ID, State{Review, Run})
	if c, err := s.Card(c.ID); err != nil || c.Repo != "second" || c.State != (State{Review, Run}) {
		t.Errorf("the card after its moves: %+v, %v", c, err)
	}
	if got := summaryOf(t, s, c.ID); got.Repo != "second" {
		t.Errorf("the board's card is in %q, want second", got.Repo)
	}

	cards := countRows(t, s, "cards")
	if _, err := s.CreateIn("", "a card of no repo", 0); err == nil {
		t.Error("a card of no repo was written")
	}
	if n := countRows(t, s, "cards"); n != cards {
		t.Errorf("%d cards after the refusal, want %d", n, cards)
	}

	if k := mustCreate(t, s, "a card of the flags"); k.Repo != KilnRepo {
		t.Errorf("Create wrote %q, want %q", k.Repo, KilnRepo)
	}
}

// TestProposalCardInItsRepo makes a card from a proposal in the repo of the
// card the proposal was noticed on.
func TestProposalCardInItsRepo(t *testing.T) {
	s := open(t)
	c, err := s.CreateIn("second", "the noticed card", 0)
	if err != nil {
		t.Fatal(err)
	}
	mustMove(t, s, c.ID, State{Review, Run})
	mustAddRun(t, s, theRun(c.ID, "r1"))
	if _, err := s.EndRun("r1", Finished, "exit 0", State{Review, Developer}, Cost{}); err != nil {
		t.Fatal(err)
	}
	p := propose(t, s, c.ID, "r1")
	made, _, err := s.MakeCard(p)
	if err != nil {
		t.Fatal(err)
	}
	if made.Repo != "second" {
		t.Errorf("the card made from the proposal is in %q, want second", made.Repo)
	}
	if read, err := s.Card(made.ID); err != nil || read.Repo != "second" {
		t.Errorf("the card as read: %+v, %v", read, err)
	}
}

// TestPredecessorInAnotherRepo lets a card name a predecessor of another
// repo, and releases it when that card is done.
func TestPredecessorInAnotherRepo(t *testing.T) {
	s := open(t)
	first := mustCreate(t, s, "a card of kiln")
	c, err := s.CreateIn("second", "a card of the second repo", 0, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c, err = s.Start(c.ID); err != nil {
		t.Fatal(err)
	}
	if c.State != (State{Backlog, Room}) {
		t.Fatalf("the started card is %+v, want queued", c.State)
	}
	merged(t, s, first.ID)
	if moved, _ := release(t, s); len(moved) != 1 {
		t.Fatalf("released %v, want the one card", moved)
	}
	if c, err := s.Card(c.ID); err != nil || c.State != (State{Spec, Run}) || c.Repo != "second" {
		t.Errorf("the released card: %+v, %v", c, err)
	}
}
