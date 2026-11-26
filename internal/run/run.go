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
	Repo   string // the instance's copy of the target repo, a bare repo
	Commit string // the commit the run starts from
	// Parent is the commit where the card's branch left main, when it is not
	// Commit: the baseline in the run's /work gets it as its one parent. It is
	// not among the labels a repeat must match: a repeat into a container that
	// is there keeps the parent that container was created with.
	Parent      string
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
	// The list of hosts the run may reach, and its way out's image and
	// outward network (ADR 0059). Empty, the run is sealed as before. The
	// list is not among the labels a repeat must match: a run keeps the way
	// out it was created with.
	Hosts         []string
	WayOutImage   string
	WayOutNetwork string
	// SkipSave leaves step 6 out: no save container, nothing taken into the
	// instance's copy, and the branch neither created nor moved. The record's
	// save is "not saved". The tick sets it for a spec run; `kiln run` by hand
	// never does.
	SkipSave bool
	// Title is the message's first line for the save, the pull request title
	// of the approved spec (TitleOf); empty, the save is "chore: save run
	// <id>". It is not among the labels a repeat must match. `kiln run` by
	// hand has no flag for it.
	Title string
	// Activity, when it is set, is handed what the run does while it is
	// followed (step 4), as it comes: the screen's activity. It is not
	// recorded, and not sent as JSON, as to a test's child.
	Activity func(Activity) `json:"-"`
	// Refused, when it is set, is called while the run is followed for every
	// event of its output that says the upstream refused the credential, so
	// that the core can renew it at once (ADR 0040). Like Activity, it is not
	// recorded.
	Refused func() `json:"-"`
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
	labelHosts       = "kiln.hosts"       // set by the seal from Inputs.Hosts
	// The parent tree the container's command names, set only when it names
	// one. It is not matched: a repeat takes it from the container.
	labelParentTree = "kiln.parent-tree"
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
	if in.Title != "" {
		if err := validTitle(in.Title); err != nil {
			return err
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
	parentTree  string // the tree of Parent, empty without one
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
	if in.Parent != "" {
		parent, err := gitLine(ctx, in.Repo, "rev-parse", "--verify", "--end-of-options", in.Parent+"^{commit}")
		if err != nil {
			return nil, fmt.Errorf("the parent commit %q: %w", in.Parent, err)
		}
		if parent != r.commit {
			if r.parentTree, err = gitLine(ctx, in.Repo, "rev-parse", "--verify", parent+"^{tree}"); err != nil {
				return nil, err
			}
		}
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

// parentTreeOf checks the parent tree that a container's label names: empty,
// or a tree in the repo, by its full ID.
func parentTreeOf(ctx context.Context, repo, label string) (string, error) {
	if label == "" {
		return "", nil
	}
	tree, err := gitLine(ctx, repo, "rev-parse", "--verify", "--end-of-options", label+"^{tree}")
	if err != nil || tree != label {
		return "", fmt.Errorf("the container's parent tree %q is not a tree of the repo: %v", label, err)
	}
	return tree, nil
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
// comes on standard input, and the effort is always given (ADR 0030). The
// baseline script runs first and takes four arguments, the last of them empty
// without a parent tree.
func (r *run) command() []string {
	return []string{"sh", "-c", baselineScript + "exec claude \"$@\" < /kiln/instruction.md\n", "sh",
		"/work", "/kiln/base.pack", r.tree, r.parentTree,
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

// ErrOtherInstruction is what Carry's refusal of a run matches when the run
// was recorded, or its container started, with another instruction than the
// one it is given now: the instruction's file was removed by hand and written
// again with another text.
var ErrOtherInstruction = errors.New("the run was started with another instruction")

// otherInstruction is a refusal for another instruction, with its own text.
type otherInstruction struct{ text string }

func (e *otherInstruction) Error() string        { return e.text }
func (e *otherInstruction) Is(target error) bool { return target == ErrOtherInstruction }

// Verdict is how a recorded run ended.
type Verdict struct {
	// Finished is true when the container exited with 0, it was not killed
	// for memory, the result is success and not an error, the model was the
	// one asked for, and the work is saved or nothing changed.
	Finished bool
	// Refused is true when the run ended because the upstream refused the
	// credential: its result carries api_error_status 401.
	Refused bool
	// Reason is one line: the container's ending, the result's subtype,
	// turns and cost, and the saved commit, "no change" or "not saved".
	Reason string
	// Hosts is the run's list of hosts, empty without one, and WayOutGone is
	// set when its way out was gone when it was collected (ADR 0059).
	Hosts      []string
	WayOutGone bool
	// Saved is the record's save: the saved commit, "no change" or "not
	// saved". Changed tells the first from the other two.
	Saved string
	// Cost is the result's total_cost_usd and Session its session_id; Cost is
	// nil when the cost is unknown (see record.cost).
	Cost    *float64
	Session string
}

// Changed reports whether the run's work was saved as a commit.
func (v Verdict) Changed() bool {
	return v.Saved != "" && v.Saved != noChange && v.Saved != notSaved
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
	rec, err := carry(ctx, in, out, errOut, errOut, func() error { return nil }, false)
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
// to do, collects it, saves its work unless SkipSave is set, and records it,
// and returns the verdict. Step 8 is Remove.
//
// starting is called once, right before the container is started for the
// first time, and puts the run in the store. When a step fails before
// starting has returned, or starting itself fails, the container, which has
// never started, is removed with its network. From the moment starting has
// returned, nothing is removed: a step that fails returns its error and
// leaves what is there for the next call.
//
// With starting nil, the run is in the store already; its container may
// never have started. Carry then removes nothing and never creates a
// container: one that never started has the proxy attached, its files placed
// again and is started; where there is no container and no record, Carry
// returns ErrGone. A cancelled context while it follows returns
// ErrInterrupted. It prints nothing of the run: the run's output is in its
// record. What a step notes and goes on after, such as a way out that is gone
// while its run runs, goes to notes.
func Carry(ctx context.Context, in Inputs, starting func() error, notes io.Writer) (Verdict, error) {
	rec, err := carry(ctx, in, io.Discard, io.Discard, notes, starting, true)
	if err != nil {
		return Verdict{}, err
	}
	cost, session := rec.cost()
	return Verdict{Finished: rec.status() == 0, Refused: rec.refused(), Reason: rec.reason(),
		Hosts: rec.Hosts, WayOutGone: rec.WayOut == wayOutGone, Saved: rec.Saved,
		Cost: cost, Session: session}, nil
}

// With stored, the run is known to be in the store once starting has
// returned, or from the outset when starting is nil; see create.
func carry(ctx context.Context, in Inputs, out, errOut, notes io.Writer, starting func() error, stored bool) (*record, error) {
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
			text := fmt.Sprintf("the container %s was started with other inputs (%v, not %v); it is left alone", "kiln-run-"+r.ID, got, want)
			if got[labelInstruction] != want[labelInstruction] {
				return nil, &otherInstruction{text}
			}
			return nil, errors.New(text)
		}
		// The command was fixed with the container, so the pack placed
		// again has to hold the parent tree it names, not today's.
		if r.parentTree, err = parentTreeOf(ctx, r.Repo, state.Labels[labelParentTree]); err != nil {
			return nil, err
		}
	}
	status := ""
	if state != nil {
		status = state.Status
	}
	if status == "" && starting == nil {
		return nil, ErrGone
	}

	// Where the branch is matters only to the save, which moves it. A run
	// that saves nothing, a spec run or a review run, never touches it: a
	// spec run after a send-back to spec starts from main while the card's
	// branch stays where it is for the implement run that follows (card 17).
	var at string
	if !r.SkipSave {
		if at, err = branchAt(ctx, r.Repo, r.ref); err != nil {
			return nil, err
		}
	}
	switch {
	case r.SkipSave || at == "" || at == r.commit:
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
			// A way out stopped since, as by a restart of the engine, is
			// started again; one that is gone is noted, and the run goes on
			// without the web.
			if err := seal.StartWayOut(ctx, r.ID); errors.Is(err, seal.ErrWayOutGone) {
				fmt.Fprintf(notes, "run %s: %v\n", r.ID, err)
			} else if err != nil {
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
	if r.SkipSave {
		rec.Saved = notSaved
	} else if rec.Saved, err = r.save(ctx, rec.Ending, guard); err != nil {
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

	// 1: CreateRun leaves nothing behind when it fails. A container that is
	// used again keeps everything CreateRun gave it but the proxy on its
	// network: a proxy replaced since is on no earlier run's network.
	if inStore && status == "created" {
		if err := seal.AttachProxy(ctx, r.ID, r.Proxy); err != nil {
			return err
		}
	} else {
		labels := r.labels()
		if r.parentTree != "" {
			labels[labelParentTree] = r.parentTree
		}
		if err := seal.CreateRun(ctx, seal.Run{ID: r.ID, Image: r.Image, Proxy: r.Proxy,
			Command: r.command(), Labels: labels, Instance: r.Instance,
			Hosts: r.Hosts, WayOutImage: r.WayOutImage, WayOutNetwork: r.WayOutNetwork}); err != nil {
			return err
		}
		if err := after("create"); err != nil {
			return undo(err)
		}
	}

	// 2: the same files of the same commit, whose labels step 0 checked, are
	// written again into a container that was placed into before.
	files, err := placingTar(ctx, r.Repo, r.entries)
	var base []byte
	if err == nil {
		trees := []string{r.tree}
		if r.parentTree != "" {
			trees = append(trees, r.parentTree)
		}
		base, err = basePack(ctx, r.Repo, trees...)
	}
	if err == nil {
		err = seal.PlaceRun(ctx, r.ID, bytes.NewReader(files), r.instruction, base)
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
	f := &follower{out: r.out, model: r.Model, activity: r.Activity, refused: r.Refused, mismatch: func(seen string) {
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

	// A run with a list keeps what its way out let through and refused
	// (ADR 0038, ADR 0059). One whose way out is gone has none. The way out
	// is stopped first, so that it writes the line of each connection still
	// open; a stopped way out is never started again for this run.
	var hosts []string
	if l := state.Labels[labelHosts]; l != "" {
		hosts = strings.Split(l, ",")
	}
	wayOut := ""
	if len(hosts) > 0 {
		if err := seal.StopWayOut(ctx, r.ID); err != nil {
			return nil, err
		}
		err := writeFile(filepath.Join(partial, "wayout.jsonl"), func(w io.Writer) error {
			return seal.ReadWayOut(ctx, r.ID, w)
		})
		if errors.Is(err, seal.ErrNoWayOut) {
			wayOut = wayOutGone
			err = os.Remove(filepath.Join(partial, "wayout.jsonl"))
		}
		if err != nil {
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
	if hosts == nil {
		hosts = []string{}
	}
	return &record{
		Inputs:       r.recordInputs(),
		Ending:       endingOf(state),
		ModelSeen:    s.model,
		Result:       s.result,
		TooLongLines: s.tooLong,
		Hosts:        hosts,
		WayOut:       wayOut,
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
		commit, err := commitSave(ctx, r.Repo, tree, r.commit, r.ID, r.Title, end.FinishedAt)
		if err != nil {
			return "", err
		}
		at, err := branchAt(ctx, r.Repo, r.ref)
		if err != nil {
			return "", err
		}
		if at != commit && at != "" {
			// This run's save of the same tree with another message, as a core
			// of another version wrote it, counts as saved too.
			same, err := savedAs(ctx, r.Repo, at, r.commit, r.ID, tree)
			if err != nil {
				return "", err
			}
			if same {
				commit = at
			}
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
