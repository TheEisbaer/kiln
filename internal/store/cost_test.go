package store

import (
	"path/filepath"
	"reflect"
	"testing"
)

func ptr(f float64) *float64 { return &f }

// costOf reads the cost and the session of a run, as the table keeps them.
func costOf(t *testing.T, s *Store, id string) Cost {
	t.Helper()
	runs, err := s.Runs(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.ID == id {
			return r.Cost
		}
	}
	t.Fatalf("no run %s", id)
	return Cost{}
}

func TestRunCost(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, "r1"))
	if _, err := s.EndRun("r1", Finished, "ok", State{Spec, Approval}, Cost{USD: ptr(0.25), Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	if got := costOf(t, s, "r1"); got.USD == nil || *got.USD != 0.25 || got.Session != "s1" {
		t.Errorf("r1 cost %+v, %v", got, got.USD)
	}
	// A second end of the same run changes nothing.
	if _, err := s.EndRun("r1", Finished, "ok", State{Spec, Approval}, Cost{USD: ptr(9), Session: "s2"}); err != nil {
		t.Fatal(err)
	}
	if got := costOf(t, s, "r1"); *got.USD != 0.25 || got.Session != "s1" {
		t.Errorf("r1 cost after a repeat: %+v", got)
	}

	// No cost is NULL, not 0.
	if _, err := s.Move(card.ID, State{Implement, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, "r2"))
	if _, err := s.EndRun("r2", Failed, "no result", State{Implement, Developer}, Cost{}); err != nil {
		t.Fatal(err)
	}
	if got := costOf(t, s, "r2"); got.USD != nil || got.Session != "" {
		t.Errorf("r2 cost %+v, want unknown", got)
	}

	// The same for a run that ends by the rule.
	if _, err := s.Move(card.ID, State{Implement, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, "r3"))
	rule := Rule{To: State{Review, Developer}, Reason: "by the rule"}
	if _, err := s.EndRunByRule("r3", Finished, "ok", State{Implement, Developer}, rule, Cost{USD: ptr(0.5), Session: "s3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndRunByRule("r3", Finished, "ok", State{Implement, Developer}, rule, Cost{USD: ptr(7), Session: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := costOf(t, s, "r3"); *got.USD != 0.5 || got.Session != "s3" {
		t.Errorf("r3 cost %+v", got)
	}
}

func TestCostOf(t *testing.T) {
	run := func(stage Stage, usd *float64, session string) RunAt {
		return RunAt{RunRow: RunRow{Ending: Finished}, Stage: stage, Cost: Cost{USD: usd, Session: session}}
	}
	flight := RunAt{Stage: Spec, Cost: Cost{USD: ptr(5), Session: "A"}}
	for name, tc := range map[string]struct {
		runs []RunAt
		want *CardCost
	}{
		"a single run": {
			[]RunAt{run(Spec, ptr(0.5), "A")},
			&CardCost{Total: Part{0.5, 1, 0}, Spec: Part{0.5, 1, 0}},
		},
		"a resumed run counts what it added": {
			[]RunAt{run(Spec, ptr(0.5), "A"), run(Spec, ptr(0.8), "A")},
			&CardCost{Total: Part{0.8, 2, 0}, Spec: Part{0.8, 2, 0}},
		},
		"two sessions": {
			[]RunAt{run(Spec, ptr(0.5), "A"), run(Implement, ptr(0.3), "B")},
			&CardCost{Total: Part{0.8, 2, 0}, Spec: Part{0.5, 1, 0}, Implement: Part{0.3, 1, 0}},
		},
		"less than the earlier run is unknown": {
			[]RunAt{run(Spec, ptr(0.5), "A"), run(Spec, ptr(0.4), "A")},
			&CardCost{Total: Part{0.5, 2, 1}, Spec: Part{0.5, 2, 1}},
		},
		"an unknown run between runs of a session": {
			[]RunAt{run(Spec, ptr(0.5), "A"), run(Spec, nil, ""), run(Spec, ptr(0.8), "A")},
			&CardCost{Total: Part{0.8, 3, 1}, Spec: Part{0.8, 3, 1}},
		},
		"a run in flight counts nothing": {
			[]RunAt{run(Spec, ptr(0.5), "A"), flight},
			&CardCost{Total: Part{0.5, 1, 0}, Spec: Part{0.5, 1, 0}},
		},
		"a run with no stage counts in the total only": {
			[]RunAt{run("", ptr(0.5), "A"), run(Review, ptr(0.25), "")},
			&CardCost{Total: Part{0.75, 2, 0}, Review: Part{0.25, 1, 0}},
		},
		"runs with no session count whole": {
			[]RunAt{run(Spec, ptr(0.5), ""), run(Spec, ptr(0.8), "")},
			&CardCost{Total: Part{1.3, 2, 0}, Spec: Part{1.3, 2, 0}},
		},
		"no ended run":  {[]RunAt{flight}, nil},
		"no run at all": {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := CostOf(tc.runs)
			if tc.want == nil {
				if got != nil {
					t.Errorf("got %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil")
			}
			if !near(*got, *tc.want) {
				t.Errorf("got %+v, want %+v", *got, *tc.want)
			}
		})
	}
}

func near(a, b CardCost) bool {
	close := func(x, y Part) bool {
		d := x.USD - y.USD
		return d < 1e-9 && d > -1e-9 && x.Runs == y.Runs && x.Unknown == y.Unknown
	}
	return close(a.Total, b.Total) && close(a.Spec, b.Spec) && close(a.Implement, b.Implement) && close(a.Review, b.Review)
}

func TestMigrateV11(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`
ALTER TABLE runs DROP COLUMN stage;
ALTER TABLE runs DROP COLUMN cost;
ALTER TABLE runs DROP COLUMN session;
ALTER TABLE runs DROP COLUMN unread;
INSERT INTO cards (id, text, stage, waits_for, since) VALUES (1, 'a card', 'review', 'run', '2026-11-20T10:00:00Z');
INSERT INTO entries (number, card, at, cause, stage, waits_for, run, reason) VALUES
	(1, 1, '2026-11-20T10:00:00Z', 'developer', 'backlog', 'developer', NULL, NULL),
	(2, 1, '2026-11-20T10:00:01Z', 'developer', 'spec', 'run', NULL, NULL),
	(3, 1, '2026-11-20T10:01:00Z', 'run', 'spec', 'approval', 'r1', 'ok'),
	(4, 1, '2026-11-20T10:02:00Z', 'developer', 'implement', 'run', NULL, NULL),
	(5, 1, '2026-11-20T10:03:00Z', 'run', 'review', 'run', 'r2', 'ok');
INSERT INTO runs (id, card, commit_id, model, effort, ending) VALUES
	('r1', 1, 'c0ffee', 'm', 'high', 'finished'),
	('r2', 1, 'c0ffee', 'm', 'high', 'finished'),
	('r3', 1, 'c0ffee', 'm', 'high', NULL);
DROP TABLE predecessors;
PRAGMA user_version = 11`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = reopen(t, path)
	runs, err := s.Runs(1)
	if err != nil {
		t.Fatal(err)
	}
	var stages []Stage
	var costs []*float64
	for _, r := range runs {
		stages = append(stages, r.Stage)
		costs = append(costs, r.Cost.USD)
	}
	if want := []Stage{Spec, Implement, Review}; !reflect.DeepEqual(stages, want) {
		t.Errorf("stages %v, want %v", stages, want)
	}
	if !reflect.DeepEqual(costs, []*float64{nil, nil, nil}) {
		t.Errorf("costs %v, want all unknown", costs)
	}
	ids, err := s.UnreadCosts()
	if err != nil || !reflect.DeepEqual(ids, []string{"r1", "r2"}) {
		t.Errorf("unread %v, %v, want r1 and r2", ids, err)
	}
}

func TestFillCosts(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, "r1"))
	// A run ended by this version is read already: nothing is filled over it.
	if _, err := s.EndRun("r1", Finished, "ok", State{Spec, Approval}, Cost{USD: ptr(1), Session: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := s.FillCosts(map[string]Cost{"r1": {USD: ptr(2), Session: "t"}}); err != nil {
		t.Fatal(err)
	}
	if got := costOf(t, s, "r1"); *got.USD != 1 || got.Session != "s" {
		t.Errorf("r1 after FillCosts: %+v", got)
	}
}

func TestBoardCostOfRuns(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	if _, err := s.Move(card.ID, State{Spec, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, "r1"))
	if b, err := s.Board(); err != nil || b[0].Cost != nil || b[0].InFlight == nil {
		t.Fatalf("a card with a run in flight: %+v, %v", b, err)
	}
	if _, err := s.EndRun("r1", Finished, "ok", State{Spec, Approval}, Cost{USD: ptr(0.5), Session: "s"}); err != nil {
		t.Fatal(err)
	}
	b, err := s.Board()
	if err != nil || b[0].Cost == nil || b[0].Cost.Spec != (Part{0.5, 1, 0}) {
		t.Fatalf("the board's cost: %+v, %v", b, err)
	}
}
