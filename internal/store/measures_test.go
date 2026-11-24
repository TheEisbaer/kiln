package store

import (
	"fmt"
	"testing"
)

func TestInterventions(t *testing.T) {
	type step func(t *testing.T, s *Store, id int64)
	move := func(to State, cause Cause) step {
		return func(t *testing.T, s *Store, id int64) {
			t.Helper()
			if _, err := s.Move(id, to, cause); err != nil {
				t.Fatal(err)
			}
		}
	}
	start := move(State{Spec, Run}, CauseDeveloper)
	rule := func(to State) step { return move(to, CauseRule) }
	decide := func(a Action, note string) step {
		return func(t *testing.T, s *Store, id int64) {
			t.Helper()
			if _, err := s.Decide(id, a, note); err != nil {
				t.Fatalf("%s: %v", a, err)
			}
		}
	}
	// specWritten ends a spec run with its document, which a note on the
	// approval is kept with.
	specWritten := func(run string) step {
		return func(t *testing.T, s *Store, id int64) {
			t.Helper()
			mustAddRun(t, s, theRun(id, run))
			if _, err := s.EndRun(run, Finished, "ok", State{Spec, Approval}, Cost{}, &Doc{Kind: KindSpec, Text: "a spec"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	specApproval := rule(State{Spec, Approval})
	approved := []step{start, specApproval, decide(Approve, "")}
	with := func(a []step, more ...step) []step { return append(append([]step{}, a...), more...) }

	rows := []struct {
		name  string
		steps []step
		want  int
	}{
		{"written", nil, 0},
		{"written and started", []step{start}, 0},
		{"first approval", approved, 0},
		{"first approval with a note", []step{start, specWritten("r1"), decide(Approve, "mind the edge")}, 0},
		{"spec sent back, then approved", []step{start, specApproval, decide(Again, "shorter"), specApproval, decide(Approve, "")}, 0},
		{"second approval", with(approved, rule(State{Implement, Developer}), decide(ToSpec, ""), specApproval, decide(Approve, "")), 2},
		{"again from needs you", with(approved, rule(State{Implement, Developer}), decide(Again, "")), 1},
		{"send-back of a passed review", with(approved, rule(State{Review, Approval}), decide(SendBack, "fix it")), 1},
		{"send-back from the Git host", with(approved, rule(State{Review, Host}), decide(SendBack, "fix it")), 1},
		{"publish", with(approved, rule(State{Review, Approval}), func(t *testing.T, s *Store, id int64) {
			t.Helper()
			if _, err := s.DecideWith(id, Publish, "", Options{Publishing: true}); err != nil {
				t.Fatal(err)
			}
		}), 1},
		{"done", with(approved, rule(State{Review, Approval}), decide(MarkDone, "")), 1},
		{"drop", with(approved, rule(State{Implement, Developer}), decide(Drop, "")), 1},
		{"drop from the backlog", []step{decide(Drop, "")}, 1},
		{"runs and the rule only", with(approved, rule(State{Implement, Run}), rule(State{Review, Run}), rule(State{Review, Host}),
			func(t *testing.T, s *Store, id int64) {
				t.Helper()
				if _, err := s.MoveWithReason(id, State{Review, Host}, State{Done, Nothing}, CauseHost, MergedReason); err != nil {
					t.Fatal(err)
				}
			}), 0},
		{"automatic merging turned on", with(approved, func(t *testing.T, s *Store, id int64) {
			t.Helper()
			if _, err := s.TurnMergingOff("an update failed", "abc"); err != nil {
				t.Fatal(err)
			}
			if err := s.TurnMergingOn(); err != nil {
				t.Fatal(err)
			}
		}), 0},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			s := open(t)
			c := mustCreate(t, s, "a card")
			for _, st := range r.steps {
				st(t, s, c.ID)
			}
			if got := Interventions(mustHistory(t, s, c.ID)); got != r.want {
				t.Errorf("%d interventions, want %d, in the history:\n%s", got, r.want, fmt.Sprint(mustHistory(t, s, c.ID)))
			}
		})
	}
}

func TestMeasuresOf(t *testing.T) {
	got := MeasuresOf([]Summary{
		{Merged: true, Interventions: 1},
		{Merged: true, Interventions: 2},
		{Dropped: true, Interventions: 4},
		{Interventions: 8},
		{},
	})
	if want := (Measures{Merged: 2, Interventions: 3, Dropped: 1}); got != want {
		t.Errorf("%+v, want %+v", got, want)
	}
	if got := MeasuresOf(nil); got != (Measures{}) {
		t.Errorf("an empty board: %+v", got)
	}
}
