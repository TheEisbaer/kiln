package tick

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

// TestFillCosts reads the cost of runs that ended before the store kept it:
// once, from the record, and a run without a readable record stays unknown.
func TestFillCosts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	runs := filepath.Join(dir, "runs")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	card, err := s.Create("a card")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(card.ID, store.State{Stage: store.Spec, WaitsFor: store.Run}, store.CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := s.AddRun(store.RunRow{ID: id, Card: card.ID, Commit: "c0ffee", Model: "m", Effort: "high"}, 4); err != nil {
			// one run in flight per card: end the one before
			t.Fatal(err)
		}
		if id == "d" {
			break
		}
		if _, err := s.EndRun(id, store.Finished, "ok", store.State{Stage: store.Spec, WaitsFor: store.Run}, store.Cost{}); err != nil {
			t.Fatal(err)
		}
	}
	// Those that ended before this version are unread.
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE runs SET unread = 1 WHERE ending IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	record := func(id, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(runs, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runs, id, "result.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	record("a", `{"result": {"subtype": "success", "total_cost_usd": 0.5, "session_id": "sa"}}`)
	record("c", `not json`)
	record("d", `{"result": {"subtype": "success", "total_cost_usd": 7, "session_id": "sd"}}`)

	var out bytes.Buffer
	fillCosts(s, runs, &out)
	if !strings.Contains(out.String(), "read the cost of 3 earlier runs from their records; 2 have no cost\n") ||
		strings.Count(out.String(), "the cost of run c is not read") != 1 {
		t.Errorf("the core printed\n%s", out.String())
	}
	check := func(want map[string]*float64) {
		t.Helper()
		rs, err := s.Runs(card.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			w := want[r.ID]
			if (w == nil) != (r.Cost.USD == nil) || (w != nil && *w != *r.Cost.USD) {
				t.Errorf("run %s has the cost %v, want %v", r.ID, r.Cost.USD, w)
			}
		}
		if ids, err := s.UnreadCosts(); err != nil || len(ids) != 0 {
			t.Errorf("unread runs %v, %v", ids, err)
		}
	}
	half := 0.5
	check(map[string]*float64{"a": &half})
	if rs, _ := s.Runs(card.ID); rs[0].Cost.Session != "sa" {
		t.Errorf("run a has the session %q", rs[0].Cost.Session)
	}

	// Once: a changed record is not read again, and nothing is printed.
	record("a", `{"result": {"subtype": "success", "total_cost_usd": 0.9, "session_id": "sa"}}`)
	out.Reset()
	fillCosts(s, runs, &out)
	if out.Len() != 0 {
		t.Errorf("the second start printed\n%s", out.String())
	}
	check(map[string]*float64{"a": &half})
}
