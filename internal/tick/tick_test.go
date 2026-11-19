package tick

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/run"
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
