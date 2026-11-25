package run

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReadReached(t *testing.T) {
	runs := t.TempDir()
	dir := filepath.Join(runs, "a-run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"time":"t","method":"CONNECT","target":"go.dev:443","decision":"allowed","status":200}`,
		`{"time":"t","target":"go.dev:443","sent":1500,"received":70000,"duration_ms":30,"end":"host closed"}`,
		`{"time":"t","method":"CONNECT","target":"Go.Dev.:443","decision":"allowed","status":200}`,
		`{"time":"t","target":"Go.Dev.:443","sent":500,"received":1000,"duration_ms":3,"end":"run closed"}`,
		// Open when the way out was killed: no end.
		`{"time":"t","method":"CONNECT","target":"go.dev:443","decision":"allowed","status":200}`,
		`{"time":"t","method":"CONNECT","target":"pkg.go.dev:443","decision":"allowed","status":200}`,
		`{"time":"t","target":"pkg.go.dev:443","sent":90000,"received":10,"duration_ms":3,"end":"error","error":"reset"}`,
		// Refused, and let through with no address connected: no connection.
		`{"time":"t","method":"CONNECT","target":"127.0.0.1:443","decision":"refused","reason":"not public","status":403}`,
		`{"time":"t","method":"CONNECT","target":"go.dev:443","decision":"allowed","reason":"no address connected","status":502}`,
		`{"time":"t","method":"CONNECT","target":"go.dev:443","decision":"allowed","status":200}`,
		`{"time":"t","target":"go.dev:443","sent":10,"rec`,
	}
	if err := os.WriteFile(filepath.Join(dir, "wayout.jsonl"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadReached(runs, "a-run")
	if err != nil {
		t.Fatal(err)
	}
	want := []Reached{
		{Host: "pkg.go.dev", Connections: 1, Ended: 1, Sent: 90000, Received: 10},
		{Host: "go.dev", Connections: 4, Ended: 2, Sent: 2000, Received: 71000},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	// Equal bytes sent: by name.
	if err := os.WriteFile(filepath.Join(dir, "wayout.jsonl"), []byte(
		`{"target":"b.example:443","decision":"allowed","status":200}`+"\n"+
			`{"target":"a.example:443","decision":"allowed","status":200}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadReached(runs, "a-run"); err != nil || len(got) != 2 || got[0].Host != "a.example" {
		t.Errorf("got %+v, %v; want a.example first", got, err)
	}

	// A long line, longer than a scanner's default limit, is read.
	long := `{"target":"` + strings.Repeat("x", 200_000) + `:443","decision":"refused","status":403}` + "\n" +
		`{"target":"go.dev:443","decision":"allowed","status":200}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "wayout.jsonl"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadReached(runs, "a-run"); err != nil || len(got) != 1 || got[0].Connections != 1 {
		t.Errorf("after a long line: %+v, %v", got, err)
	}

	for _, id := range []string{"no-file", "no-run"} {
		if id == "no-file" {
			os.MkdirAll(filepath.Join(runs, id), 0o755)
		}
		if got, err := ReadReached(runs, id); got != nil || err != nil {
			t.Errorf("%s: %+v, %v; want nil", id, got, err)
		}
	}
}
