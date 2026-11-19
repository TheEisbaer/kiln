package tick

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
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

// TestDestination checks every row of the table of where a card goes at its
// run's end.
func TestDestination(t *testing.T) {
	const reason = "exit 0, result success, 1 turns, $0.0001, not saved"
	web := run.Verdict{Finished: true, Reason: reason, Hosts: []string{"*"}}
	spec := []byte("# Spec\n")
	diskErr := errors.New("read documents.tar: input/output error")
	for _, row := range []struct {
		what    string
		verdict run.Verdict
		spec    []byte
		problem string
		readErr error
		want    end
		wantErr error
	}{
		{"failed, by card 8's rule", run.Verdict{Reason: "exit 1, no result, not saved", Hosts: []string{"*"}}, nil, "", nil,
			end{store.Failed, failed, "exit 1, no result, not saved", nil}, nil},
		{"finished, a problem of the spec", web, nil, "the run wrote no spec (/kiln/out/spec.md)", nil,
			end{store.Finished, failed, "the run wrote no spec (/kiln/out/spec.md); " + reason, nil}, nil},
		{"finished, the archive damaged", web, nil, "the run's documents.tar is damaged: unexpected EOF", nil,
			end{store.Finished, failed, "the run's documents.tar is damaged: unexpected EOF; " + reason, nil}, nil},
		{"finished, the archive cannot be read from the disk", web, nil, "", diskErr, end{}, diskErr},
		{"finished, read, no list of hosts", run.Verdict{Finished: true, Reason: reason}, spec, "", nil,
			end{store.Finished, failed, "the spec was written without the web: no list of hosts; " + reason, ptr("# Spec\n")}, nil},
		{"finished, read, its way out gone", run.Verdict{Finished: true, Reason: reason, Hosts: []string{"*"}, WayOutGone: true}, spec, "", nil,
			end{store.Finished, failed, "the spec was written without the web: its way out was gone; " + reason, ptr("# Spec\n")}, nil},
		{"finished, read, a list and a way out", web, spec, "", nil,
			end{store.Finished, finished, reason, ptr("# Spec\n")}, nil},
	} {
		got, err := destination(row.verdict, row.spec, row.problem, row.readErr)
		if !errors.Is(err, row.wantErr) || (row.wantErr == nil && err != nil) {
			t.Errorf("%s: the error %v, want %v", row.what, err, row.wantErr)
		}
		if got.ending != row.want.ending || got.to != row.want.to || got.reason != row.want.reason ||
			(got.spec == nil) != (row.want.spec == nil) || (got.spec != nil && *got.spec != *row.want.spec) {
			t.Errorf("%s: %s to %v with %q and the spec %v, want %s to %v with %q and %v", row.what,
				got.ending, got.to, got.reason, show(got.spec), row.want.ending, row.want.to, row.want.reason, show(row.want.spec))
		}
	}
}

func ptr(s string) *string { return &s }

func show(s *string) string {
	if s == nil {
		return "none"
	}
	return fmt.Sprintf("%q", *s)
}

// TestInputs checks that a run that starts is told the instruction and the
// card's text, and that a run in flight keeps the file it was started with.
func TestInputs(t *testing.T) {
	c := &core{Config: Config{Data: t.TempDir(), Repo: "/repo.git", Image: "kiln-run", Proxy: "kiln-proxy"}, instance: "ab12cd34"}
	row := store.RunRow{ID: "ab12cd34-1-1", Card: 1, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}

	// A run that starts: the instruction, a line ## The card, an empty line and
	// the card's text, and the save skipped.
	in, err := c.inputs(row, "The card's text.", false)
	if err != nil {
		t.Fatal(err)
	}
	want := instructions.Spec + "## The card\n\nThe card's text."
	if got := string(readFile(t, in.Instruction)); got != want {
		t.Errorf("the instruction is\n%q\nwant\n%q", got, want)
	}
	if !strings.HasPrefix(want, "# Write the spec for a card\n") {
		t.Errorf("the instruction begins %q", want[:min(40, len(want))])
	}
	if !in.SkipSave {
		t.Error("the run's save is not skipped")
	}

	// A run in flight whose file holds another text keeps it: a core rebuilt
	// with another instruction does not write it again, and its label still
	// matches.
	other := "an earlier instruction\n## The card\n\nThe card's text."
	if err := os.WriteFile(in.Instruction, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := c.inputs(row, "The card's text.", true)
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
	if _, err := c.inputs(row, "The card's text.", true); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, in.Instruction)); got != want {
		t.Errorf("the missing file was written as %q", got)
	}

	// A run that starts writes its file again, with the same text.
	if err := os.WriteFile(in.Instruction, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.inputs(row, "The card's text.", false); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, in.Instruction)); got != want {
		t.Errorf("the file of a run that starts is %q", got)
	}
}
