package run

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCost(t *testing.T) {
	runs := t.TempDir()
	write := func(id, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(runs, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runs, id, "result.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ok", `{"result": {"subtype": "success", "total_cost_usd": 0.25, "session_id": "s1"}}`)
	write("old", `{"result": {"subtype": "success", "total_cost_usd": 0.5}}`)
	write("noresult", `{"result": null}`)
	write("crash", `{"result": {"subtype": "error_during_execution", "total_cost_usd": 0, "session_id": "s2"}}`)
	write("crashpaid", `{"result": {"subtype": "error_during_execution", "total_cost_usd": 0.1, "session_id": "s2"}}`)
	write("zero", `{"result": {"subtype": "success", "total_cost_usd": 0, "session_id": "s3"}}`)
	write("bad", `not json`)

	for _, tc := range []struct {
		id      string
		cost    float64
		known   bool
		session string
		err     bool
	}{
		{"ok", 0.25, true, "s1", false},
		{"old", 0.5, true, "", false},
		{"noresult", 0, false, "", false},
		{"crash", 0, false, "", false},
		{"crashpaid", 0.1, true, "s2", false},
		{"zero", 0, true, "s3", false},
		{"missing", 0, false, "", false},
		{"bad", 0, false, "", true},
	} {
		cost, session, err := ReadCost(runs, tc.id)
		if (err != nil) != tc.err || (cost != nil) != tc.known || (cost != nil && *cost != tc.cost) || session != tc.session {
			t.Errorf("%s: %v, %q, %v; want %v (%v), %q, error %v", tc.id, cost, session, err, tc.cost, tc.known, tc.session, tc.err)
		}
	}
	if _, err := os.Stat(filepath.Join(runs, "missing")); err == nil {
		t.Error("ReadCost made a directory")
	}
}
