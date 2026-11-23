package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestMerging turns automatic merging off, again with the same reason and
// with another, pauses, turns it on twice, and reopens the store: each change
// is one entry with the instance's whole state and its cause, and a repeat
// or a refused move writes none.
func TestMerging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s := reopen(t, path)
	if m, err := s.Merging(); err != nil || m != (Merging{}) {
		t.Fatalf("a new instance has merging %+v, %v, want on", m, err)
	}
	const first, second = "the update to b failed: it exited", "the update to c failed: it hung"
	for _, step := range []struct {
		reason, failed string
		changed        bool
	}{
		{first, "b", true},
		{first, "b", false},
		{second, "c", true},
	} {
		changed, err := s.TurnMergingOff(step.reason, step.failed)
		if err != nil || changed != step.changed {
			t.Errorf("TurnMergingOff(%q, %q): changed %v, %v, want %v", step.reason, step.failed, changed, err, step.changed)
		}
		if m, err := s.Merging(); err != nil || m != (Merging{Off: step.reason, Failed: step.failed}) {
			t.Errorf("after TurnMergingOff(%q, %q): merging %+v, %v", step.reason, step.failed, m, err)
		}
	}
	if _, err := s.SetPaused("the credential is unavailable"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = reopen(t, path)
	if m, err := s.Merging(); err != nil || m != (Merging{Off: second, Failed: "c"}) {
		t.Errorf("after a reopen: merging %+v, %v", m, err)
	}
	if err := s.TurnMergingOn(); err != nil {
		t.Fatal(err)
	}
	if m, err := s.Merging(); err != nil || m != (Merging{}) {
		t.Errorf("after TurnMergingOn: merging %+v, %v, want on", m, err)
	}
	if err := s.TurnMergingOn(); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "automatic merging is on") {
		t.Errorf("a second TurnMergingOn: %v, want refused", err)
	}
	s.Close()
	s = reopen(t, path)
	h, err := s.InstanceHistory()
	if err != nil {
		t.Fatal(err)
	}
	const paused = "the credential is unavailable"
	want := []InstanceEntry{
		{Cause: CauseRule, MergingOff: first, Failed: "b"},
		{Cause: CauseRule, MergingOff: second, Failed: "c"},
		{Cause: CauseRule, Paused: paused, MergingOff: second, Failed: "c"},
		{Cause: CauseDeveloper, Paused: paused},
	}
	if len(h) != len(want) {
		t.Fatalf("the history is %+v, want %+v", h, want)
	}
	for i, e := range h {
		if e.At.IsZero() || (i > 0 && e.Number <= h[i-1].Number) {
			t.Errorf("entry %d is %+v", i, e)
		}
		e.Number, e.At = 0, time.Time{}
		if e != want[i] {
			t.Errorf("entry %d is %+v, want %+v", i, e, want[i])
		}
	}
	if m, _ := s.Merging(); m != (Merging{}) {
		t.Errorf("after a reopen: merging %+v, want on", m)
	}
}
