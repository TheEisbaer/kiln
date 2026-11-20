package tick

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/instructions"
	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/store"
)

// The variable by which the test runs its own binary as `kiln core` or `kiln
// card`. Only TestMain reads it.
const asKilnVar = "KILN_TICK_TEST_AS_KILN" // a child, as JSON

// child is what the test binary does as a child.
type child struct {
	Command string // core or card
	Config  Config // of core
	Text    string // of card
	ExitAt  string // the point of a tick at which core exits, as a kill there would
	// Hook is <where>:<point>:<how>. Where is tick, a point of the tick, or
	// run, a step of internal/run, after which the hook is called. How is
	// exit, as a kill there would end core; interrupt, as SIGINT or SIGTERM
	// there would, returning once the interrupt has cancelled core's context;
	// or fail, which fails the step (run only).
	Hook string
}

// exitAtPoint is the status with which the binary exits at a point, as a kill
// there would end it.
const exitAtPoint = 3

// testInterval is the tick of the test's cores.
const testInterval = 50 * time.Millisecond

func TestMain(m *testing.M) {
	if spec := os.Getenv(asKilnVar); spec != "" {
		os.Exit(asKiln(spec))
	}
	os.Exit(m.Run())
}

// asKiln is `kiln core` or `kiln card`, as cmd/core starts them, with a tick
// of testInterval and atPoint set to exit at the child's point.
func asKiln(spec string) int {
	var c child
	if err := json.Unmarshal([]byte(spec), &c); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	switch c.Command {
	case "card":
		id, err := AddCard(c.Config.Data, c.Text)
		if err != nil {
			fmt.Fprintln(os.Stderr, "kiln card:", err)
			return 1
		}
		fmt.Println(id)
		return 0
	case "core":
		interval = testInterval
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		where, point, how := hook(c.Hook)
		act := func() error {
			switch how {
			case "exit":
				os.Exit(exitAtPoint)
			case "interrupt":
				syscall.Kill(os.Getpid(), syscall.SIGINT)
				<-ctx.Done()
			case "fail":
				return fmt.Errorf("failed by the test after the step %s", point)
			}
			return nil
		}
		atPoint = func(p string) {
			switch {
			case p == c.ExitAt:
				os.Exit(exitAtPoint)
			case where == "tick" && p == point:
				act()
			}
		}
		if where == "run" {
			run.SetAfterStep(func(step string) error {
				if step == point {
					return act()
				}
				return nil
			})
		}
		if err := Core(ctx, c.Config, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "kiln core:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "no command %q\n", c.Command)
	return 2
}

// hook splits a child's Hook into where, the point and how.
func hook(h string) (where, point, how string) {
	where, rest, _ := strings.Cut(h, ":")
	point, how, _ = strings.Cut(rest, ":")
	return where, point, how
}

// names are the actions' names, for the test's messages.
var names = map[action]string{start: "start", carry: "carry", gone: "gone", leave: "leave"}

// TestDecide checks every row of the table of what a tick does for a card
// that waits for a run.
func TestDecide(t *testing.T) {
	for _, row := range []struct {
		what      string
		inFlight  bool
		container string
		recorded  bool
		want      action
	}{
		{"no run in flight, none of this instance's", false, "", false, start},
		{"in flight, created: killed between AddRun and the start", true, "created", false, carry},
		{"in flight, running", true, "running", false, carry},
		{"in flight, exited, not recorded", true, "exited", false, carry},
		{"in flight, recorded, no container", true, "", true, carry},
		{"in flight, recorded, created", true, "created", true, carry},
		{"in flight, recorded, running", true, "running", true, carry},
		{"in flight, recorded, exited", true, "exited", true, carry},
		{"in flight, recorded, dead", true, "dead", true, carry},
		{"in flight, no container, not recorded: failed", true, "", false, gone},
		{"in flight, paused", true, "paused", false, leave},
		{"in flight, restarting", true, "restarting", false, leave},
		{"in flight, dead", true, "dead", false, leave},
	} {
		if got := decide(row.inFlight, row.container, row.recorded); got != row.want {
			t.Errorf("%s: %s, want %s", row.what, names[got], names[row.want])
		}
	}
}

// TestDestination checks every row of the tables of where a card goes at its
// run's end, the spec stage's and that of implement and review.
func TestDestination(t *testing.T) {
	const reason = "exit 0, result success, 1 turns, $0.0001, not saved"
	const savedReason = "exit 0, result success, 1 turns, $0.0001, saved c0ffee"
	const unchanged = "exit 0, result success, 1 turns, $0.0001, no change"
	web := run.Verdict{Finished: true, Reason: reason, Hosts: []string{"*"}, Saved: "not saved"}
	review := run.Verdict{Finished: true, Reason: reason, Saved: "not saved"}
	saved := run.Verdict{Finished: true, Reason: savedReason, Saved: "c0ffee"}
	nothing := run.Verdict{Finished: true, Reason: unchanged, Saved: "no change"}
	doc := func(text string) run.Document { return run.Document{Text: []byte(text)} }
	kept := func(kind string, text string) *store.Doc { return &store.Doc{Kind: kind, Text: text} }
	diskErr := errors.New("read documents.tar: input/output error")
	noReport := run.Document{Problem: "the run wrote no report (/kiln/out/report.md)", Missing: true}
	for _, row := range []struct {
		what    string
		stage   store.Stage
		verdict run.Verdict
		doc     run.Document
		readErr error
		want    end
		wantErr error
	}{
		// The spec stage's table, as card 11 has it.
		{"spec: failed, by card 8's rule", store.Spec, run.Verdict{Reason: "exit 1, no result, not saved", Hosts: []string{"*"}}, run.Document{}, nil,
			end{store.Failed, failed, "exit 1, no result, not saved", nil}, nil},
		{"spec: finished, a problem of the spec", store.Spec, web, run.Document{Problem: "the run wrote no spec (/kiln/out/spec.md)", Missing: true}, nil,
			end{store.Finished, failed, "the run wrote no spec (/kiln/out/spec.md); " + reason, nil}, nil},
		{"spec: finished, the archive damaged", store.Spec, web, run.Document{Problem: "the run's documents.tar is damaged: unexpected EOF"}, nil,
			end{store.Finished, failed, "the run's documents.tar is damaged: unexpected EOF; " + reason, nil}, nil},
		{"spec: finished, the archive cannot be read from the disk", store.Spec, web, run.Document{}, diskErr, end{}, diskErr},
		{"spec: finished, read, no list of hosts", store.Spec, run.Verdict{Finished: true, Reason: reason}, doc("# Spec\n"), nil,
			end{store.Finished, failed, "the spec was written without the web: no list of hosts; " + reason, kept(store.KindSpec, "# Spec\n")}, nil},
		{"spec: finished, read, its way out gone", store.Spec, run.Verdict{Finished: true, Reason: reason, Hosts: []string{"*"}, WayOutGone: true}, doc("# Spec\n"), nil,
			end{store.Finished, failed, "the spec was written without the web: its way out was gone; " + reason, kept(store.KindSpec, "# Spec\n")}, nil},
		{"spec: finished, read, a list and a way out", store.Spec, web, doc("# Spec\n"), nil,
			end{store.Finished, finished, reason, kept(store.KindSpec, "# Spec\n")}, nil},

		// Implement.
		{"implement: failed", store.Implement, run.Verdict{Reason: "exit 1, no result, no change"}, run.Document{}, nil,
			end{store.Failed, implementNeedsYou, "exit 1, no result, no change", nil}, nil},
		{"implement: finished, nothing changed", store.Implement, nothing, doc("# Report\n"), nil,
			end{store.Finished, implementNeedsYou, "the run changed nothing; " + unchanged, kept(store.KindReport, "# Report\n")}, nil},
		{"implement: finished, a commit saved", store.Implement, saved, doc("# Report\n"), nil,
			end{store.Finished, reviewWaits, savedReason, kept(store.KindReport, "# Report\n")}, nil},
		{"implement: finished, a commit saved, no report", store.Implement, saved, noReport, nil,
			end{store.Finished, reviewWaits, savedReason, nil}, nil},
		{"implement: finished, nothing changed, no report", store.Implement, nothing, noReport, nil,
			end{store.Finished, implementNeedsYou, "the run changed nothing; " + unchanged, nil}, nil},
		{"implement: finished, a commit saved, a report too long", store.Implement, saved, run.Document{Problem: "the report is 262145 bytes, more than 262144"}, nil,
			end{store.Finished, reviewWaits, "the report is 262145 bytes, more than 262144; " + savedReason, nil}, nil},
		{"implement: finished, the archive cannot be read from the disk", store.Implement, saved, run.Document{}, diskErr, end{}, diskErr},

		// Review.
		{"review: failed", store.Review, run.Verdict{Reason: "exit 1, no result, not saved"}, run.Document{}, nil,
			end{store.Failed, reviewNeedsYou, "exit 1, no result, not saved", nil}, nil},
		{"review: finished, no review.md", store.Review, review, run.Document{Problem: "the run wrote no review (/kiln/out/review.md)", Missing: true}, nil,
			end{store.Finished, reviewNeedsYou, "the run wrote no review (/kiln/out/review.md); " + reason, nil}, nil},
		{"review: finished, review.md empty", store.Review, review, run.Document{Problem: "the review is empty"}, nil,
			end{store.Finished, reviewNeedsYou, "the review is empty; " + reason, nil}, nil},
		{"review: finished, the archive cannot be read from the disk", store.Review, review, run.Document{}, diskErr, end{}, diskErr},
		{"review: finished, Verdict: goes back", store.Review, review, doc("Verdict: goes back\n\n1. A finding.\n"), nil,
			end{store.Finished, reviewNeedsYou, "the review sends the change back; " + reason, kept(store.KindReview, "Verdict: goes back\n\n1. A finding.\n")}, nil},
		{"review: finished, Verdict: passes", store.Review, review, doc("Verdict: passes\n\nNo findings.\n"), nil,
			end{store.Finished, reviewPassed, reason, kept(store.KindReview, "Verdict: passes\n\nNo findings.\n")}, nil},
		{"review: finished, Verdict: passes, with a carriage return", store.Review, review, doc("Verdict: passes\r\n"), nil,
			end{store.Finished, reviewPassed, reason, kept(store.KindReview, "Verdict: passes\r\n")}, nil},
		{"review: finished, the verdict not on the first line", store.Review, review, doc("# Review\nVerdict: passes\n"), nil,
			end{store.Finished, reviewNeedsYou, "the review has no verdict; " + reason, kept(store.KindReview, "# Review\nVerdict: passes\n")}, nil},
		{"review: finished, another verdict", store.Review, review, doc("Verdict: passes, mostly\n"), nil,
			end{store.Finished, reviewNeedsYou, "the review has no verdict; " + reason, kept(store.KindReview, "Verdict: passes, mostly\n")}, nil},
	} {
		got, err := destination(row.stage, row.verdict, row.doc, row.readErr)
		if !errors.Is(err, row.wantErr) || (row.wantErr == nil && err != nil) {
			t.Errorf("%s: the error %v, want %v", row.what, err, row.wantErr)
		}
		if got.ending != row.want.ending || got.to != row.want.to || got.reason != row.want.reason ||
			(got.doc == nil) != (row.want.doc == nil) || (got.doc != nil && *got.doc != *row.want.doc) {
			t.Errorf("%s: %s to %v with %q and the document %v, want %s to %v with %q and %v", row.what,
				got.ending, got.to, got.reason, show(got.doc), row.want.ending, row.want.to, row.want.reason, show(row.want.doc))
		}
	}
}

func show(d *store.Doc) string {
	if d == nil {
		return "none"
	}
	return fmt.Sprintf("%s %q", d.Kind, d.Text)
}

// TestInputs checks that a spec run that starts is told the instruction and
// the card's text, and that a run in flight keeps the file it was started with.
func TestInputs(t *testing.T) {
	c := &core{Config: Config{Data: t.TempDir(), Repo: "/repo.git", Image: "kiln-run", Proxy: "kiln-proxy",
		Hosts: []string{"go.dev"}, WayOutImage: "kiln-wayout", WayOutNetwork: "kiln-web"}, instance: "ab12cd34"}
	row := store.RunRow{ID: "ab12cd34-1-1", Card: 1, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}
	card := store.Card{ID: 1, Text: "The card's text.", State: waiting}

	// A run that starts: the instruction, a line ## The card, an empty line and
	// the card's text, the save skipped and the list of hosts given.
	in, held, err := c.inputs(t.Context(), row, card, false)
	if err != nil || held != "" {
		t.Fatal(err, held)
	}
	want := instructions.Spec + "## The card\n\nThe card's text."
	if got := string(readFile(t, in.Instruction)); got != want {
		t.Errorf("the instruction is\n%q\nwant\n%q", got, want)
	}
	if !strings.HasPrefix(want, "# Write the spec for a card\n") {
		t.Errorf("the instruction begins %q", want[:min(40, len(want))])
	}
	if !in.SkipSave || !reflect.DeepEqual(in.Hosts, c.Hosts) || in.WayOutImage == "" || in.Branch != "kiln/card-1" {
		t.Errorf("the spec run's inputs are %+v", in)
	}

	// A run in flight whose file holds another text keeps it: a core rebuilt
	// with another instruction does not write it again, and its label still
	// matches.
	other := "an earlier instruction\n## The card\n\nThe card's text."
	if err := os.WriteFile(in.Instruction, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	again, _, err := c.inputs(t.Context(), row, card, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, again.Instruction)); got != other {
		t.Errorf("the file of a run in flight was written again: %q", got)
	}
	if !reflect.DeepEqual(again, in) {
		t.Errorf("the inputs differ: %+v, was %+v", again, in)
	}

	// A run in flight whose file is missing has it written again.
	if err := os.Remove(in.Instruction); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.inputs(t.Context(), row, card, true); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, in.Instruction)); got != want {
		t.Errorf("the missing file was written as %q", got)
	}

	// A run that starts writes its file again, with the same text.
	if err := os.WriteFile(in.Instruction, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.inputs(t.Context(), row, card, false); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, in.Instruction)); got != want {
		t.Errorf("the file of a run that starts is %q", got)
	}
}

// stagedCore is a core on a store and a repo of its own, with card 1 and its
// approved spec, and the repo's main at one commit.
func stagedCore(t *testing.T) (*core, string) {
	t.Helper()
	data := t.TempDir()
	s, err := store.Open(filepath.Join(data, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	repo, commit := bareRepo(t, map[string]string{"NOTES.md": "# Notes\n"})
	c := &core{Config: Config{Data: data, Repo: repo, Image: "kiln-run", Proxy: "kiln-proxy", Model: "claude-opus-5-5",
		ImplementModel: "claude-sonnet-5-5", ReviewModel: "claude-opus-5-6", Hosts: []string{"go.dev"}}, store: s, instance: "ab12cd34", out: io.Discard, errOut: io.Discard}
	if _, err := s.Create("The card's text."); err != nil {
		t.Fatal(err)
	}
	endRun(t, c, store.Spec, store.State{Stage: store.Spec, WaitsFor: store.Approval}, &store.Doc{Kind: store.KindSpec, Text: "# The spec\n"})
	return c, commit
}

// endRun ends a run of card 1 in the stage, with its document, as a run would.
func endRun(t *testing.T, c *core, s store.Stage, to store.State, doc *store.Doc) {
	t.Helper()
	if _, err := c.store.Move(1, store.State{Stage: s, WaitsFor: store.Run}, store.CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	n, err := c.store.RunCount(1)
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("ab12cd34-1-%d", n+1)
	if err := c.store.AddRun(store.RunRow{ID: id, Card: 1, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.EndRun(id, store.Finished, "", to, doc); err != nil {
		t.Fatal(err)
	}
}

// commitOnBranch commits files on top of a commit of a bare repo and moves the
// card's branch to it, as a save would, and returns the new commit.
func commitOnBranch(t *testing.T, repo, parent string, files map[string]string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	gitIn(t, filepath.Dir(work), "clone", "--quiet", repo, work)
	gitIn(t, work, "checkout", "--quiet", "--detach", parent)
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(work, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, work, "add", "--all")
	gitIn(t, work, "commit", "--quiet", "--message", "a save")
	commit := gitIn(t, work, "rev-parse", "HEAD")
	gitIn(t, work, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/kiln/card-1")
	return commit
}

// toldParts are the headings of an instruction after its stage's own text, in
// order.
func toldParts(t *testing.T, instruction, own string) []string {
	t.Helper()
	if !strings.HasPrefix(instruction, own) {
		t.Fatalf("the instruction does not begin with the stage's own text:\n%q", instruction[:min(200, len(instruction))])
	}
	var headings []string
	for _, line := range strings.Split(instruction[len(own):], "\n") {
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, line)
		}
	}
	return headings
}

// TestStageInputs checks what an implement run and a review run start from
// and are told: the headings in order, the last review only when it sent the
// change back, and the change as a diff.
func TestStageInputs(t *testing.T) {
	ctx := t.Context()
	card := func(c *core) store.Card {
		t.Helper()
		got, err := c.store.Card(1)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	newRow := func(c *core, commit string) store.RunRow {
		t.Helper()
		cd := card(c)
		n, _ := c.store.RunCount(1)
		return store.RunRow{ID: fmt.Sprintf("ab12cd34-1-%d", n+1), Card: 1, Commit: commit, Model: c.model(cd.Stage), Effort: stages[cd.Stage].effort}
	}

	t.Run("models and efforts", func(t *testing.T) {
		c := Config{Model: "claude-opus-5-5", ImplementModel: "claude-sonnet-5-5", ReviewModel: "claude-opus-5-6"}
		for st, want := range map[store.Stage][2]string{
			store.Spec: {"claude-opus-5-5", "high"}, store.Implement: {"claude-sonnet-5-5", "medium"}, store.Review: {"claude-opus-5-6", "high"},
		} {
			if got := c.model(st); got != want[0] || stages[st].effort != want[1] {
				t.Errorf("%s: %s at %s, want %s at %s", st, got, stages[st].effort, want[0], want[1])
			}
		}
		if got := (Config{Model: "claude-opus-5-5"}).model(store.Review); got != "claude-opus-5-5" {
			t.Errorf("the review model without its flag is %s", got)
		}
	})

	t.Run("implement, first", func(t *testing.T) {
		c, main := stagedCore(t)
		if _, err := c.store.Move(1, store.State{Stage: store.Implement, WaitsFor: store.Run}, store.CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		// From the head of main: the card has no branch.
		commit, held, err := c.startsFrom(ctx, card(c))
		if err != nil || held != "" || commit != main {
			t.Fatalf("an implement run starts from %q (%q, %v), want main's %s", commit, held, err, main)
		}
		in, held, err := c.inputs(ctx, newRow(c, commit), card(c), false)
		if err != nil || held != "" {
			t.Fatal(err, held)
		}
		got := string(readFile(t, in.Instruction))
		want := instructions.Implement + "## The card\n\nThe card's text.\n\n## The approved spec\n\n# The spec\n"
		if got != want {
			t.Errorf("the implement run is told\n%q\nwant\n%q", got, want)
		}
		if in.SkipSave || in.Hosts != nil || in.WayOutImage != "" || in.Model != "claude-sonnet-5-5" || in.Effort != "medium" || in.Commit != main {
			t.Errorf("the implement run's inputs are %+v", in)
		}
	})

	t.Run("implement, after a review", func(t *testing.T) {
		for _, tc := range []struct {
			review string
			told   bool
		}{
			{"Verdict: goes back\n\n1. NOTES.md lacks a line.\n", true},
			{"Verdict: passes\n", false},
			{"No verdict here.\n", false},
		} {
			c, main := stagedCore(t)
			endRun(t, c, store.Implement, reviewWaits, &store.Doc{Kind: store.KindReport, Text: "# Report\n"})
			tip := commitOnBranch(t, c.Repo, main, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
			endRun(t, c, store.Review, reviewNeedsYou, &store.Doc{Kind: store.KindReview, Text: tc.review})
			if _, err := c.store.Move(1, store.State{Stage: store.Implement, WaitsFor: store.Run}, store.CauseDeveloper); err != nil {
				t.Fatal(err)
			}
			// From the branch's tip.
			commit, held, err := c.startsFrom(ctx, card(c))
			if err != nil || held != "" || commit != tip {
				t.Fatalf("a second implement run starts from %q (%q, %v), want the tip %s", commit, held, err, tip)
			}
			in, _, err := c.inputs(ctx, newRow(c, commit), card(c), false)
			if err != nil {
				t.Fatal(err)
			}
			got := string(readFile(t, in.Instruction))
			wantHeadings := []string{"## The card", "## The approved spec"}
			if tc.told {
				wantHeadings = append(wantHeadings, "## The last review")
				if !strings.HasSuffix(got, "## The last review\n\n"+tc.review) {
					t.Errorf("the review %q is not the end of\n%s", tc.review, got)
				}
			}
			if h := toldParts(t, got, instructions.Implement); !reflect.DeepEqual(h, wantHeadings) {
				t.Errorf("after the review %q the headings are %q, want %q", tc.review, h, wantHeadings)
			}
		}
	})

	t.Run("implement, after a new spec", func(t *testing.T) {
		c, main := stagedCore(t)
		commitOnBranch(t, c.Repo, main, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		endRun(t, c, store.Review, reviewNeedsYou, &store.Doc{Kind: store.KindReview, Text: "Verdict: goes back\n"})
		endRun(t, c, store.Spec, finished, &store.Doc{Kind: store.KindSpec, Text: "# A new spec\n"})
		if _, err := c.store.Move(1, store.State{Stage: store.Implement, WaitsFor: store.Run}, store.CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		in, _, err := c.inputs(ctx, newRow(c, main), card(c), false)
		if err != nil {
			t.Fatal(err)
		}
		want := instructions.Implement + "## The card\n\nThe card's text.\n\n## The approved spec\n\n# A new spec\n"
		if got := string(readFile(t, in.Instruction)); got != want {
			t.Errorf("after a new spec the implement run is told\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("review", func(t *testing.T) {
		c, main := stagedCore(t)
		endRun(t, c, store.Implement, reviewWaits, &store.Doc{Kind: store.KindReport, Text: "# Report\n"})
		first := commitOnBranch(t, c.Repo, main, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		tip := commitOnBranch(t, c.Repo, first, map[string]string{"NOTES.md": "# Notes\nA line.\nA fix with ```` in it.\n"})
		if _, err := c.store.Move(1, reviewWaits, store.CauseRun); err != nil {
			t.Fatal(err)
		}
		commit, held, err := c.startsFrom(ctx, card(c))
		if err != nil || held != "" || commit != tip {
			t.Fatalf("a review run starts from %q (%q, %v), want the tip %s", commit, held, err, tip)
		}
		in, held, err := c.inputs(ctx, newRow(c, commit), card(c), false)
		if err != nil || held != "" {
			t.Fatal(err, held)
		}
		got := string(readFile(t, in.Instruction))
		diff := gitIn(t, c.Repo, "diff", main, tip) + "\n"
		want := instructions.Review + "## The card\n\nThe card's text.\n\n## The approved spec\n\n# The spec\n\n## The change\n\n" +
			"`````diff\n" + diff + "`````\n"
		if got != want {
			t.Errorf("the review run is told\n%s\nwant\n%s", got, want)
		}
		if !strings.Contains(diff, "+A line.") || !strings.Contains(diff, "+A fix") {
			t.Errorf("the diff is not from where the branch left main:\n%s", diff)
		}
		if !in.SkipSave || in.Hosts != nil || in.Model != "claude-opus-5-6" || in.Effort != "high" || in.Commit != tip {
			t.Errorf("the review run's inputs are %+v", in)
		}
	})

	t.Run("review, a change too large", func(t *testing.T) {
		c, main := stagedCore(t)
		tip := commitOnBranch(t, c.Repo, main, map[string]string{"big.txt": strings.Repeat("a line of the change\n", 60000)})
		if _, err := c.store.Move(1, reviewWaits, store.CauseRun); err != nil {
			t.Fatal(err)
		}
		row := newRow(c, tip)
		in, held, err := c.inputs(ctx, row, card(c), false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(held, "the change is ") || !strings.HasSuffix(held, " bytes as a diff, more than 1048576; no review run is started") {
			t.Errorf("held is %q", held)
		}
		if _, err := os.Stat(filepath.Join(c.Data, "instructions", row.ID+".md")); err == nil || in.ID != "" {
			t.Error("an instruction was written for a review run that is held")
		}
		// The card needs the developer, with the reason, and no run is in the
		// store.
		if err := c.hold(card(c), held); err != nil {
			t.Fatal(err)
		}
		h, _ := c.store.History(1)
		if e := h[len(h)-1]; e.State != reviewNeedsYou || e.Reason != held || e.Run != "" {
			t.Errorf("the last entry is %+v", e)
		}
		if r, _ := c.store.InFlight(1); r != nil {
			t.Errorf("a run is in flight: %+v", r)
		}
	})

	t.Run("review, no branch", func(t *testing.T) {
		c, _ := stagedCore(t)
		if _, err := c.store.Move(1, reviewWaits, store.CauseRun); err != nil {
			t.Fatal(err)
		}
		if _, held, err := c.startsFrom(ctx, card(c)); err != nil || held != "the card's branch kiln/card-1 does not exist; no change to review" {
			t.Errorf("a review without a branch: %q, %v", held, err)
		}
	})

	t.Run("spec, with a branch", func(t *testing.T) {
		c, main := stagedCore(t)
		commitOnBranch(t, c.Repo, main, map[string]string{"NOTES.md": "x\n"})
		if _, err := c.store.Move(1, waiting, store.CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		if commit, _, err := c.startsFrom(ctx, card(c)); err != nil || commit != main {
			t.Errorf("a spec run starts from %q (%v), want main's %s", commit, err, main)
		}
	})
}

func TestFenced(t *testing.T) {
	for in, want := range map[string]string{
		"":              "```diff\n```\n",
		"+a\n":          "```diff\n+a\n```\n",
		"+a":            "```diff\n+a\n```\n",
		"+```\n":        "````diff\n+```\n````\n",
		"+`` and ```\n": "````diff\n+`` and ```\n````\n",
	} {
		if got := fenced(in); got != want {
			t.Errorf("fenced(%q) is %q, want %q", in, got, want)
		}
	}
}
