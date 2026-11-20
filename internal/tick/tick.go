// Package tick is the core's loop (ADR 0035): every tick it reconciles the
// store with the container engine, and gives a card that waits for a run its
// one run. A follower beside the tick carries each run to its end and records
// it, so that the next tick comes after the interval whatever the runs do; at
// most Config.Runs runs are in flight at once.
//
// A card that waits for a run gets the run of its stage: spec, implement or
// review (see stages). The data directory holds the store, store.db; the lock,
// lock; the runs' records, runs/<id>/, as internal/run writes them; and the
// runs' instructions, instructions/<id>.md: the installed Kiln's instruction
// for the run's stage, followed by what the run is told of its card.
//
// Only internal/seal drives the container engine; this package calls it and
// internal/run.
package tick

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/TheEisbaer/kiln/instructions"
	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/seal"
	"github.com/TheEisbaer/kiln/internal/store"
	"github.com/TheEisbaer/kiln/internal/web"
)

// Config is what `kiln core` is started with.
type Config struct {
	Data  string // the data directory
	Repo  string // the instance's copy of the target repo, a bare repo
	Model string // the model of a spec run that starts, an exact identifier
	// The models of an implement run and of a review run that start; Model
	// when empty (ADR 0030).
	ImplementModel string
	ReviewModel    string
	Image          string // the run image
	Proxy          string // the proxy container's name
	// The list of hosts of a spec run (ADR 0059), and its way out's image and
	// outward network. An empty list seals a spec run as before; implement and
	// review runs have no list.
	Hosts         []string
	WayOutImage   string
	WayOutNetwork string
	// The loopback address the screen is served on while the core ticks;
	// empty, none is served.
	Listen string
	// A file the screen's page is read from on every request instead of the
	// embedded one, for work on the screen; empty, the embedded page.
	Page string
	// The most runs of this instance in flight at once, from 1 to MaxRuns.
	Runs int
}

// The limit of runs in flight: DefaultRuns unless `kiln core --runs` says
// otherwise, and at most MaxRuns, since every run takes a network, and a
// container engine has about thirty address ranges for networks by default,
// which other work shares (card 17).
const (
	DefaultRuns = 3
	MaxRuns     = 8
)

// stage is what a run of a card's stage is: its effort, fixed per stage
// (ADR 0030), the instruction it is told first, the kind of the document it
// writes, and whether its working tree is saved.
type stage struct {
	effort      string
	instruction string
	kind        string
	save        bool
}

// stages are the stages a card can wait in for a run.
var stages = map[store.Stage]stage{
	store.Spec:      {"high", instructions.Spec, store.KindSpec, false},
	store.Implement: {"medium", instructions.Implement, store.KindReport, true},
	store.Review:    {"high", instructions.Review, store.KindReview, false},
}

// model is the model of a run of the stage.
func (c Config) model(s store.Stage) string {
	switch {
	case s == store.Implement && c.ImplementModel != "":
		return c.ImplementModel
	case s == store.Review && c.ReviewModel != "":
		return c.ReviewModel
	}
	return c.Model
}

// maxDiff is the largest change a review run is told: 1 MiB of diff.
const maxDiff = 1 << 20

// interval is the time between two ticks. Only the test changes it.
var interval = 2 * time.Second

// atPoint, set only by the test, is called at the named points of a tick and
// of a follower: ticked after each tick, followed when a follower has returned
// while the core goes on, and the points of a card's run. It can exit the
// process, as a kill there would. A point of a card's run is called twice: by
// its name, and as <stage>/<name>.
var atPoint func(point string)

func at(point string) {
	if atPoint != nil {
		atPoint(point)
	}
}

// The states a run moves its card to. A card waits in spec for a run, and goes
// on to wait for the developer's approval when the run finished with a spec and
// the web, or for the developer, "needs you", otherwise (see specDestination).
// After an implement run and a review run the card goes where laterDestination
// says.
var (
	waiting  = store.State{Stage: store.Spec, WaitsFor: store.Run}
	finished = store.State{Stage: store.Spec, WaitsFor: store.Approval}
	failed   = store.State{Stage: store.Spec, WaitsFor: store.Developer}

	implementNeedsYou = store.State{Stage: store.Implement, WaitsFor: store.Developer}
	reviewWaits       = store.State{Stage: store.Review, WaitsFor: store.Run}
	reviewNeedsYou    = store.State{Stage: store.Review, WaitsFor: store.Developer}
	reviewPassed      = store.State{Stage: store.Review, WaitsFor: store.Approval}
)

// Lock takes the lock on a data directory, creating the directory, and holds
// it for the life of the process, or until the file is closed. A second
// holder is refused. The kernel drops the lock with the process.
func Lock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Opened with close-on-exec, as Go opens every file, so that no child,
	// such as docker logs, holds the lock after the process has ended.
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another core holds the lock on %s", dir)
		}
		return nil, fmt.Errorf("the lock on %s: %w", dir, err)
	}
	return f, nil
}

// openStore takes the lock on a data directory and opens its store.
func openStore(dir string) (*store.Store, *os.File, error) {
	lock, err := Lock(dir)
	if err != nil {
		return nil, nil, err
	}
	s, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		lock.Close()
		return nil, nil, err
	}
	return s, lock, nil
}

// AddCard is `kiln card`: under the lock, it writes a card with the text and
// makes it wait in spec for a run, and returns the card's ID. It stands in for
// the API, which is the only way to change the store (ADR 0044), and never
// writes beside a running core.
func AddCard(dir, text string) (int64, error) {
	s, lock, err := openStore(dir)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	defer s.Close()
	c, err := s.Create(text)
	if err != nil {
		return 0, err
	}
	if _, err := s.Move(c.ID, waiting, store.CauseDeveloper); err != nil {
		return 0, err
	}
	return c.ID, nil
}

// Core is `kiln core`: under the lock, it opens the store, serves the screen
// on cfg.Listen, if it is set, and ticks every interval until the context is
// cancelled, with a follower beside the tick for every run in flight. A step
// that fails prints its error, changes nothing, and is tried again on the
// next tick. The screen's handlers and the followers share the store with the
// tick (see store.Store). Once the context is cancelled, Core returns when
// every follower has returned; the runs go on in their containers.
func Core(ctx context.Context, cfg Config, out, errOut io.Writer) error {
	return runCore(ctx, cfg, out, errOut, sealed{}, interval)
}

// runCore is Core on an engine, with a tick every interval.
func runCore(ctx context.Context, cfg Config, out, errOut io.Writer, eng engine, every time.Duration) error {
	// Nothing the core does depends on the directory it was started in, which
	// may be removed while it runs: its paths are made absolute while that
	// directory is there, and every command it runs, through the seal, runs in
	// the data directory, and git in the instance's copy of the repo (card 16).
	for _, p := range []*string{&cfg.Data, &cfg.Repo, &cfg.Page} {
		if *p == "" {
			continue
		}
		abs, err := filepath.Abs(*p)
		if err != nil {
			return err
		}
		*p = abs
	}
	ctx = seal.InDir(ctx, cfg.Data)
	if cfg.Listen != "" {
		if err := web.CheckAddress(cfg.Listen); err != nil {
			return err
		}
	}
	// A page that cannot be served is refused before anything is made.
	if cfg.Page != "" {
		if cfg.Listen == "" {
			return errors.New("--page needs a screen, and --listen is empty")
		}
		if _, _, err := web.ReadPage(cfg.Page); err != nil {
			return err
		}
	}
	if cfg.Runs < 1 || cfg.Runs > MaxRuns {
		return fmt.Errorf("--runs is %d; it is from 1 to %d", cfg.Runs, MaxRuns)
	}
	s, lock, err := openStore(cfg.Data)
	if err != nil {
		return err
	}
	defer lock.Close()
	defer s.Close()
	instance, err := s.Instance()
	if err != nil {
		return err
	}
	// The tick and the followers print one line at a time, so that the lines
	// of two runs do not mix.
	var mu sync.Mutex
	out, errOut = lockedWriter{&mu, out}, lockedWriter{&mu, errOut}
	c := &core{Config: cfg, store: s, instance: instance, out: out, errOut: errOut, activity: web.NewActivity(),
		engine: eng, followed: map[int64]string{}, refused: map[int64]bool{}}
	fmt.Fprintf(out, "kiln core: instance %s, data %s, up to %d runs at once\n", instance, cfg.Data, cfg.Runs)
	if cfg.Listen != "" {
		stop, err := c.serve()
		if err != nil {
			return err
		}
		defer stop()
	}
	// The followers end with the context, and the store is closed after them.
	defer c.followers.Wait()
	for {
		if err := c.tick(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintln(errOut, "kiln core:", run.Printable(err.Error()))
		}
		at("ticked")
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
}

// lockedWriter is a writer shared by goroutines, one write at a time.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// core is one running core.
type core struct {
	Config
	store       *store.Store
	instance    string
	out, errOut io.Writer
	activity    *web.Activity // of the runs the followers follow, for the screen
	changes     changes       // the last change the screen asked for, per card
	engine      engine

	// followed is the run of each follower, by its card, from before the
	// follower starts until it has returned. The tick gives a card one
	// follower at a time, and the sweep leaves a followed run alone, which
	// may not be in the store yet. It is only a guard: a core started again
	// has none, and the store and the engine say what is in flight.
	mu        sync.Mutex
	followed  map[int64]string
	followers sync.WaitGroup
	// refused are the cards whose last follower failed before its run was in
	// the store, such as one whose branch Carry refuses; the tick takes them
	// after the other cards. Like followed, it is only kept in memory.
	refused map[int64]bool
}

// b2i is 1 for true and 0 for false.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// engine is what the tick and its followers ask of the container engine, all
// of it through the seal and internal/run. Only the test gives a core another.
type engine interface {
	ListRuns(ctx context.Context, instance string) ([]string, error)
	RemoveRun(ctx context.Context, id, proxy string) error
	InspectRun(ctx context.Context, id string) (*seal.RunState, error)
	Carry(ctx context.Context, in run.Inputs, starting func() error, notes io.Writer) (run.Verdict, error)
	Remove(ctx context.Context, in run.Inputs) error
	ReadDocument(runs, id, kind string) (run.Document, error)
}

// sealed is the engine of a core: the seal and internal/run.
type sealed struct{}

func (sealed) ListRuns(ctx context.Context, instance string) ([]string, error) {
	return seal.ListRuns(ctx, instance)
}

func (sealed) RemoveRun(ctx context.Context, id, proxy string) error {
	return seal.RemoveRun(ctx, id, proxy)
}

func (sealed) InspectRun(ctx context.Context, id string) (*seal.RunState, error) {
	return seal.InspectRun(ctx, id)
}

func (sealed) Carry(ctx context.Context, in run.Inputs, starting func() error, notes io.Writer) (run.Verdict, error) {
	return run.Carry(ctx, in, starting, notes)
}

func (sealed) Remove(ctx context.Context, in run.Inputs) error { return run.Remove(ctx, in) }

func (sealed) ReadDocument(runs, id, kind string) (run.Document, error) {
	return run.ReadDocument(runs, id, kind)
}

// changes keeps, per card, the last change the screen was given, by the
// commits it was made from, so that a card's page that asks every 2 seconds
// does not make the same diff again each time.
type changes struct {
	mu   sync.Mutex
	last map[int64]cachedChange
}

type cachedChange struct {
	base, tip string
	change    web.Change
}

// serve serves the screen on the core's Listen address, and prints where. The
// function it returns stops serving, waiting a moment for the requests that
// are under way.
func (c *core) serve() (func(), error) {
	ln, err := web.Listen(c.Listen)
	if err != nil {
		return nil, fmt.Errorf("the screen: %w", err)
	}
	info := web.Info{Instance: c.instance, Repo: c.Repo, Model: c.model(store.Spec), ImplementModel: c.model(store.Implement),
		ReviewModel: c.model(store.Review), Runs: c.Runs, Change: c.change, Page: c.Page}
	h, err := web.Handler(c.store, info, c.activity, ln.Addr().String())
	if err != nil {
		ln.Close()
		return nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(c.errOut, "kiln core: the screen: ", 0)}
	go srv.Serve(ln)
	fmt.Fprintf(c.out, "kiln core: the screen is at http://%s/\n", ln.Addr())
	if c.Page != "" {
		fmt.Fprintf(c.out, "kiln core: the page is read from %s on every request\n", c.Page)
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if srv.Shutdown(ctx) != nil {
			srv.Close()
		}
	}, nil
}

// tick is one tick: the sweep, then each card that waits for a run, in the
// order of store.Waiting, except that a card whose last start failed before
// its run was in the store comes after the others: its follower takes a place
// until it fails, and would otherwise keep the cards behind it from starting
// on every tick. A card whose run is followed is left alone. A card
// with a run in flight in the store gets a follower, also when more than
// Runs are in flight, as after a restart; a run is started only while fewer
// than Runs are in flight. An error of one card is printed and the next card
// is taken; an error of the sweep, or an interrupt, ends the tick.
func (c *core) tick(ctx context.Context) error {
	if err := c.sweep(ctx); err != nil {
		return fmt.Errorf("the sweep: %w", err)
	}
	cards, err := c.store.Waiting()
	if err != nil {
		return err
	}
	inFlight, err := c.store.InFlightRuns()
	if err != nil {
		return err
	}
	c.mu.Lock()
	refused := maps.Clone(c.refused)
	c.mu.Unlock()
	slices.SortStableFunc(cards, func(a, b int64) int { return cmp.Compare(b2i(refused[a]), b2i(refused[b])) })
	// The cards whose runs count against the limit: those with a run in
	// flight in the store, and those with a follower, whose run may not be in
	// the store yet. A card counted once has its place: its run may have ended
	// since, and its next run takes the place.
	busy := map[int64]bool{}
	for card := range inFlight {
		busy[card] = true
	}
	c.mu.Lock()
	for card := range c.followed {
		busy[card] = true
	}
	c.mu.Unlock()
	for _, id := range cards {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if c.follows(id) {
			continue
		}
		f, err := c.card(ctx, id, busy[id] || len(busy) < c.Runs)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// An error can carry text of the run's, such as file names in
			// the save's output.
			fmt.Fprintf(c.errOut, "kiln core: card %d: %s\n", id, run.Printable(err.Error()))
			continue
		}
		if f != nil {
			busy[id] = true
			c.follow(ctx, f)
		}
	}
	return nil
}

// follows reports whether a card's run is followed.
func (c *core) follows(card int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.followed[card]
	return ok
}

// followsRun reports whether a run is followed.
func (c *core) followsRun(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.followed {
		if r == id {
			return true
		}
	}
	return false
}

// sweep removes every run of this instance on the engine whose run is not in
// flight in the store: one the store does not know, which never started, and
// one that has ended there (ADR 0035, rule 4). A run that a follower carries
// is left alone: it is placed before it is in the store, and removed by its
// follower once it has ended.
func (c *core) sweep(ctx context.Context) error {
	ids, err := c.engine.ListRuns(ctx, c.instance)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if c.followsRun(id) {
			continue
		}
		r, err := c.store.Run(id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case r.Ending == "":
			continue // in flight
		}
		if err := c.engine.RemoveRun(ctx, id, c.Proxy); err != nil {
			return err
		}
	}
	return nil
}

// action is what the tick does for a card that waits for a run.
type action int

const (
	start action = iota // start a run, with AddRun right before its start
	carry               // carry the run in flight on from where it is
	gone                // end the run in flight as failed: nothing of it is left
	leave               // leave the run alone, and say so on every tick
)

// decide is the table of what the tick does for a card that waits for a run,
// by whether a run is in flight in the store, the state of its container on
// the engine, "" for none, and whether its result.json is recorded.
func decide(inFlight bool, container string, recorded bool) action {
	switch {
	case !inFlight:
		return start
	case recorded:
		return carry
	}
	switch container {
	case "":
		return gone
	case "created", "running", "exited":
		return carry
	default: // paused, restarting, dead, removing
		return leave
	}
}

// follower is what a follower carries a card's run on with, as the tick
// decided it.
type follower struct {
	card     store.Card
	stage    stage
	row      store.RunRow
	in       run.Inputs
	starting func() error // nil for a run in the store
	added    bool         // set by starting once the run is in the store
	mark     func(point string)
}

// card decides, by the table, what is done for one card that waits for a run,
// and returns the follower that does it, or nil when there is nothing to
// follow: the card waits for a run no longer, it is held, or it has no run in
// flight and mayStart is false, since the limit is reached.
func (c *core) card(ctx context.Context, id int64, mayStart bool) (*follower, error) {
	card, err := c.store.Card(id)
	if err != nil {
		return nil, err
	}
	if card.WaitsFor != store.Run {
		return nil, nil // its run ended since the tick read the cards
	}
	st, ok := stages[card.Stage]
	if !ok {
		return nil, fmt.Errorf("it waits for a run in %s, which has none", card.Stage)
	}
	// The points of the run, by their names and by the stage's.
	mark := func(point string) {
		at(point)
		at(string(card.Stage) + "/" + point)
	}
	row, err := c.store.InFlight(id)
	if err != nil {
		return nil, err
	}
	inFlight := row != nil
	if !inFlight {
		if !mayStart {
			return nil, nil
		}
		// The same ID on every tick until the run is in the store.
		n, err := c.store.RunCount(id)
		if err != nil {
			return nil, err
		}
		commit, held, err := c.startsFrom(ctx, card)
		if err != nil {
			return nil, err
		}
		if held != "" {
			return nil, c.hold(card, held)
		}
		row = &store.RunRow{ID: fmt.Sprintf("%s-%d-%d", c.instance, id, n+1), Card: id,
			Commit: commit, Model: c.model(card.Stage), Effort: st.effort}
	}
	in, held, err := c.inputs(ctx, *row, card, inFlight)
	if err != nil {
		return nil, err
	}
	if held != "" {
		return nil, c.hold(card, held)
	}
	if !inFlight {
		mark("after-instruction")
	}

	state, err := c.engine.InspectRun(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	container := ""
	if state != nil {
		container = state.Status
	}
	recorded, err := exists(filepath.Join(in.Runs, row.ID, "result.json"))
	if err != nil {
		return nil, err
	}

	f := &follower{card: card, stage: st, row: *row, in: in, mark: mark}
	switch decide(inFlight, container, recorded) {
	case start:
		f.starting = func() error {
			mark("before-add")
			if err := c.store.AddRun(f.row, c.Runs); err != nil {
				return err
			}
			f.added = true
			mark("after-add")
			fmt.Fprintf(c.out, "card %d: %s run %s starts\n", id, card.Stage, row.ID)
			return nil
		}
	case carry:
		fmt.Fprintf(c.out, "card %d: %s run %s goes on\n", id, card.Stage, row.ID)
	case gone:
		// Carry, with starting nil, refuses to start it a second time.
	case leave:
		return nil, fmt.Errorf("the container kiln-run-%s is %s; it is left alone", row.ID, container)
	}
	return f, nil
}

// follow starts a follower beside the tick, which carries the run to its end
// and records it. The card is followed from now until the follower has
// returned. An error of the follower is printed with the card's ID, and the
// next tick decides again; an interrupt ends the following, and the run goes
// on.
func (c *core) follow(ctx context.Context, f *follower) {
	id := f.card.ID
	c.mu.Lock()
	c.followed[id] = f.row.ID
	c.mu.Unlock()
	c.followers.Add(1)
	go func() {
		defer c.followers.Done()
		err := c.carry(ctx, f)
		if ctx.Err() == nil {
			if err != nil {
				fmt.Fprintf(c.errOut, "kiln core: card %d: %s\n", id, run.Printable(err.Error()))
			}
			at("followed")
		}
		c.mu.Lock()
		delete(c.followed, id)
		if err != nil && f.starting != nil && !f.added {
			c.refused[id] = true
		} else {
			delete(c.refused, id)
		}
		c.mu.Unlock()
	}()
}

// carry carries a card's run to its end: it follows the run, records its
// ending with the card's new state and its document in one transaction, and
// removes what is left of the run.
func (c *core) carry(ctx context.Context, f *follower) error {
	id, row, in := f.card.ID, f.row, f.in
	// Following gives the run's output from its first event, so its activity
	// starts again with every Carry.
	c.activity.Drop(row.ID)
	in.Activity = func(a run.Activity) { c.activity.Add(row.ID, a) }
	verdict, err := c.engine.Carry(ctx, in, f.starting, c.errOut)
	switch {
	case errors.Is(err, run.ErrGone):
		verdict = run.Verdict{Reason: run.ErrGone.Error()}
	case err != nil:
		return err
	}
	var doc run.Document
	var readErr error
	if verdict.Finished {
		doc, readErr = c.engine.ReadDocument(in.Runs, row.ID, f.stage.kind)
	}
	d, err := destination(f.card.Stage, verdict, doc, readErr)
	if err != nil {
		return fmt.Errorf("the %s of run %s: %w; it is read again on the next tick", f.stage.kind, row.ID, err)
	}

	f.mark("before-end")
	if _, err := c.store.EndRun(row.ID, d.ending, d.reason, d.to, d.doc); err != nil {
		return err
	}
	f.mark("after-end")
	c.activity.Drop(row.ID)
	fmt.Fprintf(c.out, "card %d: run %s %s: %s\n", id, row.ID, d.ending, d.reason)
	return c.engine.Remove(ctx, in)
}

// hold stops a card that waits for a run, before the run is in the store: the
// card needs the developer, with the reason.
func (c *core) hold(card store.Card, reason string) error {
	if _, err := c.store.MoveWithReason(card.ID, card.State, store.State{Stage: card.Stage, WaitsFor: store.Developer},
		store.CauseRule, reason); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "card %d: no %s run is started: %s\n", card.ID, card.Stage, reason)
	return nil
}

// startsFrom is the commit a card's next run starts from: the head of main for
// a spec run; the tip of the card's branch for a review run, and for an
// implement run when the branch exists, so that a second implement run builds
// on the first (ADR 0013); and the head of main otherwise. A review run of a
// card without a branch is held, with the reason.
func (c *core) startsFrom(ctx context.Context, card store.Card) (commit, held string, err error) {
	if card.Stage != store.Spec {
		tip, err := branchTip(ctx, c.Repo, branchOf(card.ID))
		if err != nil || tip != "" {
			return tip, "", err
		}
		if card.Stage == store.Review {
			return "", "the card's branch " + branchOf(card.ID) + " does not exist; no change to review", nil
		}
	}
	commit, err = head(ctx, c.Repo)
	return commit, "", err
}

// branchOf is the branch of a card's work.
func branchOf(card int64) string { return fmt.Sprintf("kiln/card-%d", card) }

// end is how a run's end moves its card: the run's ending, the card's new
// state, the reason on its entry, and the document that is kept, if any.
type end struct {
	ending store.Ending
	to     store.State
	reason string
	doc    *store.Doc
}

// destination is where a card goes at its run's end, from the stage, the run's
// verdict and, for a finished run, what ReadDocument gave: the spec stage's
// table, or the table of implement and review. A file that could not be read
// from the disk changes nothing: the error is returned, and the run stays in
// flight.
func destination(s store.Stage, v run.Verdict, doc run.Document, readErr error) (end, error) {
	if v.Finished && readErr != nil {
		return end{}, readErr
	}
	if s == store.Spec {
		return specDestination(v, doc), nil
	}
	return laterDestination(s, v, doc), nil
}

// specDestination is the table of where a card goes at its spec run's end. A
// run that finished without a spec it could read, or without the web, is still
// finished (ADR 0043): it produced its stage's result, and its card stops under
// "needs you".
func specDestination(v run.Verdict, doc run.Document) end {
	switch {
	case !v.Finished:
		return end{store.Failed, failed, v.Reason, nil}
	case doc.Problem != "":
		return end{store.Finished, failed, doc.Problem + "; " + v.Reason, nil}
	}
	spec := &store.Doc{Kind: store.KindSpec, Text: string(doc.Text)}
	switch {
	case len(v.Hosts) == 0:
		return end{store.Finished, failed, "the spec was written without the web: no list of hosts; " + v.Reason, spec}
	case v.WayOutGone:
		return end{store.Finished, failed, "the spec was written without the web: its way out was gone; " + v.Reason, spec}
	}
	return end{store.Finished, finished, v.Reason, spec}
}

// The first lines of a review that has a verdict (instructions/review.md).
const (
	passes   = "Verdict: passes"
	goesBack = "Verdict: goes back"
)

// verdictLine is the first line of a review, without the white space at its
// end, a carriage return included.
func verdictLine(review string) string {
	line, _, _ := strings.Cut(review, "\n")
	return strings.TrimRight(line, " \t\r")
}

// laterDestination is the table of where a card goes at the end of its
// implement run or its review run.
//
//	implement  failed                        implement, needs you, the run's reason
//	implement  finished, nothing changed     implement, needs you: the run changed nothing
//	implement  finished, a commit saved      review, waits for a run
//	review     failed, or a problem of       review, needs you, the reason
//	           review.md
//	review     finished, Verdict: goes back  review, needs you: the review sends the change back
//	review     finished, Verdict: passes     review, waits for approval
//	review     finished, another first line  review, needs you: the review has no verdict
//
// A finished implement run's report is kept whenever it can be read; a missing
// report is no problem, and another problem of it is said in the reason but
// does not stop the card. A review that can be read is kept.
func laterDestination(s store.Stage, v run.Verdict, doc run.Document) end {
	if s == store.Implement {
		if !v.Finished {
			return end{store.Failed, implementNeedsYou, v.Reason, nil}
		}
		reason := v.Reason
		var report *store.Doc
		switch {
		case doc.Problem == "":
			report = &store.Doc{Kind: store.KindReport, Text: string(doc.Text)}
		case !doc.Missing:
			reason = doc.Problem + "; " + reason
		}
		if !v.Changed() {
			return end{store.Finished, implementNeedsYou, "the run changed nothing; " + reason, report}
		}
		return end{store.Finished, reviewWaits, reason, report}
	}
	switch {
	case !v.Finished:
		return end{store.Failed, reviewNeedsYou, v.Reason, nil}
	case doc.Problem != "":
		return end{store.Finished, reviewNeedsYou, doc.Problem + "; " + v.Reason, nil}
	}
	review := &store.Doc{Kind: store.KindReview, Text: string(doc.Text)}
	switch verdictLine(review.Text) {
	case goesBack:
		return end{store.Finished, reviewNeedsYou, "the review sends the change back; " + v.Reason, review}
	case passes:
		return end{store.Finished, reviewPassed, v.Reason, review}
	}
	return end{store.Finished, reviewNeedsYou, "the review has no verdict; " + v.Reason, review}
}

// inputs are the inputs of a card's run, from its row. A run that starts is
// told the installed Kiln's instruction for its stage, then what told says,
// written to its file before AddRun. A run in flight keeps the file it was
// started with, which is written again only when it is missing: a core rebuilt
// with another instruction would otherwise write another text, which the run's
// label kiln.instruction would refuse for ever. A review run whose change is
// too large is not started: held says why.
func (c *core) inputs(ctx context.Context, row store.RunRow, card store.Card, inFlight bool) (in run.Inputs, held string, err error) {
	st := stages[card.Stage]
	instruction := filepath.Join(c.Data, "instructions", row.ID+".md")
	write := !inFlight
	if inFlight {
		there, err := exists(instruction)
		if err != nil {
			return run.Inputs{}, "", err
		}
		write = !there
	}
	if write {
		text, held, err := c.told(ctx, row, card)
		if err != nil {
			return run.Inputs{}, "", err
		}
		if held != "" {
			if inFlight {
				return run.Inputs{}, "", fmt.Errorf("the instruction of run %s cannot be written again: %s", row.ID, held)
			}
			return run.Inputs{}, held, nil
		}
		if err := writeAtomically(instruction, []byte(st.instruction+text)); err != nil {
			return run.Inputs{}, "", err
		}
	}
	in = run.Inputs{
		Repo:        c.Repo,
		Commit:      row.Commit,
		Branch:      branchOf(row.Card),
		ID:          row.ID,
		Instruction: instruction,
		Runs:        filepath.Join(c.Data, "runs"),
		Image:       c.Image,
		Proxy:       c.Proxy,
		Model:       row.Model,
		Effort:      row.Effort,
		Instance:    c.instance,
		SkipSave:    !st.save,
	}
	if card.Stage == store.Spec {
		in.Hosts, in.WayOutImage, in.WayOutNetwork = c.Hosts, c.WayOutImage, c.WayOutNetwork
	}
	// An implement run's save is named after the approved spec's title, read
	// again whenever the inputs are built, also for a run in flight: the
	// approved spec does not change while the card's one run is in flight, so
	// a repeat gets the same title.
	if card.Stage == store.Implement {
		spec, err := c.store.Spec(card.ID)
		if err != nil {
			return run.Inputs{}, "", err
		}
		in.Title = run.TitleOf(spec.Text)
	}
	return in, "", nil
}

// noteHeading and noteIntro begin the part of the developer's note.
const (
	noteHeading = "## The developer's note"
	noteIntro   = "The developer wrote this when sending the card back. Follow it. Where it differs from the spec, the note wins."
)

// told is what a run is told after its stage's instruction, each part under its
// heading: the card; for a spec run that follows the developer's note, or a
// send-back to spec, the last spec, which it revises, and after a send-back to
// spec the newest report or review, which says why; for an implement run and a review run, the
// approved spec; for an implement run, the last review if it sent the change
// back, or if it passed and the developer sent the change back; for a review
// run, the change as a diff from where the card's branch left main to the
// run's commit; and last, the developer's note, if the entry that made the
// card wait for this run has one (card 16). A change over maxDiff is held.
//
// A part that a run wrote, a spec, a report or a review, is told fenced as
// markdown, as the change is fenced as a diff: nothing in it can then read as
// a heading or open a block that runs past it, so only the tick's own
// headings are headings. The card and the developer's note are the
// developer's text and are told as they are.
func (c *core) told(ctx context.Context, row store.RunRow, card store.Card) (text, held string, err error) {
	note, err := c.store.Note(card.ID)
	if err != nil {
		return "", "", err
	}
	parts := []string{"## The card", card.Text}
	if card.Stage == store.Spec {
		// A report or a review newer than every spec is the newest document
		// of a card in spec only when the developer sent it back to spec.
		newest, err := c.store.Newest(card.ID, store.KindSpec, store.KindReport, store.KindReview)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", "", err
		}
		toSpec := err == nil && newest.Kind != store.KindSpec
		if note != "" || toSpec {
			last, err := c.store.Spec(card.ID)
			switch {
			case err == nil:
				parts = append(parts, "## The last spec", fenced(last.Text, "markdown"))
			case !errors.Is(err, sql.ErrNoRows):
				return "", "", err
			}
		}
		if toSpec {
			parts = append(parts, "## The last "+newest.Kind, fenced(newest.Text, "markdown"))
		}
		return told(withNote(parts, note)), "", nil
	}
	spec, err := c.store.Spec(card.ID)
	if err != nil {
		return "", "", err
	}
	parts = append(parts, "## The approved spec", fenced(spec.Text, "markdown"))
	if card.Stage == store.Implement {
		last, err := c.store.Newest(card.ID, store.KindSpec, store.KindReview)
		if err != nil {
			return "", "", err
		}
		// A review that passed is the newest document of a card in implement
		// only when the developer sent the change back from it.
		if v := verdictLine(last.Text); last.Kind == store.KindReview && (v == goesBack || v == passes) {
			parts = append(parts, "## The last review", fenced(last.Text, "markdown"))
		}
		return told(withNote(parts, note)), "", nil
	}
	diff, size, err := changeOf(ctx, c.Repo, row.Commit)
	if err != nil {
		return "", "", err
	}
	if size > maxDiff {
		return "", "the change is " + mebibytes(size) + " as a diff, more than 1 MiB; no review run is started", nil
	}
	return told(withNote(append(parts, "## The change", fenced(diff, "diff")), note)), "", nil
}

// withNote is the parts with the developer's note after them, if there is one.
func withNote(parts []string, note string) []string {
	if note == "" {
		return parts
	}
	return append(parts, noteHeading, noteIntro+"\n\n"+note)
}

// mebibytes is a size over 1 MiB as a person reads it: in MiB with one
// decimal, or with two, rounded up, where one would read as the limit.
func mebibytes(n int64) string {
	const mib = 1 << 20
	if tenths := (n*10 + mib/2) / mib; tenths > 10 {
		return fmt.Sprintf("%d.%d MiB", tenths/10, tenths%10)
	}
	hundredths := (n*100 + mib - 1) / mib
	return fmt.Sprintf("%d.%02d MiB", hundredths/100, hundredths%100)
}

// told joins headings and their texts: a heading, an empty line and its text,
// and an empty line before the next heading.
func told(parts []string) string {
	var b strings.Builder
	for i := 0; i < len(parts); i += 2 {
		if i > 0 {
			switch s := b.String(); {
			case strings.HasSuffix(s, "\n\n"):
			case strings.HasSuffix(s, "\n"):
				b.WriteString("\n")
			default:
				b.WriteString("\n\n")
			}
		}
		b.WriteString(parts[i] + "\n\n" + parts[i+1])
	}
	return b.String()
}

// fenced puts a text in a code block with the info string info: a run's
// document as markdown, or the change as a diff. The fence is backticks, longer
// than any run of backticks in the text, so that nothing in the text ends the
// block, and a heading or a fence inside it stays part of the text.
func fenced(text, info string) string {
	longest, n := 0, 0
	for _, r := range text {
		if r == '`' {
			n++
			longest = max(longest, n)
		} else {
			n = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return fence + info + "\n" + text + fence + "\n"
}

// writeAtomically writes a file by a temporary file in its directory and a
// rename, so that a reader sees the old file or the new one, never a part.
func writeAtomically(name string, data []byte) error {
	dir := filepath.Dir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err == nil {
		err = os.Rename(f.Name(), name)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// exists reports whether a file exists.
func exists(name string) (bool, error) {
	_, err := os.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// gitCommand is git on a bare repo, run in the repo's directory and never in
// the directory the core was started in, which may have been removed since.
// Variables of the caller's that start with GIT_ are left out, so that none of
// them points git at another repo.
func gitCommand(ctx context.Context, repo string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + repo}, args...)...)
	cmd.Dir = repo
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	return cmd
}

// gitLine is the output of git on a bare repo, trimmed.
func gitLine(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := gitCommand(ctx, repo, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s on %s: %w: %s", args[0], repo, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// head is the commit that HEAD of a bare repo is at, the head of main.
func head(ctx context.Context, repo string) (string, error) {
	return gitLine(ctx, repo, "rev-parse", "--verify", "HEAD^{commit}")
}

// branchTip is the commit a branch of a bare repo is at, or "" when there is
// no such branch.
func branchTip(ctx context.Context, repo, branch string) (string, error) {
	ref := "refs/heads/" + branch
	refs, err := gitLine(ctx, repo, "for-each-ref", "--format=%(refname)", ref)
	if err != nil {
		return "", err
	}
	if !slices.Contains(strings.Split(refs, "\n"), ref) {
		return "", nil
	}
	return gitLine(ctx, repo, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
}

// change is the change on a card's branch for the screen: where it is, its
// commits since it left main, and the diff that a review run is told
// (changeOf), or nil when the card has no branch.
func (c *core) change(ctx context.Context, card int64) (*web.Change, error) {
	branch := branchOf(card)
	tip, err := branchTip(ctx, c.Repo, branch)
	if err != nil || tip == "" {
		return nil, err
	}
	base, err := gitLine(ctx, c.Repo, "merge-base", "HEAD", tip)
	if err != nil {
		return nil, err
	}
	c.changes.mu.Lock()
	kept, ok := c.changes.last[card]
	c.changes.mu.Unlock()
	if ok && kept.base == base && kept.tip == tip {
		ch := kept.change
		return &ch, nil
	}
	commits, err := gitLine(ctx, c.Repo, "log", "--no-color", "--format=%h%x00%s", base+".."+tip)
	if err != nil {
		return nil, err
	}
	ch := web.Change{Branch: branch, Repo: c.Repo, Commits: []web.Commit{},
		Command: "git --git-dir=" + shellWord(c.Repo) + " diff HEAD..." + branch}
	for _, line := range strings.Split(commits, "\n") {
		if hash, title, ok := strings.Cut(line, "\x00"); ok {
			ch.Commits = append(ch.Commits, web.Commit{Hash: hash, Title: title})
		}
	}
	ch.Diff, ch.Size, err = changeOf(ctx, c.Repo, tip)
	if err != nil {
		return nil, err
	}
	ch.TooLarge = ch.Size > maxDiff
	c.changes.mu.Lock()
	if c.changes.last == nil {
		c.changes.last = map[int64]cachedChange{}
	}
	c.changes.last[card] = cachedChange{base, tip, ch}
	c.changes.mu.Unlock()
	return &ch, nil
}

// shellWord is a word as a shell reads it: as it is when it holds only
// characters a shell leaves alone, else in single quotes.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./+:@%,=") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// changeOf is the change of a card's branch at commit, as a diff from where the
// branch left main, and its size in bytes. A diff over maxDiff is not kept:
// only its size is given.
func changeOf(ctx context.Context, repo, commit string) (string, int64, error) {
	base, err := gitLine(ctx, repo, "merge-base", "HEAD", commit)
	if err != nil {
		return "", 0, err
	}
	cmd := gitCommand(ctx, repo, "diff", "--no-color", "--no-ext-diff", "--no-textconv", base, commit)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", 0, err
	}
	if err := cmd.Start(); err != nil {
		return "", 0, err
	}
	var kept bytes.Buffer
	n, copyErr := io.Copy(&kept, io.LimitReader(out, maxDiff+1))
	if copyErr == nil {
		var rest int64
		rest, copyErr = io.Copy(io.Discard, out)
		n += rest
	}
	if err := errors.Join(copyErr, cmd.Wait()); err != nil {
		return "", 0, fmt.Errorf("git diff on %s: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	if n > maxDiff {
		return "", n, nil
	}
	return kept.String(), n, nil
}
