package tick

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/store"
)

// The reasons of the fake's runs, by their outcomes.
const (
	failedReason  = "exit 1, no result, not saved"
	oomReason     = "exit 137, killed for memory, no result, not saved"
	retryFailedAs = "the retry failed as well, and the rule runs a failed run once more at most, so it waits for you; "
)

// retrying is a bench for the tests of the retry, which publishes nothing.
func retrying(t *testing.T) *publishing {
	t.Helper()
	p := newPublishing(t)
	p.cfg.PublishCommand = ""
	return p
}

// history is a card's entries, and each as entryLine has it.
func (p *publishing) history(t *testing.T, card int64) (lines []string, entries []store.Entry) {
	t.Helper()
	p.look(t, func(s *store.Store) {
		h, err := s.History(card)
		if err != nil {
			t.Fatal(err)
		}
		entries = h
	})
	for _, e := range entries {
		lines = append(lines, entryLine(e))
	}
	return lines, entries
}

// runsOf is a card's runs with their endings, as "<n> <ending>".
func (p *publishing) runsOf(t *testing.T, card int64) []string {
	t.Helper()
	var got []string
	p.look(t, func(s *store.Store) {
		runs, err := s.Runs(card)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			got = append(got, fmt.Sprintf("%s %s", strings.TrimPrefix(r.ID, fmt.Sprintf("%s-%d-", p.instance, card)), r.Ending))
		}
	})
	return got
}

// TestRetried has a spec card's first run fail and its retry finish: the
// rule's entry says the run is run once more, with the first run's reason,
// and the card goes on as after any finished run. The retry is told the
// developer's note the first run was told.
func TestRetried(t *testing.T) {
	p := retrying(t)
	const note = "Name the table."
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.bench.card(t, s, store.Spec)
		// The spec sent back once with a note: the runs are told it.
		if _, err := s.Move(card, finished, store.CauseRun); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Decide(card, store.Again, note); err != nil {
			t.Fatal(err)
		}
	})
	e := newFakeEngine()
	e.outcomes[card] = []string{"fail", "finish"}
	e.open(card, "exit")
	core := p.start(t, e, 3)
	p.waitFor(t, card, finished, "")
	core.stop(t)

	lines, h := p.history(t, card)
	if want := []string{"spec/developer by run", "spec/run by rule", "spec/approval by run"}; !slices.Equal(lines[len(lines)-3:], want) {
		t.Fatalf("the history ends with %q, want %q", lines, want)
	}
	run1, rule, run2 := h[len(h)-3], h[len(h)-2], h[len(h)-1]
	id := func(n int) string { return fmt.Sprintf("%s-%d-%d", p.instance, card, n) }
	if run1.Run != id(1) || run1.Reason != failedReason {
		t.Errorf("the first run's entry is %+v", run1)
	}
	if rule.Run != "" || rule.Reason != store.RetryPrefix+failedReason {
		t.Errorf("the rule's entry is %+v", rule)
	}
	if run2.Run != id(2) {
		t.Errorf("the retry's entry is %+v", run2)
	}
	if got, want := p.runsOf(t, card), []string{"1 failed", "2 finished"}; !slices.Equal(got, want) {
		t.Errorf("the runs are %q, want %q", got, want)
	}
	if out := core.out.String(); !strings.Contains(out, fmt.Sprintf("card %d: %s%s", card, store.RetryPrefix, failedReason)) {
		t.Errorf("the core printed\n%s", out)
	}
	for _, n := range []int{1, 2} {
		told := string(readFile(t, filepath.Join(p.cfg.Data, "instructions", id(n)+".md")))
		if !strings.HasSuffix(strings.TrimRight(told, "\n"), noteIntro+"\n\n"+note) {
			t.Errorf("run %d is told\n%s\nwant it to end with the note", n, told)
		}
	}
}

// TestRetryFailed has a spec card's run and its retry fail: the card stops
// under "needs you" with both reasons, and no third run starts.
func TestRetryFailed(t *testing.T) {
	p := retrying(t)
	var card int64
	p.with(t, func(s *store.Store) { card = p.bench.card(t, s, store.Spec) })
	e := newFakeEngine()
	e.outcomes[card] = []string{"fail", "oom"}
	e.open(card, "exit")
	core := p.start(t, e, 3)
	p.waitFor(t, card, failed, retryFailedAs)
	e.waitTicks(t, 5)
	core.stop(t)

	lines, h := p.history(t, card)
	if want := []string{"spec/developer by run", "spec/run by rule", "spec/developer by run", "spec/developer by rule"}; !slices.Equal(lines[len(lines)-4:], want) {
		t.Fatalf("the history ends with %q, want %q", lines, want)
	}
	if e := h[len(h)-2]; e.Run != fmt.Sprintf("%s-%d-2", p.instance, card) || e.Reason != oomReason {
		t.Errorf("the retry's entry is %+v", e)
	}
	if got, want := h[len(h)-1].Reason, retryFailedAs+"the first run: "+failedReason+"; the retry: "+oomReason; got != want {
		t.Errorf("the rule's entry says %q, want %q", got, want)
	}
	if got, want := p.runsOf(t, card), []string{"1 failed", "2 failed"}; !slices.Equal(got, want) {
		t.Errorf("the runs are %q, want %q", got, want)
	}
	if s := e.snapshot(); s.carried[card] != 2 {
		t.Errorf("the card's runs were carried %d times, want 2", s.carried[card])
	}
}

// TestRetriedInEachStage has a card's first implement run fail and its retry
// save, and its first review run fail and its retry pass: each stage has its
// own retry, and the card waits for approval.
func TestRetriedInEachStage(t *testing.T) {
	p := retrying(t)
	var card int64
	p.with(t, func(s *store.Store) { card = p.approvedCard(t, s) })
	e := newFakeEngine()
	e.outcomes[card] = []string{"fail", "finish", "fail", "finish"}
	e.saves[card] = true
	e.open(card, "exit")
	core := p.start(t, e, 3)
	p.waitFor(t, card, reviewPassed, "")
	core.stop(t)

	lines, h := p.history(t, card)
	at := slices.Index(lines, "implement/run by developer")
	want := []string{"implement/run by developer",
		"implement/developer by run", "implement/run by rule", "review/run by run",
		"review/developer by run", "review/run by rule", "review/approval by run"}
	if at < 0 || !slices.Equal(lines[at:], want) {
		t.Fatalf("the history is\n%q\nwant it to end with\n%q", lines, want)
	}
	for _, i := range []int{at + 2, at + 5} {
		if h[i].Reason != store.RetryPrefix+failedReason {
			t.Errorf("the rule's entry %d says %q", i, h[i].Reason)
		}
	}
	if got, want := p.runsOf(t, card), []string{"1 finished", "2 failed", "3 finished", "4 failed", "5 finished"}; !slices.Equal(got, want) {
		t.Errorf("the runs are %q, want %q", got, want)
	}
}

// TestRefusedRetry has a spec run that the upstream refused the credential to
// while the instance is paused: it uses up no retry, no run starts while the
// pause lasts, and the run starts once the pause ends.
func TestRefusedRetry(t *testing.T) {
	b, clk, cmd := withClock(t, 1)
	p := &publishing{bench: b, data: filepath.Join(b.cfg.Data, "store.db")}
	var card int64
	p.with(t, func(s *store.Store) { card = p.bench.card(t, s, store.Spec) })
	e := newFakeEngine()
	e.outcomes[card] = []string{"refused", "finish"}
	r := b.start(t, e, 1)
	e.waitStarted(t, group(card))
	clk.waiting(t, 1)

	cmd.set(t, "echo 'the sign-in has expired' >&2; exit 1")
	clk.advance(renewEvery)
	clk.waiting(t, 2)
	if b.paused(t) == "" {
		t.Fatal("the instance is not paused")
	}
	e.open(card, "exit")
	p.waitFor(t, card, waiting, store.RefusedReason)
	e.waitTicks(t, 5)
	if got := e.snapshot().started; !slices.Equal(got, []int64{card}) {
		t.Fatalf("while paused the runs of %v started", got)
	}

	cmd.set(t, "echo cred-2")
	clk.advance(retryEvery)
	e.waitStarted(t, group(card), group(card))
	p.waitFor(t, card, finished, "")
	r.stop(t)
	if got, want := p.runsOf(t, card), []string{"1 failed", "2 finished"}; !slices.Equal(got, want) {
		t.Errorf("the runs are %q, want %q", got, want)
	}
}

// TestRefusedOnTheRetry has a spec run fail, its retry be refused the
// credential, and the run after that fail: the refusal kept it the retry, so
// the card stops under "needs you" with the first run's reason, after three
// runs.
func TestRefusedOnTheRetry(t *testing.T) {
	p := retrying(t)
	var card int64
	p.with(t, func(s *store.Store) { card = p.bench.card(t, s, store.Spec) })
	e := newFakeEngine()
	e.outcomes[card] = []string{"fail", "refused", "oom"}
	e.open(card, "exit")
	core := p.start(t, e, 3)
	p.waitFor(t, card, failed, retryFailedAs)
	e.waitTicks(t, 5)
	core.stop(t)

	lines, h := p.history(t, card)
	want := []string{"spec/developer by run", "spec/run by rule", "spec/developer by run", "spec/run by rule",
		"spec/developer by run", "spec/developer by rule"}
	if !slices.Equal(lines[len(lines)-6:], want) {
		t.Fatalf("the history ends with %q, want %q", lines, want)
	}
	if got, want := h[len(h)-3].Reason, store.RefusedRetryPrefix+failedReason; got != want {
		t.Errorf("the rule's entry after the refusal says %q, want %q", got, want)
	}
	if got, want := h[len(h)-1].Reason, retryFailedAs+"the first run: "+failedReason+"; the retry: "+oomReason; got != want {
		t.Errorf("the rule's last entry says %q, want %q", got, want)
	}
	if got, want := p.runsOf(t, card), []string{"1 failed", "2 failed", "3 failed"}; !slices.Equal(got, want) {
		t.Errorf("the runs are %q, want %q", got, want)
	}
}

// TestRetryAcrossCores stops a core, as Ctrl-C does, once the rule's entry
// after a failed run is written and before the retry is in the store: the
// next core starts the retry, and its failure stops the card with both
// reasons, after two runs, as on one core.
func TestRetryAcrossCores(t *testing.T) {
	p := retrying(t)
	var card int64
	p.with(t, func(s *store.Store) { card = p.bench.card(t, s, store.Spec) })
	e := newFakeEngine()
	core := p.start(t, e, 3)
	e.waitStarted(t, group(card))
	core.stop(t) // the first run goes on

	// The next core carries the first run to its failure, and holds the
	// retry before it is in the store.
	e.outcomes[card] = []string{"fail"}
	e.hold(card, "add")
	e.open(card, "exit")
	core = p.start(t, e, 3)
	p.waitFor(t, card, waiting, store.RetryPrefix+failedReason)
	waitUntil(t, "the retry is held before it is in the store", func() bool {
		s := e.snapshot()
		return s.carried[card] == 3 && s.carrying[card] == 1
	})
	core.stop(t)
	if got, want := p.runsOf(t, card), []string{"1 failed"}; !slices.Equal(got, want) {
		t.Fatalf("after the stop the runs are %q, want %q", got, want)
	}

	e = newFakeEngine()
	e.outcomes[card] = []string{"fail"}
	e.open(card, "exit")
	core = p.start(t, e, 3)
	p.waitFor(t, card, failed, retryFailedAs+"the first run: "+failedReason+"; the retry: "+failedReason)
	e.waitTicks(t, 5)
	core.stop(t)
	if got, want := p.runsOf(t, card), []string{"1 failed", "2 failed"}; !slices.Equal(got, want) {
		t.Errorf("the runs are %q, want %q", got, want)
	}
}

// TestRetryKilled kills a core at the end of a failed run, before its end is
// written: the next core writes the rule's entry once, and the retry follows.
func TestRetryKilled(t *testing.T) {
	p := retrying(t)
	var card int64
	p.with(t, func(s *store.Store) { card = p.bench.card(t, s, store.Spec) })
	cmd, done := startChild(t, child{Command: "core", Config: p.cfg, ExitAt: "spec/before-end",
		Outcomes: map[int64][]string{card: {"fail"}}})
	if code := <-done; code != exitAtPoint {
		t.Fatalf("the core exited with %d, want %d\n%s%s", code, exitAtPoint, cmd.Stdout, cmd.Stderr)
	}
	if got, want := p.runsOf(t, card), []string{"1 "}; !slices.Equal(got, want) {
		t.Fatalf("after the kill the runs are %q, want %q", got, want)
	}

	e := newFakeEngine()
	e.outcomes[card] = []string{"fail", "finish"}
	e.open(card, "exit")
	core := p.start(t, e, 3)
	p.waitFor(t, card, finished, "")
	core.stop(t)
	_, h := p.history(t, card)
	n := 0
	for _, e := range h {
		if e.Cause == store.CauseRule && strings.HasPrefix(e.Reason, store.RetryPrefix) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d entries of the rule's retry, want 1", n)
	}
	if got, want := p.runsOf(t, card), []string{"1 failed", "2 finished"}; !slices.Equal(got, want) {
		t.Errorf("the runs are %q, want %q", got, want)
	}
}

// TestFailedRule is the rule's step after a failed run, in each stage, by
// whether the upstream refused the credential and whether the run was the
// retry.
func TestFailedRule(t *testing.T) {
	for _, to := range []store.State{failed, implementNeedsYou, reviewNeedsYou} {
		again := store.State{Stage: to.Stage, WaitsFor: store.Run}
		for _, tc := range []struct {
			refused, retrying bool
			to                store.State
			reason            string
		}{
			{true, false, again, store.RefusedReason},
			{true, true, again, store.RefusedRetryPrefix + failedReason},
			{false, false, again, store.RetryPrefix + oomReason},
			{false, true, to, retryFailedAs + "the first run: " + failedReason + "; the retry: " + oomReason},
		} {
			v := run.Verdict{Refused: tc.refused, Reason: oomReason}
			first := ""
			if tc.retrying {
				first = failedReason
			}
			r := failedRule(to, v, first, tc.retrying)
			if r == nil || r.To != tc.to || r.Reason != tc.reason || r.SendBack || r.Publish {
				t.Errorf("in %s, refused %v, retrying %v: %+v, want %v with %q", to.Stage, tc.refused, tc.retrying, r, tc.to, tc.reason)
			}
		}
	}
}
