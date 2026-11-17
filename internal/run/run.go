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
	// 0: check the inputs, and look at what is there.
	r, err := check(ctx, in)
	if err != nil {
		return exitFailed, err
	}
	r.out, r.errOut = out, errOut

	rec, err := readRecord(r.Runs, r.ID)
	if err != nil {
		return exitFailed, err
	}
	if rec != nil {
		if err := rec.Inputs.matches(r); err != nil {
			return exitFailed, err
		}
		fmt.Fprintf(out, "the run %s is recorded already\n", r.ID)
		if err := r.remove(ctx); err != nil {
			return exitFailed, err
		}
		return rec.status(), nil
	}

	state, err := seal.InspectRun(ctx, r.ID) // refuses a container without kiln.run=<id>
	if err != nil {
		return exitFailed, err
	}
	if state != nil {
		want := r.labels()
		got := maps.Clone(state.Labels)
		maps.DeleteFunc(got, func(k, _ string) bool { _, ok := want[k]; return !ok })
		if !maps.Equal(got, want) {
			return exitFailed, fmt.Errorf("the container %s was started with other inputs (%v, not %v); it is left alone", "kiln-run-"+r.ID, got, want)
		}
	}
	status := ""
	if state != nil {
		status = state.Status
	}

	at, err := branchAt(ctx, r.Repo, r.ref)
	if err != nil {
		return exitFailed, err
	}
	switch {
	case at == "" || at == r.commit:
	case status == "exited":
		ours, err := isSaveOf(ctx, r.Repo, at, r.commit, r.ID)
		if err != nil {
			return exitFailed, err
		}
		if !ours {
			return exitFailed, fmt.Errorf("the branch %s is at %s, neither absent nor at the commit %s nor at this run's save", r.Branch, at, r.commit)
		}
	default:
		return exitFailed, fmt.Errorf("the branch %s is at %s, neither absent nor at the commit %s", r.Branch, at, r.commit)
	}
	// What update-ref requires of the branch: absent, or at the commit.
	guard := r.commit
	if at == "" {
		guard = ""
	}

	switch status {
	case "", "created":
		if err := r.create(ctx); err != nil {
			return exitFailed, err
		}
		fallthrough
	case "running":
		if status == "running" {
			// A proxy replaced since is on no earlier run's network.
			if err := seal.AttachProxy(ctx, r.ID, r.Proxy); err != nil {
				return exitFailed, err
			}
		}
		if code, err := r.follow(ctx); err != nil || code != 0 {
			return code, err
		}
	case "exited":
	default:
		return exitFailed, fmt.Errorf("the container kiln-run-%s is %s; it is left alone", r.ID, status)
	}

	rec, err = r.collect(ctx)
	if err != nil {
		return exitFailed, err
	}
	if rec.Saved, err = r.save(ctx, rec.Ending, guard); err != nil {
		return exitFailed, err
	}
	if err := r.record(rec); err != nil {
		return exitFailed, err
	}
	if err := r.remove(ctx); err != nil {
		return exitFailed, err
	}
	return rec.status(), nil
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
func (r *run) create(ctx context.Context) error {
	// 1: CreateRun leaves nothing behind when it fails.
	if err := seal.CreateRun(ctx, seal.Run{ID: r.ID, Image: r.Image, Proxy: r.Proxy,
		Command: r.command(), Labels: r.labels()}); err != nil {
		return err
	}
	if err := after("create"); err != nil {
		return r.undo(ctx, err)
	}

	// 2
	files, err := placingTar(ctx, r.Repo, r.entries)
	if err == nil {
		err = seal.PlaceRun(ctx, r.ID, bytes.NewReader(files), r.instruction)
	}
	if err == nil {
		err = after("place")
	}
	if err != nil {
		return r.undo(ctx, err)
	}

	// 3: a container that never started is removed; one that did stays.
	err = seal.StartRun(ctx, r.ID)
	if err == nil {
		err = after("start")
	}
	if err != nil {
		state, inspectErr := seal.InspectRun(context.WithoutCancel(ctx), r.ID)
		if inspectErr == nil && state != nil && state.Status == "created" {
			return r.undo(ctx, err)
		}
		return err
	}
	return nil
}

// follow is step 4: it prints the run's events until its container stops. If
// system/init names another model, it stops the run, which then goes through
// the other steps like any other. A cancelled context ends the following only.
func (r *run) follow(ctx context.Context) (int, error) {
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
		fmt.Fprintf(r.out, "following ended; the run goes on in kiln-run-%s, and the same command started again follows it and saves its work\n", r.ID)
		return exitInterrupted, nil
	}
	err = errors.Join(err, readErr, stopErr)
	if err == nil {
		err = after("follow")
	}
	if err != nil {
		return exitFailed, err
	}
	return 0, nil
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

// remove is step 8.
func (r *run) remove(ctx context.Context) error {
	if err := seal.RemoveRun(ctx, r.ID, r.Proxy); err != nil {
		return err
	}
	return after("remove")
}
