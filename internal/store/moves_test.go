package store

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// The table of moves, written out here and not read from the code, so that a
// row dropped or changed in the code fails the test.
var wantMoves = map[Action]map[State]State{
	"approve":   {{"spec", "approval"}: {"implement", "run"}},
	"again":     {{"spec", "developer"}: {"spec", "run"}, {"implement", "developer"}: {"implement", "run"}, {"review", "developer"}: {"review", "run"}},
	"send-back": {{"review", "developer"}: {"implement", "run"}},
	"done":      {{"review", "approval"}: {"done", "nothing"}},
}

// TestDecide makes every move from every valid state: a row of the table moves
// the card, with an entry of the developer's; anything else, a move that is not
// an action included, is refused and changes nothing.
func TestDecide(t *testing.T) {
	s := open(t)
	actions := []Action{"approve", "again", "send-back", "done", "drop", ""}
	for from := range validPairings {
		for _, action := range actions {
			card := mustCreate(t, s, "a card")
			if _, err := s.Move(card.ID, from, CauseRule); err != nil {
				t.Fatal(err)
			}
			before := len(mustHistory(t, s, card.ID))
			got, err := s.Decide(card.ID, action)
			want, ok := wantMoves[action][from]
			if !ok {
				if !errors.Is(err, ErrRefused) || got.State != from {
					t.Errorf("%s from %v: %+v, %v; want refused with the card's state", action, from, got, err)
				}
				if after, _ := s.Card(card.ID); after.State != from || len(mustHistory(t, s, card.ID)) != before {
					t.Errorf("%s from %v was refused and changed the card to %+v", action, from, after)
				}
				continue
			}
			if err != nil || got.State != want {
				t.Errorf("%s from %v: %+v, %v; want %v", action, from, got, err, want)
				continue
			}
			h := mustHistory(t, s, card.ID)
			if e := h[len(h)-1]; len(h) != before+1 || e.Cause != CauseDeveloper || e.State != want || e.Run != "" {
				t.Errorf("%s from %v wrote %+v", action, from, h[before:])
			}
		}
	}
	if _, err := s.Decide(999, "approve"); err == nil || errors.Is(err, ErrRefused) {
		t.Errorf("a move on no card: %v", err)
	}
}

// TestMoves lists the moves of each state as the table has them.
func TestMoves(t *testing.T) {
	for from := range validPairings {
		var want []Action
		for _, action := range []Action{"approve", "again", "send-back", "done"} {
			if _, ok := wantMoves[action][from]; ok {
				want = append(want, action)
			}
		}
		if got := Moves(from); !reflect.DeepEqual(got, want) {
			t.Errorf("Moves(%v) = %v, want %v", from, got, want)
		}
	}
}

func TestDocuments(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	other := mustCreate(t, s, "another")
	if docs, err := s.Documents(card.ID); err != nil || len(docs) != 0 {
		t.Fatalf("the documents of a new card: %v, %v", docs, err)
	}
	for i, text := range []string{"the first <script>", "the second \x1b[2J"} {
		id := []string{"r1", "r2"}[i]
		if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		mustAddRun(t, s, theRun(card.ID, id))
		if _, err := s.EndRun(id, Finished, "", State{Spec, Approval}, specDoc(text)); err != nil {
			t.Fatal(err)
		}
	}
	// A report and a review, each under its kind (card 14).
	if _, err := s.Move(card.ID, State{Implement, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, "r3"))
	if _, err := s.EndRun("r3", Finished, "", State{Review, Run}, &Doc{KindReport, "the report"}); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, "r4"))
	if _, err := s.EndRun("r4", Finished, "", State{Review, Approval}, &Doc{KindReview, "Verdict: passes\n"}); err != nil {
		t.Fatal(err)
	}
	want := []Document{{"spec", "r1", "the first <script>"}, {"spec", "r2", "the second \x1b[2J"},
		{"report", "r3", "the report"}, {"review", "r4", "Verdict: passes\n"}}
	if docs, err := s.Documents(card.ID); err != nil || !reflect.DeepEqual(docs, want) {
		t.Errorf("Documents = %+v, %v; want %+v", docs, err, want)
	}
	if docs, _ := s.Documents(other.ID); len(docs) != 0 {
		t.Errorf("another card's documents: %+v", docs)
	}
}

func TestBoard(t *testing.T) {
	s := open(t)
	if b, err := s.Board(); err != nil || len(b) != 0 {
		t.Fatalf("the board of a new store: %+v, %v", b, err)
	}
	waiting := mustCreate(t, s, "waits\nfor a run")
	if _, err := s.Move(waiting.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	ended := mustCreate(t, s, "has a spec")
	if _, err := s.Move(ended.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(ended.ID, "r1"))
	if _, err := s.EndRun("r1", Failed, "exit 1", State{Spec, Developer}, specDoc("# Spec")); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	mustAddRun(t, s, theRun(waiting.ID, "r2"))

	b, err := s.Board()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 2 || b[0].ID != waiting.ID || b[1].ID != ended.ID {
		t.Fatalf("the board: %+v", b)
	}
	if w := b[0]; w.Text != "waits\nfor a run" || w.State != (State{Spec, Run}) || w.Reason != "" || w.Kinds != nil ||
		w.InFlight == nil || w.InFlight.RunRow != theRun(waiting.ID, "r2") || w.InFlight.Started.Before(before.Add(-time.Second)) {
		t.Errorf("the card waiting for its run: %+v, in flight %+v", w, w.InFlight)
	}
	if e := b[1]; e.Reason != "exit 1" || e.InFlight != nil || !reflect.DeepEqual(e.Kinds, []string{"spec"}) {
		t.Errorf("the card with a spec: %+v", e)
	}
	// After a move the reason is the new entry's, which has none.
	if _, err := s.Decide(ended.ID, Again); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Board(); b[1].Reason != "" {
		t.Errorf("the reason after a move: %q", b[1].Reason)
	}

	runs, err := s.Runs(ended.ID)
	if err != nil || len(runs) != 1 || runs[0].ID != "r1" || runs[0].Ending != Failed || runs[0].Started.IsZero() {
		t.Errorf("Runs = %+v, %v", runs, err)
	}
}
