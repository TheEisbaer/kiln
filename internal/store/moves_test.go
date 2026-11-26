package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// row is a row of the table of moves as the test writes it: where the move
// goes, and what it takes of a note.
type row struct {
	to   State
	note Note
}

// The table of moves, written out here and not read from the code, so that a
// row dropped or changed in the code fails the test.
var wantMoves = map[Action]map[State]row{
	"approve": {{"spec", "approval"}: {State{"implement", "run"}, "optional"}},
	"start":   {{"backlog", "developer"}: {State{"spec", "run"}, "none"}},
	"again": {
		{"spec", "approval"}:       {State{"spec", "run"}, "required"},
		{"spec", "developer"}:      {State{"spec", "run"}, "optional"},
		{"implement", "developer"}: {State{"implement", "run"}, "optional"},
		{"review", "developer"}:    {State{"review", "run"}, "optional"},
	},
	"send-back": {
		{"review", "developer"}: {State{"implement", "run"}, "optional"},
		{"review", "approval"}:  {State{"implement", "run"}, "required"},
		{"review", "host"}:      {State{"implement", "run"}, "required"},
	},
	"done": {
		{"review", "approval"}: {State{"done", "nothing"}, "none"},
		{"review", "host"}:     {State{"done", "nothing"}, "none"},
	},
	"to-spec": {
		{"implement", "developer"}: {State{"spec", "run"}, "optional"},
		{"review", "developer"}:    {State{"spec", "run"}, "optional"},
	},
	"drop": func() map[State]row {
		m := map[State]row{}
		for from := range validPairings {
			if from.Stage != "done" {
				m[from] = row{State{"done", "nothing"}, "optional"}
			}
		}
		return m
	}(),
}

// TestDecide makes every move from every valid state, without a note and with
// one: a row of the table moves the card, with an entry of the developer's
// whose reason is the note, trimmed, or for a drop the words of a drop and the
// note; a note that does not fit the row is refused; anything else, a move
// that is not an action included, is refused. A refused move changes nothing.
func TestDecide(t *testing.T) {
	s := open(t)
	// Without a publish command the table has no publish, and a card at the
	// Git host, which a core with one left there, has its moves.
	actions := []Action{"approve", "again", "send-back", "done", "to-spec", "drop", "publish", "start", "to-review", ""}
	for from := range validPairings {
		for _, action := range actions {
			for _, note := range []string{"", " \n ", "  Use the store's words.\n"} {
				card := mustCreate(t, s, "a card")
				if _, err := s.Move(card.ID, from, CauseRule); err != nil {
					t.Fatal(err)
				}
				before := len(mustHistory(t, s, card.ID))
				// The cards put in progress before are many: the repo's
				// limit is not this test's subject.
				got, err := s.DecideWith(card.ID, action, note, Options{Limits: Limits{Repos: map[string]int{KilnRepo: 1000}, Instance: 1000}})
				want, ok := wantMoves[action][from]
				trimmed := strings.TrimSpace(note)
				refused := func(sentinel error) {
					t.Helper()
					if !errors.Is(err, sentinel) || got.State != from {
						t.Errorf("%s from %v with %q: %+v, %v; want %v with the card's state", action, from, note, got, err, sentinel)
					}
					if after, _ := s.Card(card.ID); after.State != from || len(mustHistory(t, s, card.ID)) != before {
						t.Errorf("%s from %v was refused and changed the card to %+v", action, from, after)
					}
				}
				switch {
				case !ok:
					refused(ErrRefused)
					continue
				case action == "approve" && trimmed != "":
					// These cards have no spec to keep the note with.
					refused(ErrRefused)
					continue
				case want.note == "required" && trimmed == "", want.note == "none" && trimmed != "":
					refused(ErrNote)
					continue
				}
				if err != nil || got.State != want.to {
					t.Errorf("%s from %v with %q: %+v, %v; want %v", action, from, note, got, err, want.to)
					continue
				}
				reason := trimmed
				if action == "drop" {
					reason = "dropped by the developer"
					if trimmed != "" {
						reason += ": " + trimmed
					}
				}
				h := mustHistory(t, s, card.ID)
				e := h[len(h)-1]
				if len(h) != before+1 || e.Cause != CauseDeveloper || e.State != want.to || e.Run != "" || e.Reason != reason {
					t.Errorf("%s from %v with %q wrote %+v", action, from, note, h[before:])
				}
				if e.Note() != trimmed {
					t.Errorf("%s from %v: the entry's note is %q, want %q", action, from, e.Note(), trimmed)
				}
			}
		}
	}
	if _, err := s.Decide(999, "approve", ""); err == nil || errors.Is(err, ErrRefused) {
		t.Errorf("a move on no card: %v", err)
	}
}

// TestDecideNoteSize takes a note of 16 KiB, trimmed, and refuses a longer one.
func TestDecideNoteSize(t *testing.T) {
	s := open(t)
	for _, tc := range []struct {
		note string
		ok   bool
	}{
		{strings.Repeat("a", 16<<10), true},
		{"\n  " + strings.Repeat("a", 16<<10) + "  \n", true},
		{strings.Repeat("a", 16<<10+1), false},
	} {
		card := mustCreate(t, s, "a card")
		if _, err := s.Move(card.ID, State{Spec, Approval}, CauseRule); err != nil {
			t.Fatal(err)
		}
		got, err := s.Decide(card.ID, Again, tc.note)
		if tc.ok != (err == nil) || (!tc.ok && (!errors.Is(err, ErrNote) || got.State != (State{Spec, Approval}))) {
			t.Errorf("a note of %d bytes: %+v, %v", len(tc.note), got, err)
		}
	}
}

// TestDropInFlight refuses to drop a card with a run in flight, from each
// state a run is in flight in, and drops it once the run has ended.
func TestDropInFlight(t *testing.T) {
	s := open(t)
	for i, from := range []State{{Spec, Run}, {Implement, Run}, {Review, Run}} {
		card := mustCreate(t, s, "a card")
		if _, err := s.Move(card.ID, from, CauseRule); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("r%d", i)
		mustAddRun(t, s, theRun(card.ID, id))
		if got := Moves(from, Facts{InFlight: true}); len(got) != 0 {
			t.Errorf("the moves from %v with a run in flight are %v", from, got)
		}
		if got, err := s.Decide(card.ID, Drop, "not needed"); !errors.Is(err, ErrRefused) || got.State != from {
			t.Errorf("drop from %v with a run in flight: %+v, %v", from, got, err)
		}
		if _, err := s.EndRun(id, Failed, "exit 1", State{from.Stage, Developer}, Cost{}, nil); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Decide(card.ID, Drop, ""); err != nil || got.State != (State{Done, Nothing}) {
			t.Errorf("drop from %v after the run: %+v, %v", from, got, err)
		}
	}
}

// TestAddRunWaits refuses a run of a card that does not wait for one, such as
// a card dropped after the tick read it.
func TestAddRunWaits(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(card.ID, Drop, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRun(theRun(card.ID, "r1"), 8); err == nil {
		t.Error("a run of a dropped card was written")
	}
	if r, _ := s.InFlight(card.ID); r != nil {
		t.Errorf("a run is in flight: %+v", r)
	}
}

// TestNote reads the note a card's next run is told: the developer's note on
// the entry that made the card wait for a run, and nothing otherwise.
func TestNote(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	note := func(want string) {
		t.Helper()
		if got, err := s.Note(card.ID); err != nil || got != want {
			t.Errorf("Note = %q, %v; want %q", got, err, want)
		}
	}
	note("")
	if _, err := s.Move(card.ID, State{Spec, Approval}, CauseRun); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(card.ID, Again, " Name the table. "); err != nil {
		t.Fatal(err)
	}
	note("Name the table.")
	mustAddRun(t, s, theRun(card.ID, "r1"))
	note("Name the table.")
	if _, err := s.EndRun("r1", Finished, "exit 0", State{Spec, Approval}, Cost{}, specDoc("# Spec")); err != nil {
		t.Fatal(err)
	}
	note("")
	if _, err := s.Decide(card.ID, Approve, ""); err != nil {
		t.Fatal(err)
	}
	note("")
	// A run's reason is not a note.
	if _, err := s.MoveWithReason(card.ID, State{Implement, Run}, State{Implement, Developer}, CauseRule, "held"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(card.ID, State{Implement, Run}, CauseRule); err != nil {
		t.Fatal(err)
	}
	note("")
	if _, err := s.Note(999); !errors.Is(err, ErrNoCard) {
		t.Errorf("the note of no card: %v", err)
	}
}

// TestMoves lists the moves of each state as the table has them, without a
// run in flight.
func TestMoves(t *testing.T) {
	for from := range validPairings {
		var want []Allowed
		for _, action := range []Action{"start", "approve", "again", "done", "send-back", "to-spec", "drop"} {
			if r, ok := wantMoves[action][from]; ok {
				want = append(want, Allowed{action, r.note})
			}
		}
		if got := Moves(from, Facts{}); !reflect.DeepEqual(got, want) {
			t.Errorf("Moves(%v) = %v, want %v", from, got, want)
		}
	}
}

// TestMovesPullClosed offers publish from "needs you" after a closed pull
// request only with a publish command, and no other move changes.
func TestMovesPullClosed(t *testing.T) {
	has := func(f Facts, a Action) bool {
		return slices.ContainsFunc(Moves(State{Review, Developer}, f), func(m Allowed) bool { return m.Action == a })
	}
	if !has(Facts{Publishing: true, PullClosed: true}, Publish) {
		t.Error("no publish after a closed pull request with a publish command")
	}
	if has(Facts{PullClosed: true}, Publish) {
		t.Error("publish after a closed pull request without a publish command")
	}
	if has(Facts{Publishing: true}, Publish) {
		t.Error("publish from needs you without a failure or a closed pull request")
	}
	for _, a := range []Action{Again, SendBack, ToSpec, Drop} {
		if !has(Facts{Publishing: true, PullClosed: true}, a) {
			t.Errorf("no %s after a closed pull request", a)
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
		if _, err := s.EndRun(id, Finished, "", State{Spec, Approval}, Cost{}, specDoc(text)); err != nil {
			t.Fatal(err)
		}
	}
	// A report and a review, each under its kind.
	if _, err := s.Move(card.ID, State{Implement, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, "r3"))
	if _, err := s.EndRun("r3", Finished, "", State{Review, Run}, Cost{}, &Doc{Kind: KindReport, Text: "the report"}); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, "r4"))
	if _, err := s.EndRun("r4", Finished, "", State{Review, Approval}, Cost{}, &Doc{Kind: KindReview, Text: "Verdict: passes\n"}); err != nil {
		t.Fatal(err)
	}
	want := []Document{{"spec", "r1", "the first <script>", ""}, {"spec", "r2", "the second \x1b[2J", ""},
		{"report", "r3", "the report", ""}, {"review", "r4", "Verdict: passes\n", ""}}
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
	if _, err := s.EndRun("r1", Failed, "exit 1", State{Spec, Developer}, Cost{}, specDoc("# Spec")); err != nil {
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
	if _, err := s.Decide(ended.ID, Again, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Board(); b[1].Reason != "" {
		t.Errorf("the reason after a move: %q", b[1].Reason)
	}

	// A dropped card is told apart from a card marked done.
	finished := mustCreate(t, s, "finished")
	if _, err := s.Move(finished.ID, State{Review, Approval}, CauseRun); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(finished.ID, MarkDone, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ended.ID, Drop, "not needed"); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Board(); !b[1].Dropped || b[1].Reason != "dropped by the developer: not needed" || b[2].Dropped || b[0].Dropped {
		t.Errorf("the dropped card %+v, the finished one %+v", b[1], b[2])
	}

	runs, err := s.Runs(ended.ID)
	if err != nil || len(runs) != 1 || runs[0].ID != "r1" || runs[0].Ending != Failed || runs[0].Started.IsZero() {
		t.Errorf("Runs = %+v, %v", runs, err)
	}
}

// TestApproveNote keeps the note of an approval on the spec's row, and nowhere
// else: the entry has no reason, and the next run is not told it as a
// send-back note.
func TestApproveNote(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	for i, id := range []string{"r1", "r2"} {
		if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		mustAddRun(t, s, theRun(card.ID, id))
		if _, err := s.EndRun(id, Finished, "", State{Spec, Approval}, Cost{}, specDoc("spec "+id)); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := s.Decide(card.ID, Again, "again"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Refused for a note that is too long: nothing is written.
	if _, err := s.Decide(card.ID, Approve, strings.Repeat("a", MaxNote+1)); !errors.Is(err, ErrNote) {
		t.Fatalf("a long note: %v", err)
	}
	before := len(mustHistory(t, s, card.ID))
	got, err := s.Decide(card.ID, Approve, "  Write no card number.\n")
	if err != nil || got.State != (State{Implement, Run}) {
		t.Fatalf("Decide: %+v, %v", got, err)
	}
	if sp, err := s.Spec(card.ID); err != nil || sp.Note != "Write no card number." || sp.Run != "r2" {
		t.Errorf("Spec = %+v, %v", sp, err)
	}
	docs, err := s.Documents(card.ID)
	if err != nil || len(docs) != 2 || docs[0].Note != "" || docs[1].Note != "Write no card number." {
		t.Errorf("Documents = %+v, %v", docs, err)
	}
	h := mustHistory(t, s, card.ID)
	if len(h) != before+1 || h[len(h)-1].Reason != "" {
		t.Errorf("the approval's entry: %+v", h[before:])
	}
	if n, err := s.Note(card.ID); err != nil || n != "" {
		t.Errorf("Note = %q, %v; want none", n, err)
	}
	if d, err := s.Newest(card.ID, KindSpec); err != nil || d.Note != "" {
		t.Errorf("Newest = %+v, %v; it does not read the note", d, err)
	}
}

// approvedCard makes a card with a spec of the run id, approved, and moved to
// the state to.
func approvedCard(t *testing.T, s *Store, id string, to State) Card {
	t.Helper()
	card := mustCreate(t, s, "a card")
	if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, id))
	if _, err := s.EndRun(id, Finished, "", State{Spec, Approval}, Cost{}, specDoc("spec "+id)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(card.ID, Approve, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(card.ID, to, CauseRun); err != nil {
		t.Fatal(err)
	}
	return card
}

// TestCorrections: a send-back or an again with a note from implement or
// review writes a row with the newest spec's run; no other move does, and a
// refused one writes nothing.
func TestCorrections(t *testing.T) {
	s := open(t)
	card := approvedCard(t, s, "r1", State{Review, Developer})
	list := func() []Correction {
		t.Helper()
		l, err := s.Corrections(card.ID)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if len(list()) != 0 {
		t.Fatalf("corrections before any move: %+v", list())
	}
	// A move without a note, and a refused one, write none.
	if _, err := s.Decide(card.ID, Again, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(card.ID, SendBack, "refused: a run is awaited"); !errors.Is(err, ErrRefused) {
		t.Fatalf("a send-back from review waiting for a run: %v", err)
	}
	if len(list()) != 0 {
		t.Fatalf("corrections after a move without a note and a refused one: %+v", list())
	}
	if _, err := s.Move(card.ID, State{Review, Developer}, CauseRun); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(card.ID, SendBack, " Note A. "); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(card.ID, State{Implement, Developer}, CauseRun); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(card.ID, Again, "Note B."); err != nil {
		t.Fatal(err)
	}
	want := []Correction{
		{SendBack, State{Review, Developer}, "Note A."},
		{Again, State{Implement, Developer}, "Note B."},
	}
	if got := list(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Corrections = %+v, want %+v", got, want)
	}
	// A drop with a note writes none.
	if _, err := s.Decide(card.ID, Drop, "no more"); err != nil {
		t.Fatal(err)
	}
	if got := list(); len(got) != 2 {
		t.Errorf("after a drop: %+v", got)
	}
}

// TestCorrectionsSpec: an again with a note from a spec writes none, and a new
// spec ends the notes.
func TestCorrectionsSpec(t *testing.T) {
	s := open(t)
	card := approvedCard(t, s, "r1", State{Review, Developer})
	if _, err := s.Decide(card.ID, SendBack, "Note A."); err != nil {
		t.Fatal(err)
	}
	if l, err := s.Corrections(card.ID); err != nil || len(l) != 1 {
		t.Fatalf("Corrections = %+v, %v", l, err)
	}
	if _, err := s.Move(card.ID, State{Spec, Approval}, CauseRun); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(card.ID, Again, "Spec note."); err != nil {
		t.Fatal(err)
	}
	if l, err := s.Corrections(card.ID); err != nil || len(l) != 1 {
		t.Fatalf("an again from spec wrote a row: %+v, %v", l, err)
	}
	mustAddRun(t, s, theRun(card.ID, "r2"))
	if _, err := s.EndRun("r2", Finished, "", State{Spec, Approval}, Cost{}, specDoc("spec r2")); err != nil {
		t.Fatal(err)
	}
	if l, err := s.Corrections(card.ID); err != nil || len(l) != 0 {
		t.Errorf("after a new spec: %+v, %v", l, err)
	}
}

// TestMovesToReview offers to-review, between again and to-spec, from
// implement needing you only with both facts, and from no other state.
func TestMovesToReview(t *testing.T) {
	from := State{Implement, Developer}
	for _, f := range []Facts{{}, {Unchanged: true}, {HasChange: true}} {
		for _, m := range Moves(from, f) {
			if m.Action == ToReview {
				t.Errorf("Moves(%v, %+v) lists to-review", from, f)
			}
		}
	}
	want := []Allowed{{Again, OptionalNote}, {ToReview, OptionalNote}, {ToSpec, OptionalNote}, {Drop, OptionalNote}}
	if got := Moves(from, Facts{Unchanged: true, HasChange: true}); !reflect.DeepEqual(got, want) {
		t.Errorf("Moves with both facts = %v, want %v", got, want)
	}
	for st := range validPairings {
		if st == from {
			continue
		}
		for _, m := range Moves(st, Facts{Unchanged: true, HasChange: true}) {
			if m.Action == ToReview {
				t.Errorf("Moves(%v) lists to-review", st)
			}
		}
	}
}

// TestToReviewDecide makes the move after a run that changed nothing, with the
// change's fact, and keeps the note as a correction; without the fact, or after
// a failed run or the rule's entry, it is refused and writes nothing.
func TestToReviewDecide(t *testing.T) {
	s := open(t)
	// ended makes an approved card whose implement run ended in implement,
	// needing you, with the reason.
	ended := func(id string, ending Ending, reason string) Card {
		t.Helper()
		card := approvedCard(t, s, id, State{Implement, Run})
		mustAddRun(t, s, theRun(card.ID, id+"-i"))
		if _, err := s.EndRun(id+"-i", ending, reason, State{Implement, Developer}, Cost{}); err != nil {
			t.Fatal(err)
		}
		return card
	}
	refused := func(card Card, o Options) {
		t.Helper()
		before := len(mustHistory(t, s, card.ID))
		if _, err := s.DecideWith(card.ID, ToReview, "A note.", o); !errors.Is(err, ErrRefused) {
			t.Errorf("to-review with %+v: %v, want ErrRefused", o, err)
		}
		if c, _ := s.Card(card.ID); c.State != (State{Implement, Developer}) || len(mustHistory(t, s, card.ID)) != before {
			t.Errorf("a refused to-review changed the card to %+v", c)
		}
		if l, _ := s.Corrections(card.ID); len(l) != 0 {
			t.Errorf("a refused to-review wrote %+v", l)
		}
	}

	card := ended("u1", Finished, UnchangedReason+"; exit 0, no commit")
	refused(card, Options{})
	before := len(mustHistory(t, s, card.ID))
	interventions := Interventions(mustHistory(t, s, card.ID))
	got, err := s.DecideWith(card.ID, ToReview, "  Built already; review it.\n", Options{HasChange: true})
	if err != nil || got.State != (State{Review, Run}) {
		t.Fatalf("to-review: %+v, %v", got, err)
	}
	h := mustHistory(t, s, card.ID)
	if e := h[len(h)-1]; len(h) != before+1 || e.Cause != CauseDeveloper || e.Reason != "Built already; review it." {
		t.Errorf("the entry is %+v", e)
	}
	want := []Correction{{ToReview, State{Implement, Developer}, "Built already; review it."}}
	if l, err := s.Corrections(card.ID); err != nil || !reflect.DeepEqual(l, want) {
		t.Errorf("Corrections = %+v, %v, want %+v", l, err, want)
	}
	if n := Interventions(h); n != interventions+1 {
		t.Errorf("interventions %d, want %d", n, interventions+1)
	}

	refused(ended("u2", Failed, "exit 1, no result, saved"), Options{HasChange: true})
	rule := approvedCard(t, s, "u3", State{Implement, Developer})
	if _, err := s.Move(rule.ID, State{Implement, Developer}, CauseRule); err != nil {
		t.Fatal(err)
	}
	refused(rule, Options{HasChange: true})
}
