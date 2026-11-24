package tick

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/store"
)

// stuckBench is a bench whose core runs on a fake clock, with a look at its
// store while a core runs.
func stuckBench(t *testing.T, runs int) (*publishing, *fakeClock) {
	t.Helper()
	b, clk, _ := withClock(t, runs)
	return &publishing{bench: b, data: filepath.Join(b.cfg.Data, "store.db")}, clk
}

// runID is the ID of a card's nth run.
func (b *bench) runID(card int64, n int) string { return fmt.Sprintf("%s-%d-%d", b.instance, card, n) }

// inFlight puts a card's nth run in flight in the store, as a core before did,
// and its container on the engine in the status.
func (b *bench) inFlight(t *testing.T, s *store.Store, e *fakeEngine, card int64, n int, commit, status string) string {
	t.Helper()
	id := b.runID(card, n)
	if err := s.AddRun(store.RunRow{ID: id, Card: card, Commit: commit, Model: "m", Effort: "high"}, MaxRuns); err != nil {
		t.Fatal(err)
	}
	e.set(id, status)
	return id
}

// waitInspected waits until a run has been inspected n more times. The tick
// inspects a run once per tick, so after two more the tick of the first has
// done all it does with what it saw.
func (e *fakeEngine) waitInspected(t *testing.T, id string, n int) {
	t.Helper()
	e.mu.Lock()
	from := e.inspected[id]
	e.mu.Unlock()
	waitUntil(t, fmt.Sprintf("%d more looks at run %s", n, id), func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.inspected[id] >= from+n
	})
}

// assertInFlight asserts that a run is in flight in the store, and that its
// container is there.
func (p *publishing) assertInFlight(t *testing.T, e *fakeEngine, card int64, id string) {
	t.Helper()
	p.look(t, func(s *store.Store) {
		r, err := s.InFlight(card)
		if err != nil || r == nil || r.ID != id {
			t.Fatalf("card %d has in flight %+v (%v), want %s", card, r, err, id)
		}
	})
	if _, ok := e.snapshot().containers[id]; !ok {
		t.Fatalf("the container of run %s is gone", id)
	}
}

// entryOf is the entry of a run in a card's history, and the entry after it.
func (p *publishing) entryOf(t *testing.T, card int64, id string) (store.Entry, store.Entry) {
	t.Helper()
	_, h := p.history(t, card)
	for i, e := range h {
		if e.Run == id && i+1 < len(h) {
			return e, h[i+1]
		}
	}
	t.Fatalf("card %d has no entry of run %s with one after it: %+v", card, id, h)
	return store.Entry{}, store.Entry{}
}

// stuckReasons are the reasons of the stuck states, with <id> for the run's.
var stuckReasons = map[string]string{
	"created": "its container kiln-run-<id> never started in 5 minutes of tries; the last try: the engine refused to start kiln-run-<id>, as the test has it; " +
		"the engine refused the start, or the run's network or way out is gone: look at the run image (--image) and the engine's limits",
	"paused": "its container kiln-run-<id> was paused for 5 minutes; Kiln never pauses a run, so something else did: " +
		"unpause a run's container within 5 minutes, or leave it alone",
	"restarting": "its container kiln-run-<id> was restarting for 5 minutes; Kiln gives a run no restart policy, so one was set outside Kiln (docker update --restart)",
	"removing":   "its container kiln-run-<id> was being removed for 5 minutes; Kiln removes a run only after it has ended, so something else began it: look at the engine",
	"dead": "its container kiln-run-<id> was dead for 5 minutes, which the engine leaves when a removal failed halfway: " +
		"look at the engine, which may need a restart; the sweep tries the removal again on every tick",
}

// stuckReasonOf is the reason of a stuck state for a run.
func stuckReasonOf(status, id string) string {
	return strings.ReplaceAll(stuckReasons[status], "<id>", id)
}

// TestStuckEnded has a spec card's run 1 in flight in each stuck state: one
// second before the bound it is still in flight with its container, and after
// it the run is failed with the state's reason, its container is removed, and
// the retry starts and finishes. A created run is one whose own start the
// engine refuses, and every start after it.
func TestStuckEnded(t *testing.T) {
	for _, status := range []string{"created", "paused", "restarting", "removing", "dead"} {
		t.Run(status, func(t *testing.T) {
			p, clk := stuckBench(t, 3)
			e := newFakeEngine()
			var card int64
			p.with(t, func(s *store.Store) {
				card = p.bench.card(t, s, store.Spec)
				if status != "created" {
					p.inFlight(t, s, e, card, 1, p.commit, status)
				}
			})
			id := p.runID(card, 1)
			if status == "created" {
				e.startFails[card] = 1 << 20
			}
			e.open(card, "exit")
			core := p.start(t, e, 3)

			e.waitInspected(t, id, 3)
			clk.advance(stuckFor - time.Second)
			e.waitInspected(t, id, 2)
			p.assertInFlight(t, e, card, id)

			clk.advance(2 * time.Second)
			waitUntil(t, "run 1 ends", func() bool { return p.runsOf(t, card)[0] == "1 failed" })
			e.mu.Lock()
			e.startFails[card] = 0
			e.mu.Unlock()
			p.waitFor(t, card, finished, "")
			core.stop(t)

			reason := stuckReasonOf(status, id)
			run1, rule := p.entryOf(t, card, id)
			if run1.Reason != reason || run1.State != failed {
				t.Errorf("run 1's entry is %+v, want the reason %q", run1, reason)
			}
			if rule.Reason != store.RetryPrefix+reason || rule.Cause != store.CauseRule {
				t.Errorf("the rule's entry is %+v", rule)
			}
			if got, want := p.runsOf(t, card), []string{"1 failed", "2 finished"}; !slices.Equal(got, want) {
				t.Errorf("the runs are %q, want %q", got, want)
			}
			if _, ok := e.snapshot().containers[id]; ok {
				t.Errorf("the container of run %s is still there", id)
			}
		})
	}
}

// TestStuckPasses has three runs in a stuck state for a while that pass
// before the bound, and none is ended for it.
func TestStuckPasses(t *testing.T) {
	// A start that the engine refuses twice and then lets through.
	t.Run("created, then started", func(t *testing.T) {
		p, clk := stuckBench(t, 3)
		e := newFakeEngine()
		var card int64
		p.with(t, func(s *store.Store) { card = p.bench.card(t, s, store.Spec) })
		e.startFails[card] = 2
		e.open(card, "exit")
		core := p.start(t, e, 3)
		p.waitFor(t, card, finished, "")
		clk.advance(stuckFor + time.Minute)
		e.waitTicks(t, 5)
		core.stop(t)
		if got, want := p.runsOf(t, card), []string{"1 finished"}; !slices.Equal(got, want) {
			t.Errorf("the runs are %q, want %q", got, want)
		}
		if s := e.snapshot(); s.carried[card] != 3 {
			t.Errorf("the run was carried %d times, want 3", s.carried[card])
		}
	})

	// Paused for 4 minutes, running for a tick, and paused for 4 minutes
	// again: the note starts again at the second pause, which is ended only
	// once it has lasted the bound itself.
	t.Run("paused twice", func(t *testing.T) {
		p, clk := stuckBench(t, 3)
		e := newFakeEngine()
		var card int64
		var id string
		p.with(t, func(s *store.Store) {
			card = p.bench.card(t, s, store.Spec)
			id = p.inFlight(t, s, e, card, 1, p.commit, "paused")
		})
		e.open(card, "exit")
		core := p.start(t, e, 3)
		e.waitInspected(t, id, 3)
		clk.advance(4 * time.Minute)
		e.waitInspected(t, id, 2)

		// Running: a follower carries it, and breaks off, so that the tick
		// looks at it again.
		e.mu.Lock()
		e.breakOff[card] = true
		carried := e.carried[card]
		e.mu.Unlock()
		e.set(id, "running")
		waitUntil(t, "the running run is carried", func() bool { return e.snapshot().carried[card] > carried })
		e.mu.Lock()
		e.containers[id] = "paused"
		e.breakOff[card] = false
		e.mu.Unlock()
		e.waitInspected(t, id, 2)

		clk.advance(4 * time.Minute)
		e.waitInspected(t, id, 2)
		p.assertInFlight(t, e, card, id)
		clk.advance(time.Minute)
		p.waitFor(t, card, finished, "")
		core.stop(t)
		if run1, _ := p.entryOf(t, card, id); run1.Reason != stuckReasonOf("paused", id) {
			t.Errorf("run 1's entry is %+v", run1)
		}
	})

	// Restarting for a tick, then running: it finishes.
	t.Run("restarting once", func(t *testing.T) {
		p, clk := stuckBench(t, 3)
		e := newFakeEngine()
		var card int64
		var id string
		p.with(t, func(s *store.Store) {
			card = p.bench.card(t, s, store.Spec)
			id = p.inFlight(t, s, e, card, 1, p.commit, "restarting")
		})
		e.open(card, "exit")
		core := p.start(t, e, 3)
		e.waitInspected(t, id, 2)
		clk.advance(4 * time.Minute)
		e.waitInspected(t, id, 2)
		e.set(id, "running")
		p.waitFor(t, card, finished, "")
		clk.advance(2 * time.Minute)
		e.waitTicks(t, 5)
		core.stop(t)
		if got, want := p.runsOf(t, card), []string{"1 finished"}; !slices.Equal(got, want) {
			t.Errorf("the runs are %q, want %q", got, want)
		}
	})
}

// TestStuckAcrossCores has a paused run seen for 4 minutes by one core and 4
// by the next: the next core counts from its own first look, and ends it only
// once it has seen it for the bound.
func TestStuckAcrossCores(t *testing.T) {
	p, clk := stuckBench(t, 3)
	e := newFakeEngine()
	var card int64
	var id string
	p.with(t, func(s *store.Store) {
		card = p.bench.card(t, s, store.Spec)
		id = p.inFlight(t, s, e, card, 1, p.commit, "paused")
	})
	e.open(card, "exit")
	core := p.start(t, e, 3)
	e.waitInspected(t, id, 3)
	clk.advance(4 * time.Minute)
	e.waitInspected(t, id, 2)
	core.stop(t)

	core = p.start(t, e, 3)
	e.waitInspected(t, id, 3)
	clk.advance(4 * time.Minute)
	e.waitInspected(t, id, 2)
	p.assertInFlight(t, e, card, id)
	clk.advance(time.Minute + time.Second)
	p.waitFor(t, card, finished, "")
	core.stop(t)
	if run1, _ := p.entryOf(t, card, id); run1.Reason != stuckReasonOf("paused", id) {
		t.Errorf("run 1's entry is %+v", run1)
	}
}

// TestStuckFreesPlace has a limit of 1, a card A whose starts the engine
// always refuses, and a card B behind it: once A's run is ended by the bound,
// B's run starts, and A's retry waits its turn; once B has finished, A's retry
// starts, is ended by the bound as well, and A waits for the developer with
// both reasons.
func TestStuckFreesPlace(t *testing.T) {
	p, clk := stuckBench(t, 1)
	e := newFakeEngine()
	var a, b int64
	p.with(t, func(s *store.Store) {
		a = p.bench.card(t, s, store.Spec)
		b = p.bench.card(t, s, store.Spec)
	})
	e.startFails[a] = 1 << 20
	e.open(a, "exit")
	core := p.start(t, e, 1)

	a1, a2 := p.runID(a, 1), p.runID(a, 2)
	e.waitInspected(t, a1, 3)
	if s := e.snapshot(); len(s.started) != 0 {
		t.Fatalf("the runs of cards %v started while A's run holds the place", s.started)
	}
	clk.advance(stuckFor + time.Second)
	e.waitStarted(t, group(b))
	e.waitTicks(t, 5)
	if got, want := p.runsOf(t, a), []string{"1 failed"}; !slices.Equal(got, want) {
		t.Errorf("while B runs, A's runs are %q, want %q", got, want)
	}

	e.open(b, "exit")
	waitUntil(t, "A's retry is in flight", func() bool { return len(p.runsOf(t, a)) == 2 })
	e.waitInspected(t, a2, 3)
	clk.advance(stuckFor + time.Second)
	p.waitFor(t, a, failed, retryFailedAs)
	core.stop(t)

	first, second := stuckReasonOf("created", a1), stuckReasonOf("created", a2)
	_, h := p.history(t, a)
	if got, want := h[len(h)-1].Reason, retryFailedAs+"the first run: "+first+"; the retry: "+second; got != want {
		t.Errorf("the rule's entry says %q, want %q", got, want)
	}
	if got, want := p.runsOf(t, a), []string{"1 failed", "2 failed"}; !slices.Equal(got, want) {
		t.Errorf("A's runs are %q, want %q", got, want)
	}
	if got, want := p.runsOf(t, b), []string{"1 finished"}; !slices.Equal(got, want) {
		t.Errorf("B's runs are %q, want %q", got, want)
	}
	if s := e.snapshot(); len(s.containers) != 0 {
		t.Errorf("the containers %v are left", s.containers)
	}
}

// TestStuckInstruction has runs in flight whose instruction was removed by
// hand: one written again with another text, which the run's container
// refuses, and one that cannot be written again, since its change is now
// held. Each is failed at once, with the file in its reason, and its
// container is removed.
func TestStuckInstruction(t *testing.T) {
	t.Run("written again with another text", func(t *testing.T) {
		p, _ := stuckBench(t, 3)
		e := newFakeEngine()
		var card int64
		var id string
		p.with(t, func(s *store.Store) {
			card = p.bench.card(t, s, store.Spec)
			id = p.inFlight(t, s, e, card, 1, p.commit, "running")
		})
		e.otherInstruction[card] = true
		e.open(card, "exit")
		core := p.start(t, e, 3)
		p.waitFor(t, card, finished, "")
		core.stop(t)

		dir := filepath.Join(p.cfg.Data, "instructions")
		reason := "its instruction " + filepath.Join(dir, id+".md") + " was removed or changed while the run was in flight, " +
			"and the run was started with another; leave the files under " + dir + " alone while their runs are in flight"
		run1, rule := p.entryOf(t, card, id)
		if run1.Reason != reason || rule.Reason != store.RetryPrefix+reason {
			t.Errorf("run 1's entry is %+v and the rule's %+v, want the reason %q", run1, rule, reason)
		}
		if got, want := p.runsOf(t, card), []string{"1 failed", "2 finished"}; !slices.Equal(got, want) {
			t.Errorf("the runs are %q, want %q", got, want)
		}
		if _, ok := e.snapshot().containers[id]; ok {
			t.Errorf("the container of run %s is still there", id)
		}
	})

	t.Run("cannot be written again", func(t *testing.T) {
		p, _ := stuckBench(t, 3)
		e := newFakeEngine()
		var card int64
		var id string
		p.with(t, func(s *store.Store) {
			card = p.bench.card(t, s, store.Review)
			if card != 1 {
				t.Fatalf("the card is %d; commitOnBranch writes the branch of card 1", card)
			}
			tip := commitOnBranch(t, p.cfg.Repo, p.commit, map[string]string{"big.txt": strings.Repeat("a line of the change\n", 55000)})
			id = p.inFlight(t, s, e, card, 3, tip, "running")
		})
		e.open(card, "exit")
		core := p.start(t, e, 3)
		p.waitFor(t, card, reviewNeedsYou, "the change is ")
		core.stop(t)

		run3, rule := p.entryOf(t, card, id)
		want := "its instruction " + filepath.Join(p.cfg.Data, "instructions", id+".md") +
			" was removed while the run was in flight and cannot be written again: the change is "
		if !strings.HasPrefix(run3.Reason, want) || !strings.HasSuffix(run3.Reason, "more than 1 MiB; no review run is started") ||
			rule.Reason != store.RetryPrefix+run3.Reason {
			t.Errorf("run 3's entry is %+v and the rule's %+v, want a reason that begins %q", run3, rule, want)
		}
		if got, want := p.runsOf(t, card), []string{"1 finished", "2 finished", "3 failed"}; !slices.Equal(got, want) {
			t.Errorf("the runs are %q, want %q", got, want)
		}
		if s := e.snapshot(); len(s.containers) != 0 || s.carried[card] != 0 {
			t.Errorf("the containers %v are left, and Carry was called %d times", s.containers, s.carried[card])
		}
	})
}

// TestStuckDocuments has spec runs that finish with a record whose
// documents.tar cannot be read: each is failed at once, with the file in its
// reason and its cost kept, and after the retry the card waits for the
// developer.
func TestStuckDocuments(t *testing.T) {
	p, _ := stuckBench(t, 3)
	e := newFakeEngine()
	var card int64
	p.with(t, func(s *store.Store) { card = p.bench.card(t, s, store.Spec) })
	e.docsFail[card] = fmt.Errorf("as the test has it: %w", fs.ErrNotExist)
	e.open(card, "exit")
	core := p.start(t, e, 3)
	p.waitFor(t, card, failed, retryFailedAs)
	core.stop(t)

	runs := filepath.Join(p.cfg.Data, "runs")
	reasonOf := func(id string) string {
		return "its record " + filepath.Join(runs, id, "documents.tar") + " cannot be read: as the test has it: file does not exist; " +
			"leave the files under " + runs + " alone"
	}
	id1, id2 := p.runID(card, 1), p.runID(card, 2)
	run1, rule := p.entryOf(t, card, id1)
	if run1.Reason != reasonOf(id1) || rule.Reason != store.RetryPrefix+reasonOf(id1) {
		t.Errorf("run 1's entry is %+v and the rule's %+v, want the reason %q", run1, rule, reasonOf(id1))
	}
	if _, last := p.entryOf(t, card, id2); last.Reason != retryFailedAs+"the first run: "+reasonOf(id1)+"; the retry: "+reasonOf(id2) {
		t.Errorf("the rule's last entry is %+v", last)
	}
	p.look(t, func(s *store.Store) {
		rs, err := s.Runs(card)
		if err != nil || len(rs) != 2 {
			t.Fatalf("the runs are %+v (%v)", rs, err)
		}
		for _, r := range rs {
			if r.Ending != store.Failed || r.Cost.USD == nil || *r.Cost.USD != 0.0001 || r.Cost.Session != "fake" {
				t.Errorf("run %s ended %s with the cost %+v", r.ID, r.Ending, r.Cost)
			}
		}
	})
}

// TestSweepGoesOn has two runs of the instance on the engine that are not in
// flight in the store, and the engine cannot remove the first: the sweep says
// so with its ID, removes the second, and the tick goes on to start a waiting
// card's run.
func TestSweepGoesOn(t *testing.T) {
	p, _ := stuckBench(t, 3)
	e := newFakeEngine()
	var card int64
	p.with(t, func(s *store.Store) { card = p.bench.card(t, s, store.Spec) })
	first, second := p.instance+"-98-1", p.instance+"-99-1"
	e.set(first, "exited")
	e.set(second, "exited")
	e.removeFails[first] = true
	e.open(card, "exit")
	core := p.start(t, e, 3)
	p.waitFor(t, card, finished, "")
	core.stop(t)

	s := e.snapshot()
	if !slices.Contains(s.removed, second) || slices.Contains(s.removed, first) {
		t.Errorf("the sweep removed %v", s.removed)
	}
	if _, ok := s.containers[first]; !ok {
		t.Errorf("the run %s that cannot be removed is gone", first)
	}
	if out, want := core.out.String(), "kiln core: the sweep: run "+first+": the engine cannot remove kiln-run-"+first+", as the test has it\n"; !strings.Contains(out, want) {
		t.Errorf("the core printed\n%s\nwant it to say %q", out, want)
	}
}

// TestStuckState is the table of the Engine API's states of a container that
// a run in the store cannot leave by itself.
func TestStuckState(t *testing.T) {
	for status, want := range map[string]bool{
		"created": true, "running": false, "paused": true, "restarting": true, "removing": true, "exited": false, "dead": true, "": false,
	} {
		if got := stuckState(status); got != want {
			t.Errorf("stuckState(%q) is %v, want %v", status, got, want)
		}
	}
}
