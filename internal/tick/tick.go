// Package tick is the core's loop (ADR 0035): every tick it reconciles the
// store with the container engine, and gives a card that waits for a run its
// one run, carried to its end before the tick goes on.
//
// The data directory holds the store, store.db; the lock, lock; the runs'
// records, runs/<id>/, as internal/run writes them; and the cards' text as
// the runs' instructions, instructions/<id>.md.
//
// Only internal/seal drives the container engine; this package calls it and
// internal/run.
package tick

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/seal"
	"github.com/TheEisbaer/kiln/internal/store"
)

// Config is what `kiln core` is started with.
type Config struct {
	Data   string // the data directory
	Repo   string // the instance's copy of the target repo, a bare repo
	Model  string // the model of a run that starts, an exact identifier
	Effort string // the effort of a run that starts
	Image  string // the run image
	Proxy  string // the proxy container's name
	// The list of hosts of a spec run (ADR 0059), which until the stage cards
	// is every run the tick starts, and its way out's image and outward
	// network. An empty list seals a run as before.
	Hosts         []string
	WayOutImage   string
	WayOutNetwork string
}

// interval is the time between two ticks. Only the test changes it.
var interval = 2 * time.Second

// atPoint, set only by the test, is called at the named points of a tick. It
// can exit the process, as a kill there would.
var atPoint func(point string)

func at(point string) {
	if atPoint != nil {
		atPoint(point)
	}
}

// The states a run moves its card to, until the stage cards: a card waits in
// spec for a run, and goes on to wait for the developer's approval when the
// run finished, or for the developer, "needs you", when it failed.
var (
	waiting  = store.State{Stage: store.Spec, WaitsFor: store.Run}
	finished = store.State{Stage: store.Spec, WaitsFor: store.Approval}
	failed   = store.State{Stage: store.Spec, WaitsFor: store.Developer}
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

// Core is `kiln core`: under the lock, it opens the store and ticks every
// interval until the context is cancelled. A step that fails prints its
// error, changes nothing, and is tried again on the next tick.
func Core(ctx context.Context, cfg Config, out, errOut io.Writer) error {
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
	c := &core{Config: cfg, store: s, instance: instance, out: out, errOut: errOut}
	fmt.Fprintf(out, "kiln core: instance %s, data %s\n", instance, cfg.Data)
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
		case <-time.After(interval):
		}
	}
}

// core is one running core.
type core struct {
	Config
	store       *store.Store
	instance    string
	out, errOut io.Writer
}

// tick is one tick: the sweep, then each card that waits for a run in turn.
// An error of one card is printed and the next card is taken; an error of
// the sweep, or an interrupt, ends the tick.
func (c *core) tick(ctx context.Context) error {
	if err := c.sweep(ctx); err != nil {
		return fmt.Errorf("the sweep: %w", err)
	}
	cards, err := c.store.Waiting()
	if err != nil {
		return err
	}
	for _, card := range cards {
		err := c.card(ctx, card)
		if errors.Is(err, run.ErrInterrupted) || ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// An error can carry text of the run's, such as file names in
			// the save's output.
			fmt.Fprintf(c.errOut, "kiln core: card %d: %s\n", card, run.Printable(err.Error()))
		}
	}
	return nil
}

// sweep removes every run of this instance on the engine whose run is not in
// flight in the store: one the store does not know, which never started, and
// one that has ended there (ADR 0035, rule 4).
func (c *core) sweep(ctx context.Context) error {
	ids, err := seal.ListRuns(ctx, c.instance)
	if err != nil {
		return err
	}
	for _, id := range ids {
		r, err := c.store.Run(id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case r.Ending == "":
			continue // in flight
		}
		if err := seal.RemoveRun(ctx, id, c.Proxy); err != nil {
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

// card acts for one card that waits for a run, by the table, and carries its
// run to the end.
func (c *core) card(ctx context.Context, id int64) error {
	card, err := c.store.Card(id)
	if err != nil {
		return err
	}
	row, err := c.store.InFlight(id)
	if err != nil {
		return err
	}
	inFlight := row != nil
	if !inFlight {
		// The same ID on every tick until the run is in the store.
		n, err := c.store.RunCount(id)
		if err != nil {
			return err
		}
		commit, err := head(ctx, c.Repo)
		if err != nil {
			return err
		}
		row = &store.RunRow{ID: fmt.Sprintf("%s-%d-%d", c.instance, id, n+1), Card: id,
			Commit: commit, Model: c.Model, Effort: c.Effort}
	}
	in, err := c.inputs(*row, card.Text)
	if err != nil {
		return err
	}

	state, err := seal.InspectRun(ctx, row.ID)
	if err != nil {
		return err
	}
	container := ""
	if state != nil {
		container = state.Status
	}
	recorded, err := exists(filepath.Join(in.Runs, row.ID, "result.json"))
	if err != nil {
		return err
	}

	var starting func() error
	switch decide(inFlight, container, recorded) {
	case start:
		starting = func() error {
			at("before-add")
			if err := c.store.AddRun(*row); err != nil {
				return err
			}
			at("after-add")
			fmt.Fprintf(c.out, "card %d: run %s starts\n", id, row.ID)
			return nil
		}
	case carry:
		fmt.Fprintf(c.out, "card %d: run %s goes on\n", id, row.ID)
	case gone:
		// Carry, with starting nil, refuses to start it a second time.
	case leave:
		return fmt.Errorf("the container kiln-run-%s is %s; it is left alone", row.ID, container)
	}

	ending, to := store.Failed, failed
	verdict, err := run.Carry(ctx, in, starting, c.errOut)
	switch {
	case errors.Is(err, run.ErrGone):
		verdict.Reason = run.ErrGone.Error()
	case err != nil:
		return err
	case verdict.Finished:
		ending, to = store.Finished, finished
	}

	at("before-end")
	if _, err := c.store.EndRun(row.ID, ending, verdict.Reason, to); err != nil {
		return err
	}
	at("after-end")
	fmt.Fprintf(c.out, "card %d: run %s %s: %s\n", id, row.ID, ending, verdict.Reason)
	return run.Remove(ctx, in)
}

// inputs are the inputs of a card's run, from its row, and with the card's
// text written as the run's instruction.
func (c *core) inputs(row store.RunRow, text string) (run.Inputs, error) {
	instruction := filepath.Join(c.Data, "instructions", row.ID+".md")
	if err := writeAtomically(instruction, []byte(text)); err != nil {
		return run.Inputs{}, err
	}
	return run.Inputs{
		Repo:          c.Repo,
		Commit:        row.Commit,
		Branch:        fmt.Sprintf("kiln/card-%d", row.Card),
		ID:            row.ID,
		Instruction:   instruction,
		Runs:          filepath.Join(c.Data, "runs"),
		Image:         c.Image,
		Proxy:         c.Proxy,
		Model:         row.Model,
		Effort:        row.Effort,
		Instance:      c.instance,
		Hosts:         c.Hosts,
		WayOutImage:   c.WayOutImage,
		WayOutNetwork: c.WayOutNetwork,
	}, nil
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

// head is the commit that HEAD of a bare repo is at. Variables of the
// caller's that start with GIT_ are left out, so that none of them points git
// at another repo.
func head(ctx context.Context, repo string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+repo, "rev-parse", "--verify", "HEAD^{commit}")
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("HEAD of %s: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
