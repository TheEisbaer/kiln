package tick

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/seal"
	"github.com/TheEisbaer/kiln/internal/store"
)

// fakeEngine stands in for the seal and internal/run in the tests of the
// followers, which need no container engine. A run's container exists from
// Carry's first call until Remove or RemoveRun; it is created, then started
// once starting has returned, runs until the test opens its card's gate, and
// then exits with its record written. Carry can be held at three more points
// of a card's run: after the container is created and before starting is
// called ("add"), before a created container starts ("start"), and after the
// record and before Carry returns ("end").
type fakeEngine struct {
	mu         sync.Mutex
	containers map[string]string // a run's container's status, by the run's ID
	most       int               // the most containers there were at once
	started    []int64           // the cards whose runs' containers started, in order
	carrying   map[int64]int     // the calls of Carry under way, by card
	carried    map[int64]int     // the calls of Carry so far, by card
	twice      []int64           // the cards that had two calls of Carry at once
	removed    []string          // the runs RemoveRun removed
	lists      int               // the calls of ListRuns, one per tick
	gates      map[string]chan struct{}
	held       map[string]bool // the points that hold a card's run, as <card>/<point>
	fail       map[int64]bool  // the cards whose runs fail
	errOnce    map[int64]bool  // the cards whose first Carry fails once its run is in the store
	refuse     map[int64]bool  // the cards whose Carry always fails before starting
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{containers: map[string]string{}, carrying: map[int64]int{}, carried: map[int64]int{},
		gates: map[string]chan struct{}{}, held: map[string]bool{}, fail: map[int64]bool{}, errOnce: map[int64]bool{},
		refuse: map[int64]bool{}}
}

// cardOf is the card of a run's ID, <instance>-<card>-<number>.
func cardOf(id string) int64 {
	parts := strings.Split(id, "-")
	n, _ := strconv.ParseInt(parts[1], 10, 64)
	return n
}

// gate is the channel of a card's point, which open closes.
func (e *fakeEngine) gate(card int64, point string) chan struct{} {
	key := fmt.Sprint(card, "/", point)
	if e.gates[key] == nil {
		e.gates[key] = make(chan struct{})
	}
	return e.gates[key]
}

// hold makes a card's run wait at a point, "add", "start" or "end", until it
// is opened; every run waits at "exit".
func (e *fakeEngine) hold(card int64, point string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.held[fmt.Sprint(card, "/", point)] = true
}

// open lets a card's runs pass a point, once and for all.
func (e *fakeEngine) open(card int64, point string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	gate := e.gate(card, point)
	select {
	case <-gate:
	default:
		close(gate)
	}
}

// wait waits at a card's point, if it is held, or until the context is
// cancelled.
func (e *fakeEngine) wait(ctx context.Context, card int64, point string) error {
	e.mu.Lock()
	held := point == "exit" || e.held[fmt.Sprint(card, "/", point)]
	gate := e.gate(card, point)
	e.mu.Unlock()
	if !held {
		return nil
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *fakeEngine) ListRuns(_ context.Context, instance string) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lists++
	var ids []string
	for id := range e.containers {
		if strings.HasPrefix(id, instance+"-") {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func (e *fakeEngine) RemoveRun(_ context.Context, id, _ string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removed = append(e.removed, id)
	delete(e.containers, id)
	return nil
}

func (e *fakeEngine) InspectRun(_ context.Context, id string) (*seal.RunState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if status, ok := e.containers[id]; ok {
		return &seal.RunState{Status: status}, nil
	}
	return nil, nil
}

func (e *fakeEngine) Remove(_ context.Context, in run.Inputs) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.containers, in.ID)
	return nil
}

func (e *fakeEngine) ReadDocument(_, id, kind string) (run.Document, error) {
	text := map[string]string{store.KindSpec: "# Spec\n", store.KindReport: "# Report\n", store.KindReview: "Verdict: passes\n"}[kind]
	return run.Document{Text: []byte(text + id + "\n")}, nil
}

// set sets a run's container's status and counts the containers.
func (e *fakeEngine) set(id, status string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.containers[id] = status
	e.most = max(e.most, len(e.containers))
	if status == "running" {
		e.started = append(e.started, cardOf(id))
	}
}

// Carry is run.Carry on the fake's containers.
func (e *fakeEngine) Carry(ctx context.Context, in run.Inputs, starting func() error, _ io.Writer) (run.Verdict, error) {
	card := cardOf(in.ID)
	e.mu.Lock()
	e.carrying[card]++
	e.carried[card]++
	if e.carrying[card] > 1 {
		e.twice = append(e.twice, card)
	}
	status, there := e.containers[in.ID]
	errOnce := e.errOnce[card]
	delete(e.errOnce, card)
	refuse := e.refuse[card]
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.carrying[card]--
		e.mu.Unlock()
	}()

	record := filepath.Join(in.Runs, in.ID, "result.json")
	recorded, err := exists(record)
	if err != nil || recorded {
		return e.verdict(in), err
	}
	if !there && starting == nil {
		return run.Verdict{}, run.ErrGone
	}
	if refuse && starting != nil {
		// As Carry refuses a branch that is neither absent nor at the commit,
		// before anything is made.
		return run.Verdict{}, errors.New("refused before the start, as the test has it")
	}
	if !there {
		e.set(in.ID, "created")
		status = "created"
		if err := e.wait(ctx, card, "add"); err != nil {
			e.Remove(ctx, in)
			return run.Verdict{}, err
		}
	}
	if status == "created" {
		if starting != nil {
			if err := starting(); err != nil {
				e.Remove(ctx, in)
				return run.Verdict{}, err
			}
		}
		if errOnce {
			return run.Verdict{}, errors.New("the start failed, as the test has it")
		}
		if err := e.wait(ctx, card, "start"); err != nil {
			return run.Verdict{}, err
		}
		e.set(in.ID, "running")
		status = "running"
	}
	if status == "running" {
		if err := e.wait(ctx, card, "exit"); err != nil {
			return run.Verdict{}, run.ErrInterrupted
		}
		e.set(in.ID, "exited")
	}
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		return run.Verdict{}, err
	}
	if err := os.WriteFile(record, []byte("{}\n"), 0o600); err != nil {
		return run.Verdict{}, err
	}
	if err := e.wait(ctx, card, "end"); err != nil {
		return run.Verdict{}, err
	}
	return e.verdict(in), nil
}

// verdict is how a run of the fake ends: failed for a card the test fails,
// and finished otherwise, with the web for a spec run and no change for an
// implement run.
func (e *fakeEngine) verdict(in run.Inputs) run.Verdict {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fail[cardOf(in.ID)] {
		return run.Verdict{Reason: "exit 1, no result, not saved", Saved: "not saved"}
	}
	return run.Verdict{Finished: true, Reason: "exit 0, result success, 1 turns, $0.0001, no change", Hosts: in.Hosts, Saved: "no change"}
}

// seen is what the fake has seen so far.
type seen struct {
	containers map[string]string
	most       int
	started    []int64
	carrying   map[int64]int
	carried    map[int64]int
	twice      []int64
	removed    []string
	lists      int
}

// snapshot is what the fake has seen so far, read under its lock.
func (e *fakeEngine) snapshot() seen {
	e.mu.Lock()
	defer e.mu.Unlock()
	return seen{containers: maps.Clone(e.containers), most: e.most, started: slices.Clone(e.started),
		carrying: maps.Clone(e.carrying), carried: maps.Clone(e.carried), twice: slices.Clone(e.twice),
		removed: slices.Clone(e.removed), lists: e.lists}
}

func cloned[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// waitTicks waits until the core has ticked n more times.
func (e *fakeEngine) waitTicks(t *testing.T, n int) {
	t.Helper()
	from := e.snapshot().lists
	waitUntil(t, fmt.Sprintf("%d more ticks", n), func() bool { return e.snapshot().lists >= from+n })
}

// bench is a data directory and a repo for cores on the fake engine.
type bench struct {
	cfg      Config
	instance string
	commit   string // main's head
}

// newBench makes a data directory with its store, and a repo whose main is at
// one commit.
func newBench(t *testing.T, runs int) *bench {
	t.Helper()
	b := &bench{}
	repo, commit := bareRepo(t, map[string]string{"NOTES.md": "# Notes\n"})
	b.cfg = Config{Data: filepath.Join(t.TempDir(), "data"), Repo: repo, Model: "claude-opus-5-5",
		ImplementModel: "claude-sonnet-5-5", ReviewModel: "claude-opus-5-5", Image: "kiln-run", Proxy: "kiln-proxy",
		Hosts: []string{"go.dev"}, Runs: runs}
	b.commit = commit
	b.with(t, func(s *store.Store) {
		var err error
		if b.instance, err = s.Instance(); err != nil {
			t.Fatal(err)
		}
	})
	return b
}

// with opens the bench's store, while no core runs, and closes it after f.
//
// The tests of the followers do not run side by side: a child that a test
// forks holds a copy of the files of the whole process, the lock of another
// test's core included, until it has started its program, and a core that
// starts in that moment is refused.
func (b *bench) with(t *testing.T, f func(s *store.Store)) {
	t.Helper()
	s, lock, err := openStore(b.cfg.Data)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer s.Close()
	f(s)
}

// card writes a card that waits for a run in the stage: a spec run's straight
// away, an implement run's after a spec run that ended with a spec and the
// developer's approval, and a review run's after an implement run as well,
// with a commit on the card's branch. The runs that stage it are ended in the
// store and have no container.
func (b *bench) card(t *testing.T, s *store.Store, stage store.Stage) int64 {
	t.Helper()
	c, err := s.Create("A card in " + string(stage) + ".")
	if err != nil {
		t.Fatal(err)
	}
	end := func(n int, from, to store.State, doc *store.Doc) {
		t.Helper()
		if _, err := s.Move(c.ID, from, store.CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("%s-%d-%d", b.instance, c.ID, n)
		if err := s.AddRun(store.RunRow{ID: id, Card: c.ID, Commit: b.commit, Model: "m", Effort: "high"}, MaxRuns); err != nil {
			t.Fatal(err)
		}
		if _, err := s.EndRun(id, store.Finished, "staged", to, doc); err != nil {
			t.Fatal(err)
		}
	}
	switch stage {
	case store.Implement, store.Review:
		end(1, waiting, finished, &store.Doc{Kind: store.KindSpec, Text: "# The spec\n"})
		if stage == store.Review {
			end(2, store.State{Stage: store.Implement, WaitsFor: store.Run}, reviewWaits, &store.Doc{Kind: store.KindReport, Text: "# Report\n"})
			b.branch(t, c.ID)
			return c.ID
		}
		if _, err := s.Move(c.ID, store.State{Stage: store.Implement, WaitsFor: store.Run}, store.CauseDeveloper); err != nil {
			t.Fatal(err)
		}
	default:
		if _, err := s.Move(c.ID, waiting, store.CauseDeveloper); err != nil {
			t.Fatal(err)
		}
	}
	return c.ID
}

// branch puts a commit on a card's branch, as a save would.
func (b *bench) branch(t *testing.T, card int64) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	gitIn(t, filepath.Dir(work), "clone", "--quiet", b.cfg.Repo, work)
	if err := os.WriteFile(filepath.Join(work, "NOTES.md"), []byte("# Notes\nA line.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "commit", "--quiet", "--all", "--message", "a save")
	gitIn(t, work, "push", "--quiet", "origin", fmt.Sprintf("HEAD:refs/heads/kiln/card-%d", card))
}

// running is a core on the fake engine in a goroutine.
type running struct {
	cancel context.CancelFunc
	done   chan error
	out    *bytes.Buffer // what it printed, both streams; read only after it has returned
}

// start starts a core of the bench on the engine, with a tick of 5 ms.
func (b *bench) start(t *testing.T, e *fakeEngine, runs int) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &running{cancel: cancel, done: make(chan error, 1), out: &bytes.Buffer{}}
	cfg := b.cfg
	cfg.Runs = runs
	go func() { r.done <- runCore(ctx, cfg, r.out, r.out, e, 5*time.Millisecond) }()
	e.waitTicks(t, 1) // a core that cannot start says so here, not in a wait for its runs
	t.Cleanup(func() {
		cancel()
		<-r.done
	})
	return r
}

// stop cancels the core's context, as Ctrl-C does, and waits for it to return.
func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("the core returned %v", err)
		}
		r.done <- nil // for the cleanup
	case <-time.After(waitLimit):
		t.Fatal("the core did not return after its context was cancelled")
	}
}

// waitStarted waits until the runs of the cards in the groups have started,
// group after group. The runs of one group start in one tick, each in its
// own follower, so in any order among them.
func (e *fakeEngine) waitStarted(t *testing.T, groups ...[]int64) {
	t.Helper()
	var want []int64
	for _, g := range groups {
		want = append(want, g...)
	}
	waitUntil(t, fmt.Sprintf("the runs of cards %v start", groups), func() bool { return len(e.snapshot().started) >= len(want) })
	got := e.snapshot().started
	ok := len(got) == len(want)
	for i, rest := 0, got; ok && i < len(groups); i++ {
		n := len(groups[i])
		ok = sameSet(rest[:n], groups[i])
		rest = rest[n:]
	}
	if !ok {
		t.Fatalf("the runs started for the cards %v, want %v", got, groups)
	}
}

// sameSet reports whether two lists hold the same cards.
func sameSet(a, b []int64) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// group is a group of cards for waitStarted.
func group(cards ...int64) []int64 { return cards }

// waitGone waits until a card's run has ended and its container is removed.
func (e *fakeEngine) waitGone(t *testing.T, cards ...int64) {
	t.Helper()
	waitUntil(t, fmt.Sprintf("the runs of cards %v end", cards), func() bool {
		s := e.snapshot()
		for id := range s.containers {
			if slices.Contains(cards, cardOf(id)) {
				return false
			}
		}
		for _, card := range cards {
			if s.carrying[card] > 0 {
				return false
			}
		}
		return true
	})
}

// assertEnded asserts, once no core runs, that each card's last run has ended
// as given and that it no longer waits for a run, and that no run is in
// flight.
func (b *bench) assertEnded(t *testing.T, want map[int64]store.Ending) {
	t.Helper()
	b.with(t, func(s *store.Store) {
		for card, ending := range want {
			c, err := s.Card(card)
			if err != nil {
				t.Fatal(err)
			}
			runs, err := s.Runs(card)
			if err != nil || len(runs) == 0 {
				t.Fatalf("card %d has runs %+v (%v)", card, runs, err)
			}
			if last := runs[len(runs)-1]; last.Ending != ending || c.WaitsFor == store.Run {
				t.Errorf("card %d is in %s waiting for %s, its last run %s %q, want %s", card, c.Stage, c.WaitsFor, last.ID, last.Ending, ending)
			}
		}
		if runs, err := s.InFlightRuns(); err != nil || len(runs) != 0 {
			t.Errorf("runs in flight: %v (%v)", runs, err)
		}
	})
}

// TestFollowersLimitAndOrder has a core with a limit of 2 and four waiting
// cards: never more than two runs are in flight, the card furthest along
// comes first and, in a stage, the one that has waited longest, and all four
// end. A card whose run is followed gets no second follower over many ticks.
func TestFollowersLimitAndOrder(t *testing.T) {
	b := newBench(t, 2)
	var spec1, implement, spec2, review int64
	b.with(t, func(s *store.Store) {
		spec1 = b.card(t, s, store.Spec)
		implement = b.card(t, s, store.Implement)
		spec2 = b.card(t, s, store.Spec)
		review = b.card(t, s, store.Review)
	})
	e := newFakeEngine()
	core := b.start(t, e, 2)

	e.waitStarted(t, group(review, implement))
	e.waitTicks(t, 20)
	if s := e.snapshot(); len(s.started) != 2 || len(s.containers) != 2 {
		t.Fatalf("with a limit of 2, the runs of cards %v started, and the containers are %v", s.started, s.containers)
	}
	e.open(review, "exit")
	e.waitStarted(t, group(review, implement), group(spec1))
	e.waitTicks(t, 20)
	if s := e.snapshot(); len(s.started) != 3 {
		t.Fatalf("the runs of cards %v started, want three", s.started)
	}
	e.open(implement, "exit")
	e.waitStarted(t, group(review, implement), group(spec1), group(spec2))
	e.open(spec1, "exit")
	e.open(spec2, "exit")
	e.waitGone(t, review, implement, spec1, spec2)
	core.stop(t)

	s := e.snapshot()
	if s.most != 2 {
		t.Errorf("at most %d containers were there at once, want 2", s.most)
	}
	if len(s.twice) != 0 {
		t.Errorf("the cards %v had two followers at once", s.twice)
	}
	if want := map[int64]int{review: 1, implement: 1, spec1: 1, spec2: 1}; !reflect.DeepEqual(s.carried, want) {
		t.Errorf("each card's run was carried %v times, want %v", s.carried, want)
	}
	if len(s.removed) != 0 {
		t.Errorf("the sweep removed %v", s.removed)
	}
	b.assertEnded(t, map[int64]store.Ending{review: store.Finished, implement: store.Finished, spec1: store.Finished, spec2: store.Finished})
}

// TestFollowersApart has one run that fails, and one whose follower fails
// after its start; each touches only its card, is printed with the card's ID,
// and the other followers and the tick go on.
func TestFollowersApart(t *testing.T) {
	b := newBench(t, 3)
	var ok, fails, errs int64
	b.with(t, func(s *store.Store) {
		ok, fails, errs = b.card(t, s, store.Spec), b.card(t, s, store.Spec), b.card(t, s, store.Spec)
	})
	e := newFakeEngine()
	e.fail[fails] = true
	e.errOnce[errs] = true
	core := b.start(t, e, 3)

	e.waitStarted(t, group(ok, fails, errs))
	e.open(fails, "exit")
	e.waitGone(t, fails)
	e.open(errs, "exit")
	e.waitGone(t, errs)
	e.waitTicks(t, 5)
	if s := e.snapshot(); s.containers[fmt.Sprintf("%s-%d-1", b.instance, ok)] != "running" || s.carrying[ok] != 1 {
		t.Errorf("the run of card %d is %q with %d followers while the others ended", ok, s.containers[fmt.Sprintf("%s-%d-1", b.instance, ok)], s.carrying[ok])
	}
	e.open(ok, "exit")
	e.waitGone(t, ok)
	core.stop(t)

	out := core.out.String()
	if !strings.Contains(out, fmt.Sprintf("card %d: run %s-%d-1 failed: exit 1", fails, b.instance, fails)) ||
		!strings.Contains(out, fmt.Sprintf("kiln core: card %d: the start failed, as the test has it", errs)) {
		t.Errorf("the core printed\n%s", out)
	}
	if s := e.snapshot(); s.carried[errs] != 2 || s.carried[ok] != 1 || len(s.twice) != 0 {
		t.Errorf("the runs were carried %v times, and %v had two followers at once", s.carried, s.twice)
	}
	b.assertEnded(t, map[int64]store.Ending{ok: store.Finished, fails: store.Failed, errs: store.Finished})
}

// TestFollowersPickedUp stops a core with three runs in flight, as Ctrl-C
// does: it returns once every follower has, and the runs go on. A core started
// again with a limit of 2 picks up all three, and starts no fourth run until
// fewer than two are in flight.
func TestFollowersPickedUp(t *testing.T) {
	b := newBench(t, 3)
	var cards []int64
	b.with(t, func(s *store.Store) {
		for range 3 {
			cards = append(cards, b.card(t, s, store.Spec))
		}
	})
	e := newFakeEngine()
	core := b.start(t, e, 3)
	e.waitStarted(t, cards)
	core.stop(t)
	if s := e.snapshot(); len(s.containers) != 3 || s.carrying[cards[0]]+s.carrying[cards[1]]+s.carrying[cards[2]] != 0 {
		t.Fatalf("after Ctrl-C the containers are %v and Carry is under way %v", s.containers, s.carrying)
	}
	var fourth int64
	b.with(t, func(s *store.Store) {
		fourth = b.card(t, s, store.Spec)
		if runs, err := s.InFlightRuns(); err != nil || len(runs) != 3 {
			t.Fatalf("runs in flight after Ctrl-C: %v (%v)", runs, err)
		}
	})

	core = b.start(t, e, 2)
	waitUntil(t, "the three runs are followed again", func() bool {
		s := e.snapshot()
		return s.carrying[cards[0]] == 1 && s.carrying[cards[1]] == 1 && s.carrying[cards[2]] == 1
	})
	e.waitTicks(t, 20)
	e.open(cards[0], "exit")
	e.waitGone(t, cards[0])
	e.waitTicks(t, 20)
	if s := e.snapshot(); len(s.started) != 3 {
		t.Fatalf("with two runs in flight and a limit of 2, the runs of cards %v started", s.started)
	}
	e.open(cards[1], "exit")
	e.waitStarted(t, cards, group(fourth))
	e.open(cards[2], "exit")
	e.open(fourth, "exit")
	e.waitGone(t, append(cards, fourth)...)
	core.stop(t)

	s := e.snapshot()
	if len(s.twice) != 0 || s.most != 3 {
		t.Errorf("the cards %v had two followers at once; at most %d containers", s.twice, s.most)
	}
	if want := map[int64]int{cards[0]: 2, cards[1]: 2, cards[2]: 2, fourth: 1}; !reflect.DeepEqual(s.carried, want) {
		t.Errorf("each card's run was carried %v times, want %v", s.carried, want)
	}
	b.assertEnded(t, map[int64]store.Ending{cards[0]: store.Finished, cards[1]: store.Finished, cards[2]: store.Finished, fourth: store.Finished})
}

// TestFollowersSwept has the sweep go over several runs in every state
// between "container made" and "ending recorded": made and not yet in the
// store, in the store and never started, running, and recorded and not yet
// ended in the store. It removes none of them, and removes a run of the
// instance that is in no follower and not in the store.
func TestFollowersSwept(t *testing.T) {
	b := newBench(t, 8)
	var cards []int64
	b.with(t, func(s *store.Store) {
		for range 4 {
			cards = append(cards, b.card(t, s, store.Spec))
		}
	})
	made, stored, runs, recorded := cards[0], cards[1], cards[2], cards[3]
	stranger := b.instance + "-99-1"
	e := newFakeEngine()
	e.containers[stranger] = "exited"
	e.hold(made, "add")
	// The first follower of stored fails after AddRun, and the next one waits
	// before it starts the container that is in the store.
	e.errOnce[stored] = true
	e.hold(stored, "start")
	e.hold(recorded, "end")
	e.open(recorded, "exit")
	core := b.start(t, e, 8)

	waitUntil(t, "every run is in its state", func() bool {
		s := e.snapshot()
		id := func(card int64) string { return fmt.Sprintf("%s-%d-1", b.instance, card) }
		return s.containers[id(made)] == "created" && s.containers[id(stored)] == "created" && s.carrying[stored] == 1 &&
			s.carried[stored] == 2 && s.containers[id(runs)] == "running" && s.containers[id(recorded)] == "exited"
	})
	e.waitTicks(t, 20)
	if s := e.snapshot(); !reflect.DeepEqual(s.removed, []string{stranger}) {
		t.Errorf("the sweep removed %v, want only %s", s.removed, stranger)
	}
	for _, card := range cards {
		e.open(card, "exit")
	}
	e.open(made, "add")
	e.open(stored, "start")
	e.open(recorded, "end")
	e.waitGone(t, cards...)
	core.stop(t)
	if s := e.snapshot(); !reflect.DeepEqual(s.removed, []string{stranger}) || len(s.containers) != 0 {
		t.Errorf("the sweep removed %v, and %v are left", s.removed, s.containers)
	}
	b.assertEnded(t, map[int64]store.Ending{made: store.Finished, stored: store.Finished, runs: store.Finished, recorded: store.Finished})
}

// TestFollowersRefusedStart has cards whose Carry always fails before its
// run is in the store, first in the order: with a limit of 1 and one such
// card, and with a limit of 2 and two, the card behind them starts and ends.
func TestFollowersRefusedStart(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprintf("--runs %d", n), func(t *testing.T) {
			b := newBench(t, n)
			var refused []int64
			var other int64
			b.with(t, func(s *store.Store) {
				for range n {
					refused = append(refused, b.card(t, s, store.Spec))
				}
				other = b.card(t, s, store.Spec)
			})
			e := newFakeEngine()
			for _, card := range refused {
				e.refuse[card] = true
			}
			core := b.start(t, e, n)
			e.waitStarted(t, group(other))
			e.open(other, "exit")
			e.waitGone(t, other)
			e.waitTicks(t, 5)
			core.stop(t)
			if s := e.snapshot(); len(s.started) != 1 || s.most > n {
				t.Errorf("the runs of cards %v started, with at most %d containers", s.started, s.most)
			}
			if out := core.out.String(); !strings.Contains(out, fmt.Sprintf("kiln core: card %d: refused before the start", refused[0])) {
				t.Errorf("the core printed\n%s", out)
			}
			b.assertEnded(t, map[int64]store.Ending{other: store.Finished})
			b.with(t, func(s *store.Store) {
				for _, card := range refused {
					if c, err := s.Card(card); err != nil || c.State != waiting {
						t.Errorf("the refused card %d is %+v (%v), want it waiting in spec", card, c, err)
					}
				}
			})
		})
	}
}

// TestRunsRefused refuses a limit outside 1 to MaxRuns before anything is
// made.
func TestRunsRefused(t *testing.T) {
	for _, n := range []int{0, -1, MaxRuns + 1} {
		data := filepath.Join(t.TempDir(), "data")
		err := runCore(t.Context(), Config{Data: data, Repo: "/repo.git", Model: "m", Runs: n}, io.Discard, io.Discard, newFakeEngine(), time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "--runs") {
			t.Errorf("--runs %d: %v", n, err)
		}
		if _, err := os.Stat(data); !os.IsNotExist(err) {
			t.Errorf("--runs %d made the data directory: %v", n, err)
		}
	}
}
