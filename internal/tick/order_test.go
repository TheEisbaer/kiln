package tick

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

// queued is where a card waits for its predecessors.
var queued = store.State{Stage: store.Backlog, WaitsFor: store.Room}

// TestPredecessorStarts has a card wait for a predecessor in spec: no core
// starts its run while the predecessor is open, and once it is done, a core
// started again releases the card by the rule and starts its spec run, a first
// run told no note.
func TestPredecessorStarts(t *testing.T) {
	p := retrying(t)
	var pre, card int64
	var preHistory int
	p.with(t, func(s *store.Store) {
		c, err := s.Create("The predecessor.")
		if err != nil {
			t.Fatal(err)
		}
		pre = c.ID
		if _, err := s.Move(pre, finished, store.CauseRun); err != nil {
			t.Fatal(err)
		}
		if c, err = s.Create("The card after it.", pre); err != nil {
			t.Fatal(err)
		}
		card = c.ID
		if c, err = s.Start(card); err != nil || c.State != queued {
			t.Fatalf("the start: %+v, %v", c, err)
		}
		h, _ := s.History(pre)
		preHistory = len(h)
	})
	e := newFakeEngine()
	e.open(card, "exit")
	core := p.start(t, e, 3)
	e.waitTicks(t, 20)
	core.stop(t)
	p.with(t, func(s *store.Store) {
		if c, _ := s.Card(card); c.State != queued {
			t.Errorf("the card is in %v while its predecessor is open", c.State)
		}
		if n, _ := s.RunCount(card); n != 0 {
			t.Errorf("the card has %d runs while its predecessor is open", n)
		}
		if h, _ := s.History(pre); len(h) != preHistory {
			t.Errorf("the predecessor's history has %d entries, want %d", len(h), preHistory)
		}
		if _, err := s.MoveWithReason(pre, finished, store.State{Stage: store.Done, WaitsFor: store.Nothing}, store.CauseHost, store.MergedReason); err != nil {
			t.Fatal(err)
		}
	})

	core = p.start(t, e, 3)
	p.waitFor(t, card, finished, "")
	core.stop(t)
	lines, h := p.history(t, card)
	if want := []string{"backlog/room by developer", "spec/run by rule", "spec/approval by run"}; !slices.Equal(lines[len(lines)-3:], want) {
		t.Fatalf("the history ends with %q, want %q", lines, want)
	}
	if got, want := h[len(h)-3].Reason, fmt.Sprintf("%s#%d", store.QueuedPrefix, pre); got != want {
		t.Errorf("the start's reason is %q, want %q", got, want)
	}
	if got, want := h[len(h)-2].Reason, fmt.Sprintf("%s#%d", store.ReleasedPrefix, pre); got != want {
		t.Errorf("the rule's reason is %q, want %q", got, want)
	}
	if got, want := p.runsOf(t, card), []string{"1 finished"}; !slices.Equal(got, want) {
		t.Errorf("the runs are %q, want %q", got, want)
	}
	told := string(readFile(t, filepath.Join(p.cfg.Data, "instructions", fmt.Sprintf("%s-%d-1.md", p.instance, card))))
	if strings.Contains(told, noteHeading) {
		t.Errorf("the run is told a note:\n%s", told)
	}
	if out := core.out.String(); !strings.Contains(out, fmt.Sprintf("kiln core: card %d: %s#%d; its spec run starts by the rule", card, store.ReleasedPrefix, pre)) {
		t.Errorf("the core printed\n%s", out)
	}
}

// TestPredecessorDropped has a card's predecessor dropped: the core leaves the
// card waiting for the developer, with the reason, and once the developer
// starts it, a core starts its spec run.
func TestPredecessorDropped(t *testing.T) {
	p := retrying(t)
	var pre, card int64
	p.with(t, func(s *store.Store) {
		c, err := s.Create("The predecessor.")
		if err != nil {
			t.Fatal(err)
		}
		pre = c.ID
		if c, err = s.Create("The card after it.", pre); err != nil {
			t.Fatal(err)
		}
		card = c.ID
		if _, err := s.Start(card); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Decide(pre, store.Drop, ""); err != nil {
			t.Fatal(err)
		}
	})
	e := newFakeEngine()
	e.open(card, "exit")
	core := p.start(t, e, 3)
	stopped := store.State{Stage: store.Backlog, WaitsFor: store.Developer}
	last := p.waitFor(t, card, stopped, store.PredecessorDroppedPrefix)
	core.stop(t)
	if want := fmt.Sprintf("its predecessor #%d was dropped; start it without it, or drop it", pre); last.Cause != store.CauseRule || last.Reason != want {
		t.Errorf("the rule's entry is %+v, want the reason %q", last, want)
	}
	p.with(t, func(s *store.Store) {
		if _, err := s.Decide(card, store.Start, ""); err != nil {
			t.Fatal(err)
		}
	})
	core = p.start(t, e, 3)
	p.waitFor(t, card, finished, "")
	core.stop(t)
	lines, h := p.history(t, card)
	if want := []string{"backlog/developer by rule", "spec/run by developer", "spec/approval by run"}; !slices.Equal(lines[len(lines)-3:], want) {
		t.Fatalf("the history ends with %q, want %q", lines, want)
	}
	if r := h[len(h)-2].Reason; r != "" {
		t.Errorf("the start has the reason %q", r)
	}
}
