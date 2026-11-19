package tick

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/seal"
	"github.com/TheEisbaer/kiln/internal/store"
)

// The model and effort of the test's runs.
const (
	model  = "claude-haiku-5-5"
	effort = "low"
)

// The words in a card's text that change what the stand-in does.
const (
	gateWord = "kiln-stand-in-gate"
	failWord = "kiln-stand-in-fail"
)

// upstreamPort is where internal/run's scripted upstream listens.
const upstreamPort = "8081"

// waitLimit bounds every wait of the test; none waits a fixed time.
const waitLimit = 2 * time.Minute

// fixture is what the test's cases share.
type fixture struct {
	id         string // random; names the images, the proxy and the upstream
	runImage   string // the run image, with the pinned Claude Code
	standIn    string // the run image with the stand-in for Claude Code
	proxyImage string
	proxy      seal.Proxy
	upstream   string
	marker     string
	// The output.jsonl of the stand-in's run in the first case, which the
	// real case compares with its own.
	standInOutput []byte
}

// TestTick is a large test (ADR 0031, ADR 0048): it needs the container
// engine. It runs its own binary as `kiln core` with a tick of 50 ms, against
// a fresh data directory, instance and repo per case, and watches the store
// and the starts of the instance's containers. Every case but one runs the
// stand-in for Claude Code; the last runs the pinned Claude Code against
// internal/run's scripted upstream.
//
// It runs only with KILN_ENGINE_TESTS=1, and then fails, not skips, when the
// engine cannot be reached. Every name it uses carries a random id, and
// everything it creates is removed, even on failure.
func TestTick(t *testing.T) {
	if os.Getenv("KILN_ENGINE_TESTS") != "1" {
		t.Skip("needs the container engine; set KILN_ENGINE_TESTS=1")
	}
	if _, err := docker(t.Context(), "info", "--format", "{{.ServerVersion}}"); err != nil {
		t.Fatalf("the container engine is not reachable: %v", err)
	}

	f := &fixture{id: randomID(t)}
	name := func(what string) string { return "kiln-ticktest-" + f.id + "-" + what }
	// Registered first, so it runs after every other cleanup.
	t.Cleanup(func() { assertNothingLeft(t, "ticktest-"+f.id) })
	f.marker = "kiln-marker-" + f.id

	// Images. The repo's root is the build context, except for the stand-in's.
	f.proxyImage, f.runImage, f.standIn = name("proxy"), name("run"), name("stand-in")
	upstreamImage := name("upstream")
	build(t, f.proxyImage, "../..", "images/proxy/Dockerfile")
	build(t, upstreamImage, "../..", "internal/run/testdata/claude/Dockerfile")
	build(t, f.runImage, "../..", "images/run/Dockerfile")
	build(t, f.standIn, "testdata/claude", "internal/tick/testdata/claude/Dockerfile", "--build-arg", "RUN="+f.runImage)

	// The proxy creates its ordinary network; the upstream joins it.
	ordinary := name("ordinary")
	t.Cleanup(func() { dockerQuiet("network", "rm", ordinary) })
	f.upstream = name("upstream")
	f.proxy = seal.Proxy{Name: name("proxy"), Image: f.proxyImage, Network: ordinary,
		Upstream: "http://" + f.upstream + ":" + upstreamPort}
	t.Cleanup(func() { dockerQuiet("rm", "--force", f.proxy.Name) })
	if err := seal.ReplaceProxy(t.Context(), f.proxy, "echo fake-credential-"+f.id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dockerQuiet("rm", "--force", f.upstream) })
	must(t, "run", "--detach", "--name", f.upstream, "--network", ordinary, upstreamImage, f.marker)

	t.Run("a card that waits gets one run", func(t *testing.T) {
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Add a line to NOTES.md. kiln-case-happy")
		core, done := f.core(t, c, "")
		f.waitEnded(t, c)
		f.stop(t, core, done)
		f.assertEnded(t, c, store.Finished)
		f.standInOutput = readFile(t, filepath.Join(c.data, "runs", c.run, "output.jsonl"))
	})

	t.Run("killed, then started again", func(t *testing.T) {
		for _, point := range []string{"before-add", "after-add", "following", "before-end", "after-end"} {
			t.Run(point, func(t *testing.T) {
				c := f.newCase(t, f.standIn)
				text := "Add a line to NOTES.md."
				if point == "following" {
					text += " " + gateWord
				}
				f.card(t, c, text)
				if point == "following" {
					core, done := f.core(t, c, "")
					f.waitAtGate(t, c)
					core.Process.Signal(syscall.SIGKILL)
					<-done
					must(t, "exec", "kiln-run-"+c.run, "touch", "/tmp/gate")
				} else {
					_, done := f.core(t, c, point)
					if code := <-done; code != exitAtPoint {
						t.Fatalf("the core exited with %d, want %d at %s", code, exitAtPoint, point)
					}
				}

				// What the kill left.
				s := f.open(t, c)
				inFlight, err := s.InFlight(1)
				s.Close()
				if err != nil {
					t.Fatal(err)
				}
				switch point {
				case "before-add":
					if inFlight != nil {
						t.Errorf("a run is in flight: %+v", inFlight)
					}
					if got := containerState(t, c.run); got != "created" {
						t.Errorf("the container is %q, want created and never started", got)
					}
				case "after-add":
					if inFlight == nil || inFlight.ID != c.run {
						t.Errorf("the run in flight is %+v, want %s", inFlight, c.run)
					}
					if got := containerState(t, c.run); got != "created" {
						t.Errorf("the container is %q, want created and never started", got)
					}
				case "before-end":
					if inFlight == nil {
						t.Error("no run is in flight after result.json")
					}
					if _, err := os.Stat(filepath.Join(c.data, "runs", c.run, "result.json")); err != nil {
						t.Error(err)
					}
					if got := containerState(t, c.run); got != "exited" {
						t.Errorf("the container is %q while the run is in flight, want exited", got)
					}
				case "after-end":
					if inFlight != nil {
						t.Errorf("a run is in flight after EndRun: %+v", inFlight)
					}
					if got := containerState(t, c.run); got != "exited" {
						t.Errorf("the container is %q, want exited and not yet removed", got)
					}
				}

				core, done := f.core(t, c, "")
				f.waitEnded(t, c)
				f.stop(t, core, done)
				f.assertEnded(t, c, store.Finished)
			})
		}
	})

	t.Run("interrupted, then started again", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			exitAt string // a first core's kill, before the interrupted one
			at     string // where the interrupted core interrupts itself
		}{
			{"right after AddRun", "", "tick:after-add"},
			{"while a stored run is created again", "after-add", "run:create"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c := f.newCase(t, f.standIn)
				f.card(t, c, "Add a line to NOTES.md.")
				if tc.exitAt != "" {
					_, done := f.core(t, c, tc.exitAt)
					if code := <-done; code != exitAtPoint {
						t.Fatalf("the first core exited with %d, want %d at %s", code, exitAtPoint, tc.exitAt)
					}
				}
				ch := child{Command: "core", Config: f.config(c), InterruptAt: tc.at}
				if code, out, errOut := f.kiln(t, ch); code != 0 {
					t.Fatalf("the interrupted core exited with %d\n%s%s", code, out, errOut)
				}
				// The run is in flight, and its container is there.
				s := f.open(t, c)
				inFlight, err := s.InFlight(1)
				s.Close()
				if err != nil || inFlight == nil || inFlight.ID != c.run {
					t.Errorf("the run in flight is %+v (%v), want %s", inFlight, err, c.run)
				}
				if got := containerState(t, c.run); got == "" {
					t.Error("the interrupt removed the run's container")
				}

				core, done := f.core(t, c, "")
				f.waitEnded(t, c)
				f.stop(t, core, done)
				f.assertEnded(t, c, store.Finished)
			})
		}
	})

	t.Run("a vanished run", func(t *testing.T) {
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Add a line to NOTES.md. "+gateWord)
		core, done := f.core(t, c, "")
		f.waitAtGate(t, c)
		core.Process.Signal(syscall.SIGKILL)
		<-done
		must(t, "rm", "--force", "kiln-run-"+c.run)

		core, done = f.core(t, c, "")
		f.waitEnded(t, c)
		f.stop(t, core, done)
		e := f.assertEnded(t, c, store.Failed)
		if !strings.Contains(e.Reason, "container is gone") {
			t.Errorf("the reason is %q, want that the container is gone", e.Reason)
		}
	})

	t.Run("a run that fails", func(t *testing.T) {
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Add a line to NOTES.md. "+failWord)
		core, done := f.core(t, c, "")
		f.waitEnded(t, c)
		f.stop(t, core, done)
		e := f.assertEnded(t, c, store.Failed)
		if !strings.Contains(e.Reason, "exit 1") || !strings.Contains(e.Reason, "no result") {
			t.Errorf("the reason is %q, want exit 1 and no result", e.Reason)
		}
	})

	t.Run("what the sweep removes, and what it leaves", func(t *testing.T) {
		c := f.newCase(t, f.standIn)
		other := randomID(t)
		// Each container is kiln-run-<its kiln.run, or the ID it would have>.
		mine, theirs, byHand, unnamed := c.instance+"-9-1", other+"-9-1", c.instance+"-8-1", c.instance+"-7-1"
		for id, labels := range map[string][]string{
			mine:    {"kiln.run=" + mine, "kiln.instance=" + c.instance},
			theirs:  {"kiln.run=" + theirs, "kiln.instance=" + other},
			byHand:  {"kiln.run=" + byHand},
			unnamed: {"kiln.instance=" + c.instance},
		} {
			args := []string{"create", "--name", "kiln-run-" + id, "--network", "none"}
			for _, l := range labels {
				args = append(args, "--label", l)
			}
			t.Cleanup(func() { dockerQuiet("rm", "--force", "kiln-run-"+id) })
			must(t, append(args, f.proxyImage)...)
		}
		t.Cleanup(func() { dockerQuiet("network", "rm", "kiln-run-"+mine) })
		must(t, "network", "create", "--internal", "--label", "kiln.run="+mine, "--label", "kiln.instance="+c.instance, "kiln-run-"+mine)
		before := map[string]string{}
		for _, id := range []string{theirs, byHand, unnamed} {
			before[id] = must(t, "inspect", "--format", "{{.Id}} {{.State.Status}}", "kiln-run-"+id)
		}

		// One whole tick, after which the core exits.
		_, done := f.core(t, c, "ticked")
		if code := <-done; code != exitAtPoint {
			t.Fatalf("the core exited with %d, want %d after its first tick", code, exitAtPoint)
		}
		if _, err := docker(t.Context(), "container", "inspect", "kiln-run-"+mine); err == nil {
			t.Error("the container of this instance with no run is left")
		}
		if _, err := docker(t.Context(), "network", "inspect", "kiln-run-"+mine); err == nil {
			t.Error("the network of this instance with no run is left")
		}
		for id, was := range before {
			got, err := docker(t.Context(), "inspect", "--format", "{{.Id}} {{.State.Status}}", "kiln-run-"+id)
			if err != nil || got != was {
				t.Errorf("kiln-run-%s was touched: %q (%v), was %q", id, got, err, was)
			}
		}
	})

	t.Run("a second core or a kiln card while one runs", func(t *testing.T) {
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Add a line to NOTES.md. "+gateWord)
		core, done := f.core(t, c, "")
		f.waitAtGate(t, c)
		before := f.snapshot(t, c)

		for _, ch := range []child{
			{Command: "card", Config: Config{Data: c.data}, Text: "A second card."},
			{Command: "core", Config: f.config(c)},
		} {
			// A refusal comes at once; one that is not refused is killed.
			cmd, done := f.start(t, ch)
			select {
			case code := <-done:
				if errOut := cmd.Stderr.(*bytes.Buffer).String(); code == 0 || !strings.Contains(errOut, "another core holds the lock") {
					t.Errorf("kiln %s: exit status %d\n%s", ch.Command, code, errOut)
				}
			case <-time.After(10 * time.Second):
				cmd.Process.Signal(syscall.SIGKILL)
				<-done
				t.Errorf("kiln %s was not refused: it still ran after 10 s", ch.Command)
			}
		}
		if got := f.snapshot(t, c); got != before {
			t.Errorf("the store changed:\n%s\nwas\n%s", got, before)
		}
		if t.Failed() {
			t.FailNow()
		}

		must(t, "exec", "kiln-run-"+c.run, "touch", "/tmp/gate")
		f.waitEnded(t, c)
		f.stop(t, core, done)
		f.assertEnded(t, c, store.Finished)
	})

	t.Run("one run of the real Claude Code", func(t *testing.T) {
		if f.standInOutput == nil {
			t.Fatal("needs the stand-in's output of the first case")
		}
		c := f.newCase(t, f.runImage)
		f.card(t, c, "Run sh run.sh and report its result. kiln-case-tick-real")
		core, done := f.core(t, c, "")
		f.waitEnded(t, c)
		f.stop(t, core, done)
		f.assertEnded(t, c, store.Finished)

		// The card's text reached Claude Code as the instruction.
		reached := false
		for _, line := range strings.Split(must(t, "logs", f.upstream), "\n") {
			var r map[string]any
			if json.Unmarshal([]byte(line), &r) == nil && r["kind"] == "request" && r["case"] == "kiln-case-tick-real" {
				reached = true
			}
		}
		if !reached {
			t.Error("no request to the upstream carried the card's text")
		}

		// Every event the stand-in prints has a counterpart in the real output,
		// with every field, of the same JSON type.
		real := readEvents(t, readFile(t, filepath.Join(c.data, "runs", c.run, "output.jsonl")))
		for _, stand := range readEvents(t, f.standInOutput) {
			kind := fmt.Sprint(stand["type"], "/", stand["subtype"])
			var counterpart map[string]any
			for _, e := range real {
				if fmt.Sprint(e["type"], "/", e["subtype"]) == kind {
					counterpart = e
				}
			}
			if counterpart == nil {
				t.Errorf("the stand-in prints %s, which the real Claude Code does not", kind)
				continue
			}
			for field, v := range stand {
				got, ok := counterpart[field]
				if !ok {
					t.Errorf("the stand-in's %s has %s, which the real event lacks", kind, field)
				} else if jsonType(got) != jsonType(v) {
					t.Errorf("the stand-in's %s has %s as %s, the real event as %s", kind, field, jsonType(v), jsonType(got))
				}
			}
		}
	})
}

// testCase is one case: its data directory, instance, repo and image, and the
// ID of the run of its card 1.
type testCase struct {
	data     string
	instance string
	repo     string
	commit   string
	image    string
	run      string
	starts   *lines // the IDs of the instance's containers, once per start
}

// newCase makes a case's data directory with its store, and its repo, and
// removes whatever of its instance is left on the engine afterwards.
func (f *fixture) newCase(t *testing.T, image string) *testCase {
	t.Helper()
	c := &testCase{data: filepath.Join(t.TempDir(), "data"), image: image}
	s := f.open(t, c)
	var err error
	c.instance, err = s.Instance()
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	c.run = c.instance + "-1-1"
	t.Cleanup(func() { removeInstance(f.proxy.Name, c.instance) })
	c.starts = watchStarts(t, c.instance)
	c.repo, c.commit = bareRepo(t, map[string]string{
		"CLAUDE.md": "# Try\n\nThe marker is " + f.marker + ".\n",
		"NOTES.md":  "# Notes\n",
		"run.sh":    "set -eu\nprintf 'A line from the run.\\n' >> NOTES.md\necho checked\n",
	})
	return c
}

// config is the configuration of a case's core.
func (f *fixture) config(c *testCase) Config {
	return Config{Data: c.data, Repo: c.repo, Model: model, Effort: effort, Image: c.image, Proxy: f.proxy.Name}
}

// open opens a case's store. A core may be writing it, so a busy store is
// tried again.
func (f *fixture) open(t *testing.T, c *testCase) *store.Store {
	t.Helper()
	if err := os.MkdirAll(c.data, 0o700); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(waitLimit)
	for {
		s, err := store.Open(filepath.Join(c.data, "store.db"))
		if err == nil {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// card writes a case's card 1 with `kiln card`.
func (f *fixture) card(t *testing.T, c *testCase, text string) {
	t.Helper()
	code, out, errOut := f.kiln(t, child{Command: "card", Config: Config{Data: c.data}, Text: text})
	if code != 0 || strings.TrimSpace(out) != "1" {
		t.Fatalf("kiln card: exit status %d\n%s\n%s", code, out, errOut)
	}
}

// start starts the test binary as a child, and returns the command and a
// channel that gets its exit status.
func (f *fixture) start(t *testing.T, ch child) (*exec.Cmd, <-chan int) {
	t.Helper()
	spec, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(), asKilnVar+"="+string(spec))
	cmd.Stdout, cmd.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		defer cancel()
		err := cmd.Wait()
		var exit *exec.ExitError
		switch {
		case err == nil:
			done <- 0
		case errors.As(err, &exit):
			done <- exit.ExitCode()
		default:
			done <- -1
		}
	}()
	// A core that is still running when the case ends is killed; its output is
	// logged.
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGKILL)
		t.Logf("%s:\n%s%s", ch.Command, cmd.Stdout, cmd.Stderr)
	})
	return cmd, done
}

// kiln runs the test binary as a child to its end, and returns its exit status
// and what it printed.
func (f *fixture) kiln(t *testing.T, ch child) (int, string, string) {
	t.Helper()
	cmd, done := f.start(t, ch)
	code := <-done
	return code, cmd.Stdout.(*bytes.Buffer).String(), cmd.Stderr.(*bytes.Buffer).String()
}

// core starts a case's core, which exits at the point exitAt, if one is given.
func (f *fixture) core(t *testing.T, c *testCase, exitAt string) (*exec.Cmd, <-chan int) {
	t.Helper()
	return f.start(t, child{Command: "core", Config: f.config(c), ExitAt: exitAt})
}

// stop ends a core with SIGINT, which it answers with exit status 0.
func (f *fixture) stop(t *testing.T, core *exec.Cmd, done <-chan int) {
	t.Helper()
	core.Process.Signal(os.Interrupt)
	if code := <-done; code != 0 {
		t.Errorf("the core exited with %d on SIGINT\n%s%s", code, core.Stdout, core.Stderr)
	}
}

// waitAtGate waits until the run of a case's card 1 waits at its gate.
func (f *fixture) waitAtGate(t *testing.T, c *testCase) {
	t.Helper()
	waitUntil(t, "the run waits at its gate", func() bool {
		_, err := docker(t.Context(), "exec", "kiln-run-"+c.run, "test", "-e", "/tmp/waiting")
		return err == nil
	})
}

// waitEnded waits until card 1 waits no longer for a run and nothing of the
// instance is left on the engine.
func (f *fixture) waitEnded(t *testing.T, c *testCase) {
	t.Helper()
	waitUntil(t, "the card's run has ended and is removed", func() bool {
		if n := c.starts.count(); n > 1 {
			t.Fatalf("the instance's containers started %d times, want 1", n)
		}
		s := f.open(t, c)
		card, err := s.Card(1)
		s.Close()
		return err == nil && card.WaitsFor != store.Run && instanceLeft(t, c.instance) == ""
	})
}

// assertEnded asserts the end state of a case with one run of card 1, ended
// as given, and returns the entry that ended it.
func (f *fixture) assertEnded(t *testing.T, c *testCase, ending store.Ending) store.Entry {
	t.Helper()
	s := f.open(t, c)
	defer s.Close()
	want := map[store.Ending]store.WaitsFor{store.Finished: store.Approval, store.Failed: store.Developer}[ending]
	card, err := s.Card(1)
	if err != nil {
		t.Fatal(err)
	}
	if card.Stage != store.Spec || card.WaitsFor != want {
		t.Errorf("the card is at %s waiting for %s, want spec waiting for %s", card.Stage, card.WaitsFor, want)
	}
	entries, err := s.History(1)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, fmt.Sprintf("%s/%s by %s", e.Stage, e.WaitsFor, e.Cause))
	}
	wantEntries := []string{"backlog/developer by developer", "spec/run by developer", "spec/" + string(want) + " by run"}
	if strings.Join(got, ", ") != strings.Join(wantEntries, ", ") {
		t.Fatalf("the entries are %q, want %q", got, wantEntries)
	}
	last := entries[2]
	if last.Run != c.run || last.Reason == "" {
		t.Errorf("the last entry names the run %q with the reason %q, want %s and a reason", last.Run, last.Reason, c.run)
	}
	if n, err := s.RunCount(1); err != nil || n != 1 {
		t.Errorf("card 1 has %d runs (%v), want 1", n, err)
	}
	if r, err := s.Run(c.run); err != nil || r.Ending != ending || r.Commit != c.commit || r.Model != model || r.Effort != effort {
		t.Errorf("the run is %+v (%v), want %s from %s", r, err, ending, c.commit)
	}

	if ending == store.Finished {
		// kiln/card-1 holds the save, one commit on top of the start.
		commits := strings.Fields(gitIn(t, c.repo, "rev-list", c.commit+"..refs/heads/kiln/card-1"))
		if len(commits) != 1 {
			t.Fatalf("kiln/card-1 holds %d commits on top of the start, want 1", len(commits))
		}
		if got := gitIn(t, c.repo, "show", commits[0]+":NOTES.md"); got != "# Notes\nA line from the run." {
			t.Errorf("NOTES.md of the save is %q", got)
		}
		if !strings.Contains(last.Reason, "saved "+commits[0]) {
			t.Errorf("the reason %q does not name the save %s", last.Reason, commits[0])
		}
	}

	if left := instanceLeft(t, c.instance); left != "" {
		t.Errorf("left of the instance: %s", left)
	}
	if n := c.starts.count(); n != 1 {
		t.Errorf("the instance's containers started %d times, want 1", n)
	}
	return last
}

// snapshot is what a case's store holds of its cards and runs, as text.
func (f *fixture) snapshot(t *testing.T, c *testCase) string {
	t.Helper()
	s := f.open(t, c)
	defer s.Close()
	var b strings.Builder
	waiting, err := s.Waiting()
	fmt.Fprintf(&b, "waiting %v %v\n", waiting, err)
	for id := int64(1); id <= 2; id++ {
		card, err := s.Card(id)
		fmt.Fprintf(&b, "card %+v %v\n", card, err)
		entries, err := s.History(id)
		fmt.Fprintf(&b, "entries %d %v\n", len(entries), err)
		n, err := s.RunCount(id)
		fmt.Fprintf(&b, "runs %d %v\n", n, err)
	}
	return b.String()
}

// containerState is the state of a run's container, or "" when there is none.
func containerState(t *testing.T, id string) string {
	t.Helper()
	out, err := docker(t.Context(), "container", "inspect", "--format", "{{.State.Status}}", "kiln-run-"+id)
	if err != nil {
		return ""
	}
	return out
}

// instanceLeft lists the containers and networks of an instance, empty when
// there is none.
func instanceLeft(t *testing.T, instance string) string {
	t.Helper()
	var left []string
	for _, what := range [][]string{{"ps", "--all", "--format", "{{.Names}}"}, {"network", "ls", "--format", "{{.Name}}"}} {
		out, err := docker(t.Context(), append(what, "--filter", "label=kiln.instance="+instance)...)
		if err != nil {
			t.Fatal(err)
		}
		left = append(left, strings.Fields(out)...)
	}
	return strings.Join(left, " ")
}

// lines collects what a command writes, line by line, as it comes.
type lines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lines) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(strings.Fields(l.buf.String()))
}

// watchStarts follows the engine's events from now on, and collects the
// starts of an instance's containers. The events are followed as they come:
// the engine keeps only the last few hundred for a later --since, and other
// work on it can push a start out of them.
func watchStarts(t *testing.T, instance string) *lines {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := &lines{}
	cmd := exec.CommandContext(ctx, "docker", "events", "--since", strconv.FormatInt(time.Now().Add(-5*time.Second).Unix(), 10),
		"--filter", "type=container", "--filter", "label=kiln.instance="+instance, "--filter", "event=start",
		"--format", "{{.Actor.ID}}")
	cmd.Stdout = l
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	return l
}

// removeInstance removes whatever of an instance is left on the engine, for a
// cleanup.
func removeInstance(proxy, instance string) {
	filter := "label=kiln.instance=" + instance
	if out, err := exec.Command("docker", "ps", "--all", "--quiet", "--filter", filter).Output(); err == nil {
		for _, id := range strings.Fields(string(out)) {
			dockerQuiet("rm", "--force", id)
		}
	}
	if out, err := exec.Command("docker", "network", "ls", "--quiet", "--filter", filter).Output(); err == nil {
		for _, id := range strings.Fields(string(out)) {
			dockerQuiet("network", "disconnect", "--force", id, proxy)
			dockerQuiet("network", "rm", id)
		}
	}
}

// waitUntil waits until cond holds, and fails the test after waitLimit.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for this in vain: %s", waitLimit, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// readEvents reads the events of an output.jsonl.
func readEvents(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil {
			events = append(events, e)
		}
	}
	if len(events) == 0 {
		t.Fatalf("no event in %q", data)
	}
	return events
}

// jsonType is the JSON type of a decoded value.
func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// gitIn runs git in a directory, or on a bare repo, for a test, with a fixed
// author and dates, and returns its output, trimmed.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@localhost", "GIT_AUTHOR_DATE=2026-11-18T10:00:00Z",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@localhost", "GIT_COMMITTER_DATE=2026-11-18T10:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// bareRepo makes a commit of the given files in a fresh repo, and returns a
// bare copy of it and the commit's ID.
func bareRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	gitIn(t, dir, "init", "--quiet", "--initial-branch=main", work)
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(work, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, work, "add", "--all")
	gitIn(t, work, "commit", "--quiet", "--message", "start")
	bare := filepath.Join(dir, "copy.git")
	gitIn(t, dir, "clone", "--quiet", "--bare", work, bare)
	return bare, gitIn(t, work, "rev-parse", "HEAD")
}

// assertNothingLeft asserts that no container, network or image whose name
// holds the test's own word is left on the engine.
func assertNothingLeft(t *testing.T, word string) {
	t.Helper()
	ctx := context.Background()
	for _, what := range [][]string{
		{"ps", "--all", "--format", "{{.Names}}"},
		{"network", "ls", "--format", "{{.Name}}"},
		{"images", "--format", "{{.Repository}}"},
	} {
		out, err := docker(ctx, what...)
		if err != nil {
			t.Error(err)
			continue
		}
		for _, n := range strings.Fields(out) {
			if strings.Contains(n, word) {
				t.Errorf("left on the engine: %s", n)
			}
		}
	}
}

func randomID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// docker runs the docker command and returns its output, trimmed. A failure
// carries what it printed to standard error.
func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// dockerQuiet runs the docker command for a cleanup, where what is already gone
// is fine.
func dockerQuiet(args ...string) {
	exec.Command("docker", args...).Run()
}

func must(t *testing.T, args ...string) string {
	t.Helper()
	out, err := docker(t.Context(), args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// build builds an image from a context, relative to the package, with a
// Dockerfile relative to the repo's root, and removes it afterwards.
func build(t *testing.T, tag, context, dockerfile string, args ...string) {
	t.Helper()
	t.Cleanup(func() { dockerQuiet("rmi", tag) })
	must(t, append(append([]string{"build", "--quiet", "--tag", tag, "--file", "../../" + dockerfile}, args...), context)...)
}
