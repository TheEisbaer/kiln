// Package run carries out the steps of one run (ADR 0035, ADR 0038): it checks
// the inputs and looks at what is there, creates and places the run through
// the seal, starts and follows it, collects what it wrote, saves its change to
// a branch of the instance's copy, records it, and removes it. Every step looks
// at what is there before it acts, so that the same command started again
// repeats the run from where it stopped, after a failure or a kill at any step.
//
// Only internal/seal drives the container engine; this package calls it.
package run

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/TheEisbaer/kiln/internal/seal"
)

// Inputs are what one run is started with.
type Inputs struct {
	Repo        string // the instance's copy of the target repo, a bare repo
	Commit      string // the commit the run starts from
	Branch      string // the branch its save goes to
	ID          string // the run's ID
	Instruction string // the file of the instruction
	Runs        string // the directory of the runs' records
	Image       string // the run image
	Proxy       string // the proxy container's name
	Model       string // an exact model identifier (ADR 0030)
	Effort      string // low, medium or high
	// The instance the run belongs to, as a label on its network and
	// container; none for a run started by hand. A container of another
	// instance, or of none when one is given, is refused and left alone.
	Instance string
}

// Exit statuses.
const (
	exitFailed      = 1
	exitInterrupted = 130 // Ctrl-C ended the following; the run goes on
)

// The labels of a run's container, besides the seal's kiln.run.
const (
	labelCommit      = "kiln.commit"
	labelBranch      = "kiln.branch"
	labelModel       = "kiln.model"
	labelEffort      = "kiln.effort"
	labelInstruction = "kiln.instruction" // the instruction's SHA-256
	labelInstance    = "kiln.instance"    // set by the seal from Inputs.Instance
)

// Where Claude Code writes in a run: the documents of a stage (ADR 0038) and
// its sessions.
const (
	documentsPath = "/kiln/out"
	sessionsPath  = "/home/kiln/.claude/projects"
)

// afterStep, set only by the test, is called after each step with the step's
// name. It can exit the process, as a kill there would, or return an error,
// which fails the step.
var afterStep func(step string) error

// SetAfterStep sets afterStep, for the test of a package that calls this
// one, such as internal/tick. Only a test calls it.
func SetAfterStep(f func(step string) error) { afterStep = f }

// checkFlags judges the inputs that need nothing but themselves: the run's ID,
// the model and the effort.
func checkFlags(in Inputs) error {
	if err := seal.CheckRunID(in.ID); err != nil {
		return err
	}
	if !strings.HasPrefix(in.Model, "claude-") || in.Model == "claude-" || strings.Contains(in.Model, "[") {
		return fmt.Errorf("the model %q is not an exact identifier that starts with claude- and has no [ (ADR 0030); aliases are refused", in.Model)
	}
	switch in.Effort {
	case "low", "medium", "high":
	default:
		return fmt.Errorf("the effort %q is not low, medium or high", in.Effort)
	}
	for what, v := range map[string]string{"repo": in.Repo, "commit": in.Commit, "branch": in.Branch,
		"instruction": in.Instruction, "runs": in.Runs, "image": in.Image, "proxy": in.Proxy} {
		if v == "" {
			return fmt.Errorf("no %s is given", what)
		}
	}
	return nil
}

// run is one run, with its inputs checked.
type run struct {
	Inputs
	ref         string // refs/heads/<branch>
	commit      string // the full ID
	tree        string // the commit's tree
	entries     []treeEntry
	instruction []byte
	sum         string // the instruction's SHA-256
	out, errOut io.Writer
}

// check judges every input and resolves the commit. It makes nothing.
func check(ctx context.Context, in Inputs) (*run, error) {
	if err := checkFlags(in); err != nil {
		return nil, err
	}
	r := &run{Inputs: in}
	if bare, err := gitLine(ctx, in.Repo, "rev-parse", "--is-bare-repository"); err != nil || bare != "true" {
		return nil, fmt.Errorf("%s is not a bare repo: %v", in.Repo, err)
	}
	// Outside any repo, where a name such as @{-1} has nothing to expand to.
	if name, err := gitLine(ctx, os.DevNull, "check-ref-format", "--branch", in.Branch); err != nil || name != in.Branch {
		return nil, fmt.Errorf("the branch %q is not a valid branch name: %v", in.Branch, err)
	}
	r.ref = "refs/heads/" + in.Branch
	// A symbolic ref would let the guard pass against, and update-ref move,
	// another branch that the run did not create.
	if symbolic, err := isSymbolic(ctx, in.Repo, r.ref); err != nil {
		return nil, err
	} else if symbolic {
		return nil, fmt.Errorf("the branch %s is a symbolic ref; it is left alone", in.Branch)
	}
	var err error
	if r.commit, err = gitLine(ctx, in.Repo, "rev-parse", "--verify", "--end-of-options", in.Commit+"^{commit}"); err != nil {
		return nil, fmt.Errorf("the commit %q: %w", in.Commit, err)
	}
	if r.tree, err = gitLine(ctx, in.Repo, "rev-parse", "--verify", r.commit+"^{tree}"); err != nil {
		return nil, err
	}
	if r.entries, err = listTree(ctx, in.Repo, r.commit); err != nil {
		return nil, err
	}
	if err := refuseSubmodules(r.entries); err != nil {
		return nil, err
	}
	if r.instruction, err = os.ReadFile(in.Instruction); err != nil {
		return nil, err
	}
	s := sha256.Sum256(r.instruction)
	r.sum = hex.EncodeToString(s[:])
	return r, nil
}

// labels are the labels of the run's container.
func (r *run) labels() map[string]string {
	return map[string]string{
		labelCommit:      r.commit,
		labelBranch:      r.Branch,
		labelModel:       r.Model,
		labelEffort:      r.Effort,
		labelInstruction: r.sum,
	}
}

// command is the run's command, after the image (ADR 0020). The instruction
// comes on standard input, and the effort is always given (ADR 0030).
func (r *run) command() []string {
	return []string{"sh", "-c", `exec claude "$@" < /kiln/instruction.md`, "sh",
		"-p", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions",
		"--model", r.Model, "--effort", r.Effort}
}

// after calls afterStep, when the test has set it.
func after(step string) error {
	if afterStep == nil {
		return nil
	}
	return afterStep(step)
}

// ErrGone is Carry's answer for a run that is in the store, with no
// container and no record: it is never started a second time.
var ErrGone = errors.New("its container is gone and nothing was recorded")

// ErrInterrupted is Carry's answer when its context was cancelled while it
// followed the run. The run goes on.
var ErrInterrupted = errors.New("following ended; the run goes on")

// Verdict is how a recorded run ended.
type Verdict struct {
	// Finished is true when the container exited with 0, the result is
	// success, the model was the one asked for, and the work is saved or
	// nothing changed.
	Finished bool
	// Reason is one line: the container's ending, the result's subtype,
	// turns and cost, and the saved commit or "no change".
	Reason string
}

// Do carries out one run, or the rest of one started before with the same
// inputs, and returns the command's exit status. It prints the events it
// follows to out, and errors to errOut. A cancelled context ends the following
// only, with the status 130, and the run goes on; in any other step it fails
// the step.
func Do(ctx context.Context, in Inputs, out, errOut io.Writer) int {
	code, err := do(ctx, in, out, errOut)
	if err != nil {
		// An error can carry text of the run's, such as file names in the
		// save's output.
		fmt.Fprintln(errOut, "kiln run:", printable(err.Error()))
		if code == 0 {
			code = exitFailed
		}
	}
	return code
}

func do(ctx context.Context, in Inputs, out, errOut io.Writer) (int, error) {
	rec, err := carry(ctx, in, out, errOut, func() error { return nil }, false)
	if errors.Is(err, ErrInterrupted) {
		fmt.Fprintf(out, "following ended; the run goes on in kiln-run-%s, and the same command started again follows it and saves its work\n", in.ID)
		return exitInterrupted, nil
	}
	if err != nil {
		return exitFailed, err
	}
	if err := Remove(ctx, in); err != nil {
		return exitFailed, err
	}
	return rec.status(), nil
}

// Carry is steps 0 to 7 of a run: it checks the inputs and looks at what is
// there, creates, places, starts and follows the run as far as that is still
// to do, collects it, saves its work and records it, and returns the verdict.
// It removes nothing that has started; step 8 is Remove.
//
// starting is called once, right before the container is started for the
// first time. When it fails, the container is removed and nothing has
// started. With starting nil, the run is known to have been started before:
// Carry then never creates a container where there is none and no record,
// and returns ErrGone. A cancelled context while it follows returns
// ErrInterrupted. It prints nothing: the run's output is in its record.
func Carry(ctx context.Context, in Inputs, starting func() error) (Verdict, error) {
	rec, err := carry(ctx, in, io.Discard, io.Discard, starting, true)
	if err != nil {
		return Verdict{}, err
	}
	return Verdict{Finished: rec.status() == 0, Reason: rec.reason()}, nil
}

// With stored, the run is known to be in the store once starting has
// returned, or from the outset when starting is nil; see create.
func carry(ctx context.Context, in Inputs, out, errOut io.Writer, starting func() error, stored bool) (*record, error) {
	// 0: check the inputs, and look at what is there.
	r, err := check(ctx, in)
	if err != nil {
		return nil, err
	}
	r.out, r.errOut = out, errOut

	rec, err := readRecord(r.Runs, r.ID)
	if err != nil {
		return nil, err
	}
	if rec != nil {
		if err := rec.Inputs.matches(r); err != nil {
			return nil, err
		}
		fmt.Fprintf(out, "the run %s is recorded already\n", r.ID)
		return rec, nil
	}

	state, err := seal.InspectRun(ctx, r.ID) // refuses a container without kiln.run=<id>
	if err != nil {
		return nil, err
	}
	if state != nil {
		if got := state.Labels[labelInstance]; got != r.Instance {
			return nil, fmt.Errorf("the container %s belongs to the instance %q, not %q; it is left alone", "kiln-run-"+r.ID, got, r.Instance)
		}
		want := r.labels()
		got := maps.Clone(state.Labels)
		maps.DeleteFunc(got, func(k, _ string) bool { _, ok := want[k]; return !ok })
		if !maps.Equal(got, want) {
			return nil, fmt.Errorf("the container %s was started with other inputs (%v, not %v); it is left alone", "kiln-run-"+r.ID, got, want)
		}
	}
	status := ""
	if state != nil {
		status = state.Status
	}
	if status == "" && starting == nil {
		return nil, ErrGone
	}

	at, err := branchAt(ctx, r.Repo, r.ref)
	if err != nil {
		return nil, err
	}
	switch {
	case at == "" || at == r.commit:
	case status == "exited":
		ours, err := isSaveOf(ctx, r.Repo, at, r.commit, r.ID)
		if err != nil {
			return nil, err
		}
		if !ours {
			return nil, fmt.Errorf("the branch %s is at %s, neither absent nor at the commit %s nor at this run's save", r.Branch, at, r.commit)
		}
	default:
		return nil, fmt.Errorf("the branch %s is at %s, neither absent nor at the commit %s", r.Branch, at, r.commit)
	}
	// What update-ref requires of the branch: absent, or at the commit.
	guard := r.commit
	if at == "" {
		guard = ""
	}

	switch status {
	case "", "created":
		if err := r.create(ctx, starting, stored, status); err != nil {
			return nil, err
		}
		fallthrough
	case "running":
		if status == "running" {
			// A proxy replaced since is on no earlier run's network.
			if err := seal.AttachProxy(ctx, r.ID, r.Proxy); err != nil {
				return nil, err
			}
		}
		if err := r.follow(ctx); err != nil {
			return nil, err
		}
	case "exited":
	default:
		return nil, fmt.Errorf("the container kiln-run-%s is %s; it is left alone", r.ID, status)
	}

	rec, err = r.collect(ctx)
	if err != nil {
		return nil, err
	}
	if rec.Saved, err = r.save(ctx, rec.Ending, guard); err != nil {
		return nil, err
	}
	if err := r.record(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// Remove is step 8: it removes the run's container and network, and whatever
// of them is left. It can be repeated.
func Remove(ctx context.Context, in Inputs) error {
	if err := seal.CheckRunID(in.ID); err != nil {
		return err
	}
	if err := seal.RemoveRun(ctx, in.ID, in.Proxy); err != nil {
		return err
	}
	return after("remove")
}

// undo removes the run's container and network after a failure before the
// start, on a context that cannot be cancelled, since the context may be what
// failed the step.
func (r *run) undo(ctx context.Context, err error) error {
	if rmErr := seal.RemoveRun(context.WithoutCancel(ctx), r.ID, r.Proxy); rmErr != nil {
		return fmt.Errorf("%w (and removing the run failed: %v)", err, rmErr)
	}
	return err
}

// create is steps 1 to 3: create the run, place its files, and start it.
// starting, when it is not nil, is called right before the start.
//
// With stored, the run is in the store from the moment starting has returned,
// or from the outset when starting is nil. From then on nothing here removes
// its container or network (ADR 0035, rule 1): a step that fails returns its
// error, and the next tick goes on from what is there. A run in the store whose
// container never started is not created again, which would remove the
// container first; its files are placed again into the container that is
// there, and it is started. Without stored, as for `kiln run` by hand, a
// container that never started is removed when a step fails, and one that is
// there is created again.
func (r *run) create(ctx context.Context, starting func() error, stored bool, status string) error {
	inStore := stored && starting == nil
	// undo removes what this call made, but never a run that is in the store.
	undo := func(err error) error {
		if inStore {
			return err
		}
		return r.undo(ctx, err)
	}

	// 1: CreateRun leaves nothing behind when it fails.
	if !(inStore && status == "created") {
		if err := seal.CreateRun(ctx, seal.Run{ID: r.ID, Image: r.Image, Proxy: r.Proxy,
			Command: r.command(), Labels: r.labels(), Instance: r.Instance}); err != nil {
			return err
		}
		if err := after("create"); err != nil {
			return undo(err)
		}
	}

	// 2: the same files of the same commit, whose labels step 0 checked, are
	// written again into a container that was placed into before.
	files, err := placingTar(ctx, r.Repo, r.entries)
	if err == nil {
		err = seal.PlaceRun(ctx, r.ID, bytes.NewReader(files), r.instruction)
	}
	if err == nil {
		err = after("place")
	}
	if err == nil && starting != nil {
		if err = starting(); err == nil {
			inStore = stored
		}
	}
	if err != nil {
		return undo(err)
	}

	// 3: a container that never started is removed, unless its run is in the
	// store; one that did stays.
	err = seal.StartRun(ctx, r.ID)
	if err == nil {
		err = after("start")
	}
	if err != nil {
		state, inspectErr := seal.InspectRun(context.WithoutCancel(ctx), r.ID)
		if inspectErr == nil && state != nil && state.Status == "created" {
			return undo(err)
		}
		return err
	}
	return nil
}

// follow is step 4: it prints the run's events until its container stops. If
// system/init names another model, it stops the run, which then goes through
// the other steps like any other. A cancelled context ends the following only,
// with ErrInterrupted.
func (r *run) follow(ctx context.Context) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		// The run's standard error is shown as it comes, without control
		// characters; it is recorded as it is in step 5.
		err := seal.ReadRun(ctx, r.ID, true, pw, &printableWriter{w: r.errOut})
		pw.CloseWithError(err)
		done <- err
	}()
	var stopErr error
	f := &follower{out: r.out, model: r.Model, mismatch: func(seen string) {
		fmt.Fprintf(r.out, "the run uses the model %s, not %s: it is stopped\n", printable(seen), r.Model)
		stopErr = seal.StopRun(context.WithoutCancel(ctx), r.ID)
	}}
	_, readErr := readEvents(pr, f.see)
	pr.CloseWithError(readErr)
	err := <-done
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	err = errors.Join(err, readErr, stopErr)
	if err == nil {
		err = after("follow")
	}
	return err
}

// collect is step 5: it collects the run into <runs>/<id>.partial, after
// removing any leftover of that name, and returns the record without the save.
func (r *run) collect(ctx context.Context) (*record, error) {
	state, err := seal.InspectRun(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	if state == nil || state.Status != "exited" {
		return nil, fmt.Errorf("the container kiln-run-%s has not stopped", r.ID)
	}

	partial := partialDir(r.Runs, r.ID)
	if err := os.RemoveAll(partial); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(partial, 0o755); err != nil {
		return nil, err
	}

	if err := writeOutput(partial, func(stdout, stderr io.Writer) error {
		return seal.ReadRun(ctx, r.ID, false, stdout, stderr)
	}); err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(partial, "documents.tar"), func(w io.Writer) error {
		return seal.CopyFromRun(ctx, r.ID, documentsPath, w)
	}); err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(partial, "session.tar"), func(w io.Writer) error {
		return seal.CopyFromRun(ctx, r.ID, sessionsPath, w)
	}); err != nil {
		// A run stopped before Claude Code wrote a session has none.
		if !strings.Contains(err.Error(), "Could not find the file") {
			return nil, err
		}
		if err := os.Remove(filepath.Join(partial, "session.tar")); err != nil {
			return nil, err
		}
	}

	output, err := os.Open(filepath.Join(partial, "output.jsonl"))
	if err != nil {
		return nil, err
	}
	defer output.Close()
	s, err := readEvents(output, nil)
	if err != nil {
		return nil, err
	}
	if err := after("collect"); err != nil {
		return nil, err
	}
	return &record{
		Inputs:       r.recordInputs(),
		Ending:       endingOf(state),
		ModelSeen:    s.model,
		Result:       s.result,
		TooLongLines: s.tooLong,
	}, nil
}

// save is step 6: it saves the run's working tree to the branch, and returns
// the saved commit, or noChange.
func (r *run) save(ctx context.Context, end Ending, guard string) (string, error) {
	base, err := basePack(ctx, r.Repo, r.tree)
	if err != nil {
		return "", err
	}
	tree, pack, err := seal.SaveRun(ctx, r.ID, r.Image, r.tree, base)
	if err != nil {
		return "", err
	}
	saved := noChange
	if tree != r.tree {
		if err := takeIn(ctx, r.Repo, tree, pack); err != nil {
			return "", err
		}
		// Dated from the run's end, so that the same save made again is the same
		// commit.
		commit, err := commitSave(ctx, r.Repo, tree, r.commit, r.ID, end.FinishedAt)
		if err != nil {
			return "", err
		}
		at, err := branchAt(ctx, r.Repo, r.ref)
		if err != nil {
			return "", err
		}
		if at != commit { // a branch already at it counts as saved
			if err := moveBranch(ctx, r.Repo, r.ref, commit, guard); err != nil {
				return "", fmt.Errorf("the branch %s has moved; the run's container is kept: %w", r.Branch, err)
			}
		}
		saved = commit
	}
	if err := after("save"); err != nil {
		return "", err
	}
	return saved, nil
}
