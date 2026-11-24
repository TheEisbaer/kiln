package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// twoProposals are a review's two proposals.
var twoProposals = []Proposal{{Title: "The first", Text: "foo.go:12 panics on nil."}, {Title: "", Text: "Something else, as written."}}

// reviewed writes a card whose review run r1 ended with the docs given, and
// returns the card.
func reviewed(t *testing.T, s *Store, docs ...*Doc) Card {
	t.Helper()
	c := mustCreate(t, s, "the noticed card")
	if _, err := s.Move(c.ID, State{Review, Run}, CauseRule); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(c.ID, "r1"))
	c, err := s.EndRun("r1", Finished, "exit 0", State{Review, Developer}, Cost{}, docs...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// propose writes an open proposal on a card, as a review run of it would.
func propose(t *testing.T, s *Store, card int64, run string) int64 {
	t.Helper()
	res, err := s.db.Exec(`INSERT INTO proposals (card, run, number, title, text) VALUES (?, ?, 1, 'A title', 'A text.')`, card, run)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestProposalsKept keeps a review's two proposals with the card and the run,
// numbered in their order, in no other table, and once when the end is
// written again.
func TestProposalsKept(t *testing.T) {
	s := open(t)
	doc := &Doc{Kind: KindProposals, Proposals: twoProposals}
	c := reviewed(t, s, &Doc{Kind: KindReview, Text: "Verdict: goes back\n"}, doc)
	open, n, err := s.OpenProposals()
	if err != nil {
		t.Fatal(err)
	}
	want := []Proposal{
		{ID: 1, Card: c.ID, Run: "r1", Number: 1, Title: "The first", Text: "foo.go:12 panics on nil."},
		{ID: 2, Card: c.ID, Run: "r1", Number: 2, Title: "", Text: "Something else, as written."},
	}
	if !reflect.DeepEqual(open, want) || n != (ProposalCounts{Made: 2}) {
		t.Errorf("the open proposals %+v and %+v, want %+v", open, n, want)
	}
	if docs, err := s.Documents(c.ID); err != nil || len(docs) != 1 || docs[0].Kind != KindReview {
		t.Errorf("the card's documents are %+v (%v), want its review alone", docs, err)
	}

	// Read again: the run has ended, and nothing is written.
	if _, err := s.EndRun("r1", Finished, "exit 0", State{Review, Developer}, Cost{}, doc); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, "proposals"); n != 2 {
		t.Errorf("the same review read twice keeps %d proposals, want 2", n)
	}
}

// TestNoProposals keeps none of a review without a proposals doc, or with one
// that holds none.
func TestNoProposals(t *testing.T) {
	s := open(t)
	reviewed(t, s, &Doc{Kind: KindReview, Text: "Verdict: passes\n"})
	c := mustCreate(t, s, "another")
	if _, err := s.Move(c.ID, State{Review, Run}, CauseRule); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(c.ID, "r2"))
	if _, err := s.EndRun("r2", Finished, "", State{Review, Developer}, Cost{}, &Doc{Kind: KindProposals}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, "proposals"); n != 0 {
		t.Errorf("%d proposals kept, want none", n)
	}
	if open, n, err := s.OpenProposals(); err != nil || len(open) != 0 || n != (ProposalCounts{}) {
		t.Errorf("the open proposals %+v, %+v, %v", open, n, err)
	}
}

// TestMakeCard makes a card from a proposal: a backlog card, not started, with
// the title and the text, that remembers where it came from; the proposal is
// no longer open, and a second move on it is refused.
func TestMakeCard(t *testing.T) {
	s := open(t)
	noticed := reviewed(t, s, &Doc{Kind: KindProposals, Proposals: twoProposals})
	before := mustHistory(t, s, noticed.ID)
	c, p, err := s.MakeCard(1)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != (State{Backlog, Developer}) || c.Text != "The first\n\nfoo.go:12 panics on nil." {
		t.Errorf("the card made is %+v", c)
	}
	if h := mustHistory(t, s, c.ID); len(h) != 1 || h[0].Cause != CauseDeveloper || h[0].Reason != "" || h[0].Proposal != 0 {
		t.Errorf("the made card's history is %+v", h)
	}
	if p.Made != c.ID || p.Open() {
		t.Errorf("the proposal after the move is %+v", p)
	}
	if from, err := s.ProposalOf(c.ID); err != nil || from == nil || from.ID != 1 || from.Card != noticed.ID || from.Run != "r1" {
		t.Errorf("the card was made from %+v (%v)", from, err)
	}
	if from, err := s.ProposalOf(noticed.ID); err != nil || from != nil {
		t.Errorf("the noticed card was made from %+v (%v), want none", from, err)
	}
	if open, n, _ := s.OpenProposals(); len(open) != 1 || open[0].ID != 2 || n != (ProposalCounts{Made: 2, Accepted: 1}) {
		t.Errorf("the open proposals %+v, %+v", open, n)
	}
	after := mustHistory(t, s, noticed.ID)
	if len(after) != len(before)+1 {
		t.Fatalf("the noticed card has %d entries, had %d", len(after), len(before))
	}
	e := after[len(after)-1]
	if e.Proposal != 1 || e.Cause != CauseDeveloper || e.State != noticed.State || e.Run != "" ||
		e.Reason != "made card #2 from proposal 1" || e.Note() != "" {
		t.Errorf("the entry about the proposal is %+v", e)
	}
	if n, m := Interventions(after), Interventions(mustHistory(t, s, c.ID)); n != 0 || m != 0 {
		t.Errorf("interventions %d on the noticed card and %d on the made one, want none", n, m)
	}

	// Made into a card already: refused, and no second card.
	if _, _, err := s.MakeCard(1); !errors.Is(err, ErrRefused) {
		t.Errorf("a second make-card: %v", err)
	}
	if _, err := s.Dismiss(1, ""); !errors.Is(err, ErrRefused) {
		t.Errorf("a dismiss after make-card: %v", err)
	}
	if n := countRows(t, s, "cards"); n != 2 {
		t.Errorf("%d cards, want 2", n)
	}
	if _, _, err := s.MakeCard(9); !errors.Is(err, ErrNoProposal) {
		t.Errorf("an unknown proposal: %v", err)
	}
	// With an empty title, the card's text is the text alone.
	if c, _, err := s.MakeCard(2); err != nil || c.Text != "Something else, as written." {
		t.Errorf("a card from a proposal without a title: %+v, %v", c, err)
	}
}

// TestDismiss dismisses a proposal with a note and without, and leaves the
// noticed card's row as it was.
func TestDismiss(t *testing.T) {
	s := open(t)
	noticed := reviewed(t, s, &Doc{Kind: KindProposals, Proposals: twoProposals})
	since := func() string {
		var at string
		if err := s.db.QueryRow(`SELECT since FROM cards WHERE id = ?`, noticed.ID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	was := since()

	if _, err := s.Dismiss(1, strings.Repeat("x", MaxNote+1)); !errors.Is(err, ErrNote) {
		t.Errorf("a note over 16 KiB: %v", err)
	}
	p, err := s.Dismiss(1, "")
	if err != nil || !p.Dismissed || p.Note != "" || p.Open() {
		t.Fatalf("a dismiss without a note: %+v, %v", p, err)
	}
	p, err = s.Dismiss(2, "  not a bug \n")
	if err != nil || !p.Dismissed || p.Note != "not a bug" {
		t.Fatalf("a dismiss with a note: %+v, %v", p, err)
	}
	var notes []any
	rows, err := s.db.Query(`SELECT dismissed FROM proposals ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v any
		rows.Scan(&v)
		notes = append(notes, v)
	}
	rows.Close()
	if !reflect.DeepEqual(notes, []any{"", "not a bug"}) {
		t.Errorf("the notes kept are %q", notes)
	}
	h := mustHistory(t, s, noticed.ID)
	one, two := h[len(h)-2], h[len(h)-1]
	if one.Reason != "dismissed proposal 1" || one.Note() != "" || one.Proposal != 1 {
		t.Errorf("the entry of a dismiss without a note: %+v", one)
	}
	if two.Reason != "dismissed proposal 2: not a bug" || two.Note() != "not a bug" || two.Proposal != 2 {
		t.Errorf("the entry of a dismiss with a note: %+v", two)
	}
	for _, id := range []int64{1, 2} {
		if _, err := s.Dismiss(id, ""); !errors.Is(err, ErrRefused) {
			t.Errorf("a second dismiss of %d: %v", id, err)
		}
		if _, _, err := s.MakeCard(id); !errors.Is(err, ErrRefused) {
			t.Errorf("a make-card after a dismiss of %d: %v", id, err)
		}
	}
	if c, err := s.Card(noticed.ID); err != nil || c != noticed || since() != was {
		t.Errorf("the noticed card is %+v since %s, was %+v since %s", c, since(), noticed, was)
	}
	if open, n, _ := s.OpenProposals(); len(open) != 0 || n != (ProposalCounts{Made: 2, Dismissed: 2}) {
		t.Errorf("the open proposals %+v, %+v", open, n)
	}
	if _, err := s.Dismiss(9, ""); !errors.Is(err, ErrNoProposal) {
		t.Errorf("an unknown proposal: %v", err)
	}
}

// TestProposalEntriesChangeNothing dismisses a proposal on cards whose last
// entry the core reads, and the core reads the same.
func TestProposalEntriesChangeNothing(t *testing.T) {
	dismiss := func(t *testing.T, s *Store, card int64) {
		t.Helper()
		if _, err := s.Dismiss(propose(t, s, card, "x"), "a note"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("a card in its retry", func(t *testing.T) {
		s := open(t)
		c := mustCreate(t, s, "a card")
		if _, err := s.Move(c.ID, State{Spec, Run}, CauseRule); err != nil {
			t.Fatal(err)
		}
		mustAddRun(t, s, theRun(c.ID, "r1"))
		if _, err := s.EndRunByRule("r1", Failed, firstFailed, State{Spec, Developer},
			Rule{To: State{Spec, Run}, Reason: RetryPrefix + firstFailed}, Cost{}); err != nil {
			t.Fatal(err)
		}
		dismiss(t, s, c.ID)
		if first, retrying, err := s.Retrying(c.ID); err != nil || !retrying || first != firstFailed {
			t.Errorf("after a dismiss: retrying %v, %q, %v", retrying, first, err)
		}
	})
	t.Run("a card told a note", func(t *testing.T) {
		s := open(t)
		c := mustCreate(t, s, "a card")
		if _, err := s.Move(c.ID, State{Spec, Approval}, CauseRun); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Decide(c.ID, Again, "Name the table."); err != nil {
			t.Fatal(err)
		}
		dismiss(t, s, c.ID)
		if note, err := s.Note(c.ID); err != nil || note != "Name the table." {
			t.Errorf("after a dismiss: the note %q, %v", note, err)
		}
	})
	t.Run("a dropped card", func(t *testing.T) {
		s := open(t)
		p := mustCreate(t, s, "a predecessor")
		q, err := s.Create("queued", p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Start(q.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Decide(p.ID, Drop, ""); err != nil {
			t.Fatal(err)
		}
		dismiss(t, s, p.ID)
		b, err := s.Board()
		if err != nil || !b[0].Dropped || b[0].Reason != DroppedReason {
			t.Errorf("after a dismiss, the board: %+v, %v", b, err)
		}
		if f, err := readFacts(s.db, q.ID); err != nil || !reflect.DeepEqual(f.Dropped, []int64{p.ID}) {
			t.Errorf("after a dismiss, the queued card reads %+v, %v", f, err)
		}
		if _, err := s.Create("another", p.ID); !errors.Is(err, ErrPredecessor) {
			t.Errorf("a card naming the dropped one: %v", err)
		}
	})
	t.Run("a merged card", func(t *testing.T) {
		s := open(t)
		c := mustCreate(t, s, "a card")
		if _, err := s.Move(c.ID, State{Review, Host}, CauseRule); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MoveWithReason(c.ID, State{Review, Host}, State{Done, Nothing}, CauseHost, MergedReason); err != nil {
			t.Fatal(err)
		}
		b, _ := s.Board()
		want := MeasuresOf(b)
		dismiss(t, s, c.ID)
		b, err := s.Board()
		if err != nil || !b[0].Merged || MeasuresOf(b) != want || want.Merged != 1 {
			t.Errorf("after a dismiss: %+v, the measures %+v, want %+v", b, MeasuresOf(b), want)
		}
	})
	t.Run("a card whose publishing failed", func(t *testing.T) {
		s := open(t)
		c := mustCreate(t, s, "a card")
		if _, err := s.Move(c.ID, State{Review, Host}, CauseRule); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MoveWithReason(c.ID, State{Review, Host}, State{Review, Developer}, CauseRule,
			PublishFailedReason+": the publish command exited with status 1"); err != nil {
			t.Fatal(err)
		}
		dismiss(t, s, c.ID)
		if f, err := readFacts(s.db, c.ID); err != nil || !f.PublishFailed {
			t.Errorf("after a dismiss, the facts: %+v, %v", f, err)
		}
	})
}
