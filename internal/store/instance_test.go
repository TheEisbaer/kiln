package store

import (
	"path/filepath"
	"testing"
)

// TestPaused pauses the instance, changes the reason, ends the pause, and
// reopens the store: each change is one entry, and a repeat writes none.
func TestPaused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s := reopen(t, path)
	if why, err := s.Paused(); err != nil || why != "" {
		t.Fatalf("a new instance is paused: %q, %v", why, err)
	}
	for _, step := range []struct {
		reason  string
		changed bool
	}{
		{"", false},
		{"the Claude credential is unavailable: the command failed", true},
		{"the Claude credential is unavailable: the command failed", false},
		{"the Claude credential is unavailable: it printed nothing", true},
		{"", true},
		{"", false},
	} {
		changed, err := s.SetPaused(step.reason)
		if err != nil || changed != step.changed {
			t.Errorf("SetPaused(%q): changed %v, %v, want %v", step.reason, changed, err, step.changed)
		}
		if why, err := s.Paused(); err != nil || why != step.reason {
			t.Errorf("after SetPaused(%q): paused %q, %v", step.reason, why, err)
		}
	}
	if _, err := s.SetPaused("the Claude credential is unavailable: again"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = reopen(t, path)
	if why, err := s.Paused(); err != nil || why != "the Claude credential is unavailable: again" {
		t.Errorf("after a reopen: paused %q, %v", why, err)
	}
	h, err := s.InstanceHistory()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"the Claude credential is unavailable: the command failed",
		"the Claude credential is unavailable: it printed nothing", "",
		"the Claude credential is unavailable: again"}
	if len(h) != len(want) {
		t.Fatalf("the history is %+v, want %q", h, want)
	}
	for i, e := range h {
		if e.Paused != want[i] || e.At.IsZero() || (i > 0 && e.Number <= h[i-1].Number) {
			t.Errorf("entry %d is %+v, want %q", i, e, want[i])
		}
	}
}
