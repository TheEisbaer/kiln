package tick

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/seal"
	"github.com/TheEisbaer/kiln/internal/store"
)

// fakeClock is the clock of the renewal in the tests: it stands still until
// the test moves it, and then fires the waits that are due.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []fakeWait
}

type fakeWait struct {
	at    time.Time
	d     time.Duration
	ch    chan time.Time
	fired bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	f.waits = append(f.waits, fakeWait{at: f.now.Add(d), d: d, ch: ch})
	return ch
}

// advance moves the clock on and fires the waits that are due.
func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	for i := range f.waits {
		if w := &f.waits[i]; !w.fired && !w.at.After(f.now) {
			w.fired = true
			w.ch <- f.now
		}
	}
}

// waiting waits until the renewal has begun its n-th wait, which it does
// once the renewal before has ended, and returns how long that wait is.
func (f *fakeClock) waiting(t *testing.T, n int) time.Duration {
	t.Helper()
	waitUntil(t, fmt.Sprintf("the renewal's wait %d", n), func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.waits) >= n
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waits[n-1].d
}

// count is how many waits the renewal has begun.
func (f *fakeClock) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waits)
}

// standIn is the stand-in for the developer's credential command: a script the
// test writes, which notes each run and then does what the test last set.
type standIn struct {
	dir     string
	command string
}

func newStandIn(t *testing.T) *standIn {
	t.Helper()
	s := &standIn{dir: t.TempDir()}
	script := filepath.Join(s.dir, "command.sh")
	if err := os.WriteFile(script, []byte("d=$(dirname \"$0\")\necho run >>\"$d/runs\"\n. \"$d/now.sh\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.command = "sh " + shellWord(script)
	return s
}

// set makes the stand-in do what a shell does with script from now on.
func (s *standIn) set(t *testing.T, script string) {
	t.Helper()
	if err := writeAtomically(filepath.Join(s.dir, "now.sh"), []byte(script+"\n")); err != nil {
		t.Fatal(err)
	}
}

// runs is how often the stand-in has run.
func (s *standIn) runs(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.dir, "runs"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "run\n")
}

// pushed is what the fake engine was pushed so far.
func (e *fakeEngine) pushed() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.pushes)
}

// paused is why the store says the instance is paused, read beside a core.
func (b *bench) paused(t *testing.T) string {
	t.Helper()
	s, err := store.Open(filepath.Join(b.cfg.Data, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	why, err := s.Paused()
	if err != nil {
		t.Fatal(err)
	}
	return why
}

// withClock is a bench whose renewal runs on a fake clock and whose
// credential command is a stand-in, which prints cred-1 for a start.
func withClock(t *testing.T, runs int) (*bench, *fakeClock, *standIn) {
	t.Helper()
	b := newBench(t, runs)
	clk := newFakeClock()
	b.clock = clk
	cmd := newStandIn(t)
	cmd.set(t, "echo cred-1")
	b.cfg.CredentialCommand = cmd.command
	return b, clk, cmd
}

// TestRenewal follows the schedule of the renewal on a clock the test moves:
// a push at the start, before the first tick; one every five minutes after a
// success, also of the same credential; and every 30 seconds while the
// credential is unavailable, for each reason, until a success ends the pause.
// Each reason is said once on the terminal and is the store's reason of the
// pause, and the command's standard output reaches neither.
func TestRenewal(t *testing.T) {
	was := credentialLimit
	credentialLimit = 300 * time.Millisecond
	t.Cleanup(func() { credentialLimit = was })
	b, clk, cmd := withClock(t, 3)
	e := newFakeEngine()
	r := b.start(t, e, 3)

	// The start: kept, then pushed before the first tick, which b.start waited for.
	if got := e.pushed(); !slices.Equal(got, []string{"cred-1"}) || cmd.runs(t) != 1 {
		t.Fatalf("at the start: pushed %q after %d runs of the command, want cred-1 once", got, cmd.runs(t))
	}
	e.mu.Lock()
	keeps := slices.Clone(e.keeps)
	e.mu.Unlock()
	if len(keeps) != 1 || keeps[0] != (seal.Proxy{Name: "kiln-proxy", Image: "kiln-proxy", Network: "kiln", Upstream: b.cfg.Upstream}) {
		t.Errorf("the proxy kept: %+v", keeps)
	}
	e.mu.Lock()
	firstTick := e.firstTick
	e.mu.Unlock()
	if firstTick != 1 {
		t.Errorf("when the first tick began %d credentials were pushed, want 1", firstTick)
	}
	if d := clk.waiting(t, 1); d != renewEvery {
		t.Fatalf("after the start the renewal waits %s, want %s", d, renewEvery)
	}

	// Five minutes after the last success, and not before.
	cmd.set(t, "echo cred-2")
	clk.advance(renewEvery - time.Second)
	e.waitTicks(t, 3)
	if n := cmd.runs(t); n != 1 {
		t.Errorf("before five minutes the command ran %d times", n)
	}
	clk.advance(time.Second)
	if d := clk.waiting(t, 2); d != renewEvery {
		t.Errorf("after a renewal the renewal waits %s", d)
	}
	clk.advance(renewEvery) // the same credential again
	clk.waiting(t, 3)
	if got := e.pushed(); !slices.Equal(got, []string{"cred-1", "cred-2", "cred-2"}) {
		t.Errorf("pushed %q", got)
	}

	// Unavailable, for one reason after another, each tried every 30 seconds.
	const secret = "kiln-secret-stdout"
	steps := []struct {
		script, reason string
		pushFails      bool
	}{
		{"echo " + secret + "; echo 'the sign-in has expired' >&2; exit 3",
			"the credential command failed: exit status 3: the sign-in has expired", false},
		{"echo " + secret + "; echo 'the sign-in has expired' >&2; exit 3",
			"the credential command failed: exit status 3: the sign-in has expired", false},
		{"echo '  '", "the credential command printed nothing", false},
		// A child left behind, which is ended with the command's process group.
		{"sleep 120 & echo $! >\"$d/child\"; echo " + secret + "; wait", "the credential command did not end within 300ms", false},
		{"head -c 70000 /dev/zero | tr '\\000' s", "the credential command printed more than 64 KiB", false},
		{"echo cred-3", "the credential could not be given to the proxy: the push failed, as the test has it", true},
	}
	n := 3
	for i, step := range steps {
		cmd.set(t, step.script)
		e.mu.Lock()
		e.pushFails = step.pushFails
		e.mu.Unlock()
		if i == 0 {
			clk.advance(renewEvery)
		} else {
			clk.advance(retryEvery)
		}
		n++
		if d := clk.waiting(t, n); d != retryEvery {
			t.Errorf("%s: the renewal waits %s, want %s", step.reason, d, retryEvery)
		}
		if got, want := b.paused(t), unavailable+step.reason; got != want {
			t.Errorf("the store says paused %q, want %q", got, want)
		}
		if strings.Contains(step.script, "child") {
			data, err := os.ReadFile(filepath.Join(cmd.dir, "child"))
			if err != nil {
				t.Fatal(err)
			}
			var pid int
			if _, err := fmt.Sscan(string(data), &pid); err != nil {
				t.Fatal(err)
			}
			// The command has ended, so its group has been killed: the child is
			// gone within moments, not when its sleep is over.
			deadline := time.Now().Add(3 * time.Second)
			for !ended(pid) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if !ended(pid) {
				syscall.Kill(pid, syscall.SIGKILL)
				t.Errorf("the command's child %d outlived the command's limit", pid)
			}
		}
	}
	e.mu.Lock()
	e.pushFails = false
	e.mu.Unlock()
	cmd.set(t, "echo cred-4")
	clk.advance(retryEvery)
	n++
	if d := clk.waiting(t, n); d != renewEvery {
		t.Errorf("after the pause the renewal waits %s, want %s", d, renewEvery)
	}
	if got := b.paused(t); got != "" {
		t.Errorf("after a success the store says paused %q", got)
	}
	if got := e.pushed(); !slices.Equal(got, []string{"cred-1", "cred-2", "cred-2", "cred-4"}) {
		t.Errorf("pushed %q", got)
	}
	r.stop(t)

	out := r.out.String()
	if strings.Contains(out, secret) || strings.Contains(out, "cred-") || strings.Contains(out, "sssss") {
		t.Errorf("the terminal shows what the command printed:\n%s", out)
	}
	if got := strings.Count(out, "kiln core: paused: "); got != len(steps)-1 {
		t.Errorf("the terminal says paused %d times, want once per reason, %d:\n%s", got, len(steps)-1, out)
	}
	if got := strings.Count(out, "kiln core: the Claude credential is available again; runs start"); got != 1 {
		t.Errorf("the terminal says the pause ended %d times:\n%s", got, out)
	}
	b.with(t, func(s *store.Store) {
		h, err := s.InstanceHistory()
		if err != nil {
			t.Fatal(err)
		}
		if len(h) != len(steps) {
			t.Errorf("the instance has %d entries, want %d: %+v", len(h), len(steps), h)
		}
		for _, e := range h {
			if strings.Contains(e.Paused, secret) || strings.Contains(e.Paused, "cred-") {
				t.Errorf("the store keeps what the command printed: %q", e.Paused)
			}
		}
	})
}

// TestRefusedRenews hands a followed run's refused credential to the core:
// the first refusal renews at once, a second within ten seconds does not,
// and one after ten seconds does again.
func TestRefusedRenews(t *testing.T) {
	b, clk, cmd := withClock(t, 3)
	var card int64
	b.with(t, func(s *store.Store) { card = b.card(t, s, store.Spec) })
	e := newFakeEngine()
	r := b.start(t, e, 3)
	e.waitStarted(t, group(card))
	clk.waiting(t, 1)
	e.mu.Lock()
	refused := e.inputs[card].Refused
	e.mu.Unlock()
	if refused == nil {
		t.Fatal("the follower hands no Refused to Carry")
	}

	cmd.set(t, "echo cred-2")
	refused()
	if d := clk.waiting(t, 2); d != renewEvery {
		t.Errorf("after the renewal the renewal waits %s", d)
	}
	if got := e.pushed(); !slices.Equal(got, []string{"cred-1", "cred-2"}) {
		t.Fatalf("after a refusal: pushed %q", got)
	}

	refused()
	clk.advance(refusedEvery - time.Second)
	refused()
	e.waitTicks(t, 5)
	if got := clk.count(); got != 2 || cmd.runs(t) != 2 {
		t.Errorf("within ten seconds a refusal renewed: %d waits, %d runs of the command", got, cmd.runs(t))
	}

	clk.advance(time.Second)
	refused()
	clk.waiting(t, 3)
	if got := e.pushed(); !slices.Equal(got, []string{"cred-1", "cred-2", "cred-2"}) {
		t.Errorf("after ten seconds: pushed %q", got)
	}
	e.open(card, "exit")
	b.waitEnded(t, e, card)
	r.stop(t)
}

// waitEnded waits until a card's run has ended in the store: the card has
// a run, none is in flight, and the card no longer waits for one. A card
// that b.card staged in implement or review already has ended runs, so the
// count alone holds before the core has started the stage's run.
func (b *bench) waitEnded(t *testing.T, e *fakeEngine, card int64) {
	t.Helper()
	waitUntil(t, fmt.Sprintf("the run of card %d to end", card), func() bool {
		s, err := store.Open(filepath.Join(b.cfg.Data, "store.db"))
		if err != nil {
			return false
		}
		defer s.Close()
		return runEnded(s, card)
	})
}

// runEnded reports whether the card has had a run, has none in flight and
// no longer waits for one.
func runEnded(s *store.Store, card int64) bool {
	c, err := s.Card(card)
	if err != nil || c.WaitsFor == store.Run {
		return false
	}
	r, err := s.InFlight(card)
	n, _ := s.RunCount(card)
	return err == nil && r == nil && n > 0
}

// TestRunEnded checks runEnded at each step of a review card's run, with no
// core: the card staged in review already has two ended runs, so the count
// alone would hold before the review run starts.
func TestRunEnded(t *testing.T) {
	b := newBench(t, 1)
	b.with(t, func(s *store.Store) {
		card := b.card(t, s, store.Review)
		if runEnded(s, card) {
			t.Fatal("a card staged in review counts as ended before its review run started")
		}
		id := b.runID(card, 3)
		if err := s.AddRun(store.RunRow{ID: id, Card: card, Commit: b.commit, Model: "m", Effort: "high"}, MaxRuns); err != nil {
			t.Fatal(err)
		}
		if runEnded(s, card) {
			t.Fatal("a card with a run in flight counts as ended")
		}
		if _, err := s.EndRun(id, store.Finished, "ended by the test", reviewPassed, store.Cost{}, nil); err != nil {
			t.Fatal(err)
		}
		if !runEnded(s, card) {
			t.Fatal("a card whose review run ended does not count as ended")
		}
	})
}

// TestPausedStartsNoRun pauses a core while a run is in flight and another
// card waits for room: the run in flight ends as it would, the other card's
// run does not start while the pause lasts, the board says so in one line, and
// the success that ends the pause starts it.
func TestPausedStartsNoRun(t *testing.T) {
	b, clk, cmd := withClock(t, 1)
	b.cfg.Listen = freeAddress(t)
	var first, second int64
	b.with(t, func(s *store.Store) {
		first, second = b.card(t, s, store.Spec), b.card(t, s, store.Spec)
	})
	e := newFakeEngine()
	r := b.start(t, e, 1)
	e.waitStarted(t, group(first))
	clk.waiting(t, 1)

	const secret = "kiln-secret-stdout"
	cmd.set(t, "echo "+secret+"; echo 'the sign-in has expired' >&2; exit 1")
	clk.advance(renewEvery)
	clk.waiting(t, 2)
	why := unavailable + "the credential command failed: exit status 1: the sign-in has expired"
	if got := b.paused(t); got != why {
		t.Fatalf("the store says paused %q, want %q", got, why)
	}

	// The board says so, and not what the command printed.
	resp, err := http.Get("http://" + b.cfg.Listen + "/api/board")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"paused":"`+why+`"`) || strings.Contains(string(body), secret) {
		t.Errorf("the board while paused: %s", body)
	}

	// The run in flight is left alone and ends; the waiting card gets none.
	e.open(first, "exit")
	b.waitEnded(t, e, first)
	e.waitTicks(t, 5)
	if got := e.snapshot().started; !slices.Equal(got, []int64{first}) {
		t.Fatalf("while paused the runs of %v started", got)
	}

	cmd.set(t, "echo cred-2")
	clk.advance(retryEvery)
	e.waitStarted(t, group(first), group(second))
	if got := b.paused(t); got != "" {
		t.Errorf("after the pause the store says paused %q", got)
	}
	resp, err = http.Get("http://" + b.cfg.Listen + "/api/board")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"paused":""`) {
		t.Errorf("the board after the pause: %s", body)
	}
	e.open(second, "exit")
	b.waitEnded(t, e, second)
	r.stop(t)
	if out := r.out.String(); strings.Contains(out, secret) {
		t.Errorf("the terminal shows what the command printed:\n%s", out)
	}
}

// TestCredentialRefusedConfig refuses a core without a credential command or
// with an upstream that is not an http or https URL, before anything is made.
func TestCredentialRefusedConfig(t *testing.T) {
	for _, tc := range []struct{ upstream, command, says string }{
		{"http://upstream.kiln-test.example", "", "credential command"},
		{"", "echo c", "upstream"},
		{"upstream.kiln-test.example", "echo c", "upstream"},
		{"ftp://upstream.kiln-test.example", "echo c", "upstream"},
	} {
		data := filepath.Join(t.TempDir(), "data")
		err := runCore(t.Context(), Config{Data: data, Repo: "/repo.git", Model: "m", Runs: 1, Upstream: tc.upstream,
			CredentialCommand: tc.command}, io.Discard, io.Discard, newFakeEngine(), time.Millisecond, newFakeClock(), nil)
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("upstream %q, command %q: %v", tc.upstream, tc.command, err)
		}
		if _, err := os.Stat(data); !os.IsNotExist(err) {
			t.Errorf("upstream %q, command %q made the data directory: %v", tc.upstream, tc.command, err)
		}
	}
}

// TestUnavailableAtStart starts a core whose credential command fails: the
// first tick starts no run, and the card waits until the command succeeds.
func TestUnavailableAtStart(t *testing.T) {
	b, clk, cmd := withClock(t, 3)
	cmd.set(t, "echo 'the sign-in has expired' >&2; exit 1")
	var card int64
	b.with(t, func(s *store.Store) { card = b.card(t, s, store.Spec) })
	e := newFakeEngine()
	r := b.start(t, e, 3)
	if d := clk.waiting(t, 1); d != retryEvery {
		t.Errorf("after a failed start the renewal waits %s, want %s", d, retryEvery)
	}
	e.waitTicks(t, 5)
	if got := e.snapshot().started; len(got) != 0 {
		t.Fatalf("while paused from the start the runs of %v started", got)
	}
	cmd.set(t, "echo cred-1")
	clk.advance(retryEvery)
	e.waitStarted(t, group(card))
	e.open(card, "exit")
	b.waitEnded(t, e, card)
	r.stop(t)
}

// TestPushHangs gives the core an engine whose push never ends: each push is
// cut at pushLimit, counts as unavailable with a reason that is the same each
// time, and is said and written once.
func TestPushHangs(t *testing.T) {
	was := pushLimit
	pushLimit = 200 * time.Millisecond
	t.Cleanup(func() { pushLimit = was })
	b, clk, _ := withClock(t, 3)
	e := newFakeEngine()
	e.pushHangs = true
	r := b.start(t, e, 3)
	why := unavailable + "the credential could not be given to the proxy: the push did not end within 200ms"
	for n := 1; n <= 3; n++ {
		if d := clk.waiting(t, n); d != retryEvery {
			t.Errorf("after a push that hung the renewal waits %s, want %s", d, retryEvery)
		}
		if got := b.paused(t); got != why {
			t.Errorf("the store says paused %q, want %q", got, why)
		}
		clk.advance(retryEvery)
	}
	clk.waiting(t, 4)
	r.stop(t)
	if got := strings.Count(r.out.String(), "kiln core: paused: "); got != 1 {
		t.Errorf("the terminal says paused %d times, want once:\n%s", got, r.out.String())
	}
	b.with(t, func(s *store.Store) {
		if h, err := s.InstanceHistory(); err != nil || len(h) != 1 {
			t.Errorf("the instance's history is %+v, %v; want one entry", h, err)
		}
	})
}

// TestPauseNotWritten fails the store's write of the outcome: it counts as a
// failure, tried again after retryEvery, and the terminal says nothing of a
// pause or its end until the write has succeeded.
func TestPauseNotWritten(t *testing.T) {
	var mu sync.Mutex
	fails := 2
	was := setPaused
	setPaused = func(s *store.Store, reason string) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			return false, errors.New("the store is busy, as the test has it")
		}
		return s.SetPaused(reason)
	}
	t.Cleanup(func() { setPaused = was })
	b, clk, cmd := withClock(t, 3)
	b.with(t, func(s *store.Store) {
		if _, err := s.SetPaused(unavailable + "from before"); err != nil {
			t.Fatal(err)
		}
	})
	e := newFakeEngine()
	r := b.start(t, e, 3)
	if d := clk.waiting(t, 1); d != retryEvery {
		t.Fatalf("after a write that failed the renewal waits %s, want %s", d, retryEvery)
	}
	clk.advance(retryEvery)
	if d := clk.waiting(t, 2); d != retryEvery {
		t.Fatalf("after a second write that failed the renewal waits %s, want %s", d, retryEvery)
	}
	if got := b.paused(t); got != unavailable+"from before" {
		t.Errorf("the store says paused %q", got)
	}
	clk.advance(retryEvery)
	if d := clk.waiting(t, 3); d != renewEvery {
		t.Errorf("after the write the renewal waits %s, want %s", d, renewEvery)
	}
	r.stop(t)
	out := r.out.String()
	if got := strings.Count(out, "kiln core: the pause cannot be written"); got != 2 {
		t.Errorf("the terminal says %d times that the pause cannot be written:\n%s", got, out)
	}
	if got := strings.Count(out, "available again"); got != 1 {
		t.Errorf("the terminal says %d times that the pause ended:\n%s", got, out)
	}
	if cmd.runs(t) != 3 {
		t.Errorf("the command ran %d times, want 3", cmd.runs(t))
	}
}

// TestStartedWhilePaused starts a core on a store that is paused for the
// reason the command still fails with, with a run in flight: the run is
// carried to its end, no other run starts, and the terminal does not say the
// reason again, since the store's state did not change.
func TestStartedWhilePaused(t *testing.T) {
	b, clk, cmd := withClock(t, 3)
	cmd.set(t, "echo 'the sign-in has expired' >&2; exit 1")
	why := unavailable + "the credential command failed: exit status 1: the sign-in has expired"
	var flying, waits int64
	b.with(t, func(s *store.Store) {
		flying, waits = b.card(t, s, store.Spec), b.card(t, s, store.Spec)
		if err := s.AddRun(store.RunRow{ID: fmt.Sprintf("%s-%d-1", b.instance, flying), Card: flying, Commit: b.commit,
			Model: b.cfg.Model, Effort: "high"}, MaxRuns); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetPaused(why); err != nil {
			t.Fatal(err)
		}
	})
	e := newFakeEngine()
	e.set(fmt.Sprintf("%s-%d-1", b.instance, flying), "running")
	r := b.start(t, e, 3)
	clk.waiting(t, 1)
	waitUntil(t, "the run in flight to be carried", func() bool { return e.snapshot().carried[flying] > 0 })
	e.open(flying, "exit")
	b.waitEnded(t, e, flying)
	e.waitTicks(t, 5)
	if got := e.snapshot().carried[waits]; got != 0 {
		t.Errorf("while paused the waiting card's run was carried %d times", got)
	}
	r.stop(t)
	if out := r.out.String(); strings.Contains(out, "kiln core: paused: ") {
		t.Errorf("a core started on the same pause says it again:\n%s", out)
	}
	b.with(t, func(s *store.Store) {
		if h, err := s.InstanceHistory(); err != nil || len(h) != 1 {
			t.Errorf("the instance's history is %+v, %v; want the one entry from before", h, err)
		}
	})
}
