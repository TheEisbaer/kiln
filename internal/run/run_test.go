package run

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/seal"
)

// The variables by which the test runs its own binary as `kiln run`. Only
// TestMain reads them.
const (
	asKilnVar = "KILN_RUN_TEST_AS_KILN" // the inputs, as JSON
	afterVar  = "KILN_RUN_TEST_AFTER"   // <step>:exit or <step>:fail
)

// exitAfterStep is the status with which the binary exits after a step, as a
// kill there would end it.
const exitAfterStep = 3

func TestMain(m *testing.M) {
	if inputs := os.Getenv(asKilnVar); inputs != "" {
		os.Exit(asKiln(inputs, os.Getenv(afterVar)))
	}
	os.Exit(m.Run())
}

// asKiln is `kiln run` with the given inputs, and afterStep set to exit or fail
// after the named step.
func asKiln(inputs, afterSpec string) int {
	var in Inputs
	if err := json.Unmarshal([]byte(inputs), &in); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if step, how, ok := strings.Cut(afterSpec, ":"); ok {
		afterStep = func(s string) error {
			if s != step {
				return nil
			}
			if how == "exit" {
				os.Exit(exitAfterStep)
			}
			return fmt.Errorf("failed by the test after the step %s", s)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return Do(ctx, in, os.Stdout, os.Stderr)
}

// The steps, by the names afterStep is called with.
var steps = []string{"create", "place", "start", "follow", "collect", "save", "record", "remove"}

// upstreamPort is where testdata/claude listens.
const upstreamPort = "8081"

// The model and effort of the test's runs.
const (
	model  = "claude-haiku-5-5"
	effort = "low"
)

// fixture is what the test's cases share.
type fixture struct {
	id         string
	repo       string // the instance's copy
	commit     string // the commit every run starts from
	other      string // another commit, on top of it
	runs       string
	runImage   string
	proxy      seal.Proxy
	proxyImage string
	credential string
	marker     string
	upstream   string
	dir        string
}

// TestSteps is a large test (ADR 0031, ADR 0048): it needs the container
// engine. It carries out runs of the real, pinned Claude Code against a
// scripted upstream, through `kiln run`'s steps, and kills and fails them at
// each step before it starts them again.
//
// It runs only with KILN_ENGINE_TESTS=1, and then fails, not skips, when the
// engine cannot be reached. Every name it uses carries a random id, and
// everything it creates is removed, even on failure.
func TestSteps(t *testing.T) {
	if os.Getenv("KILN_ENGINE_TESTS") != "1" {
		t.Skip("needs the container engine; set KILN_ENGINE_TESTS=1")
	}
	if _, err := docker(t.Context(), "info", "--format", "{{.ServerVersion}}"); err != nil {
		t.Fatalf("the container engine is not reachable: %v", err)
	}

	id := randomID(t)
	name := func(what string) string { return "kiln-steptest-" + id + "-" + what }
	// Registered first, so it runs after every other cleanup.
	t.Cleanup(func() { assertNothingLeft(t, "steptest-"+id) })
	f := &fixture{id: id, dir: t.TempDir(), credential: "fake-credential-" + id, marker: "kiln-marker-" + id}
	f.runs = filepath.Join(f.dir, "runs")

	// Images. The repo's root is the build context.
	f.proxyImage, f.runImage = name("proxy"), name("run")
	claudeImage := name("claude")
	build(t, f.proxyImage, "images/proxy/Dockerfile")
	build(t, claudeImage, "internal/run/testdata/claude/Dockerfile")
	build(t, f.runImage, "images/run/Dockerfile")

	// The proxy creates its ordinary network; the upstream joins it.
	ordinary := name("ordinary")
	t.Cleanup(func() { dockerQuiet("network", "rm", ordinary) })
	f.upstream = name("upstream")
	f.proxy = seal.Proxy{Name: name("proxy"), Image: f.proxyImage, Network: ordinary,
		Upstream: "http://" + f.upstream + ":" + upstreamPort}
	t.Cleanup(func() { dockerQuiet("rm", "--force", f.proxy.Name) })
	if err := seal.ReplaceProxy(t.Context(), f.proxy, "echo "+f.credential); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dockerQuiet("rm", "--force", f.upstream) })
	must(t, "run", "--detach", "--name", f.upstream, "--network", ordinary, claudeImage, f.marker)

	f.makeRepo(t)

	t.Run("happy path", func(t *testing.T) {
		c := f.newCase(t, "happy")
		code, out, errOut := f.kiln(t, c, "", true)
		if code != 0 {
			t.Fatalf("exit status %d\n%s\n%s", code, out, errOut)
		}
		for _, want := range []string{
			"model " + model + ", Claude Code 2.1.326",
			"tool Bash: Run the script",
			"text: Done.",
			"result success, 2 turns, $",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("following printed no line with %q:\n%s", want, out)
			}
		}
		f.assertDone(t, c)

		// Started again after it is done: a repeat, not a retry.
		if code, out, errOut := f.kiln(t, c, "", false); code != 0 || !strings.Contains(out, "recorded already") {
			t.Errorf("started again: exit status %d\n%s\n%s", code, out, errOut)
		}
		f.assertDone(t, c)

		t.Run("another commit, model or instruction under the same ID", func(t *testing.T) {
			record := readAll(t, filepath.Join(f.runs, c.id, "result.json"))
			other := filepath.Join(f.dir, "other-instruction.md")
			writeIn(t, f.dir, "other-instruction.md", "Something else. kiln-case-happy\n", 0o644)
			for what, edit := range map[string]func(*Inputs){
				"commit":      func(in *Inputs) { in.Commit = f.other },
				"model":       func(in *Inputs) { in.Model = "claude-sonnet-5-5" },
				"instruction": func(in *Inputs) { in.Instruction = other },
			} {
				d := c
				edit(&d.in)
				if code, _, errOut := f.kiln(t, d, "", false); code == 0 || !strings.Contains(errOut, "recorded with the "+what) {
					t.Errorf("with another %s: exit status %d\n%s", what, code, errOut)
				}
			}
			if got := readAll(t, filepath.Join(f.runs, c.id, "result.json")); got != record {
				t.Errorf("the record changed:\n%s", got)
			}
			f.assertDone(t, c)
		})
	})

	t.Run("killed, then started again", func(t *testing.T) {
		for _, step := range steps {
			t.Run("after "+step, func(t *testing.T) {
				c := f.newCase(t, "kill-"+step)
				if step == "follow" {
					// While the run waits at its gate, with the proxy replaced
					// before it is started again: the new proxy is on no earlier
					// run's network until it is attached.
					cmd, done := f.start(t, c, "", false)
					f.waitAtGate(t, c)
					cmd.Process.Kill()
					<-done
					if err := seal.ReplaceProxy(t.Context(), f.proxy, "echo "+f.credential); err != nil {
						t.Fatal(err)
					}
				} else if code, out, errOut := f.kiln(t, c, step+":exit", true); code != exitAfterStep {
					t.Fatalf("exit status %d, want %d\n%s\n%s", code, exitAfterStep, out, errOut)
				}
				if code, out, errOut := f.kiln(t, c, "", true); code != 0 {
					t.Fatalf("started again: exit status %d\n%s\n%s", code, out, errOut)
				}
				f.assertDone(t, c)
			})
		}
	})

	t.Run("failed, then started again", func(t *testing.T) {
		for i, step := range steps {
			t.Run("after "+step, func(t *testing.T) {
				c := f.newCase(t, "fail-"+step)
				code, out, errOut := f.kiln(t, c, step+":fail", true)
				if code == 0 || !strings.Contains(errOut, "failed by the test") {
					t.Fatalf("exit status %d, and no failure reported\n%s\n%s", code, out, errOut)
				}
				// Before the start, nothing is left; from the start to the record,
				// the container and network stay; after the removal, nothing is.
				kept := i >= 2 && i <= 6
				assertLeft(t, c.id, kept, kept)
				if code, out, errOut := f.kiln(t, c, "", true); code != 0 {
					t.Fatalf("started again: exit status %d\n%s\n%s", code, out, errOut)
				}
				f.assertDone(t, c)
			})
		}
	})

	t.Run("Ctrl-C ends the following only", func(t *testing.T) {
		c := f.newCase(t, "interrupt")
		cmd, done := f.start(t, c, "", false)
		f.waitAtGate(t, c)
		cmd.Process.Signal(os.Interrupt)
		code := <-done
		if code != exitInterrupted || !strings.Contains(cmd.Stdout.(*bytes.Buffer).String(), "the run goes on") {
			t.Errorf("exit status %d, want %d, and the note that the run goes on\n%s", code, exitInterrupted, cmd.Stdout)
		}
		if got := must(t, "inspect", "--format", "{{.State.Status}}", "kiln-run-"+c.id); got != "running" {
			t.Errorf("the run is %s, want running", got)
		}
		if code, out, errOut := f.kiln(t, c, "", true); code != 0 {
			t.Fatalf("started again: exit status %d\n%s\n%s", code, out, errOut)
		}
		f.assertDone(t, c)
	})

	t.Run("with no proxy", func(t *testing.T) {
		c := f.newCase(t, "no-proxy")
		c.in.Proxy = name("no-such-proxy")
		if code, _, errOut := f.kiln(t, c, "", false); code == 0 {
			t.Errorf("exit status 0\n%s", errOut)
		}
		assertLeft(t, c.id, false, false)
	})

	t.Run("with no run image", func(t *testing.T) {
		c := f.newCase(t, "no-image")
		c.in.Image = name("no-such-image")
		code, _, errOut := f.kiln(t, c, "", false)
		if code == 0 || !strings.Contains(errOut, "No such image") {
			t.Errorf("want the image missing locally: exit status %d\n%s", code, errOut)
		}
		assertLeft(t, c.id, false, false)
		if _, err := docker(t.Context(), "image", "inspect", c.in.Image); err == nil {
			dockerQuiet("rmi", c.in.Image)
			t.Error("the image was pulled")
		}
	})

	t.Run("a branch at another commit", func(t *testing.T) {
		c := f.newCase(t, "other-branch")
		gitIn(t, f.repo, "update-ref", "refs/heads/"+c.in.Branch, f.other)
		if code, _, errOut := f.kiln(t, c, "", false); code == 0 || !strings.Contains(errOut, "neither absent nor at the commit") {
			t.Errorf("exit status %d\n%s", code, errOut)
		}
		assertLeft(t, c.id, false, false)
		f.assertNoRecord(t, c)
		if at := gitIn(t, f.repo, "rev-parse", "refs/heads/"+c.in.Branch); at != f.other {
			t.Errorf("the branch is at %s, was %s", at, f.other)
		}
	})

	t.Run("a branch moved while the run waits", func(t *testing.T) {
		c := f.newCase(t, "moved")
		cmd, done := f.start(t, c, "", false)
		f.waitAtGate(t, c)
		ref := "refs/heads/" + c.in.Branch
		gitIn(t, f.repo, "update-ref", ref, f.other, "")
		must(t, "exec", "kiln-run-"+c.id, "touch", "/tmp/gate")
		if code := <-done; code == 0 || !strings.Contains(cmd.Stderr.(*bytes.Buffer).String(), "has moved") {
			t.Fatalf("exit status %d\n%s", code, cmd.Stderr)
		}
		assertLeft(t, c.id, true, true)
		if got := must(t, "inspect", "--format", "{{.State.Status}}", "kiln-run-"+c.id); got != "exited" {
			t.Errorf("the run's container is %s, want exited", got)
		}
		assertNoSave(t, c.id)
		if at := gitIn(t, f.repo, "rev-parse", ref); at != f.other {
			t.Errorf("the branch is at %s, was moved to %s", at, f.other)
		}

		// The container's labels name the inputs: another model is refused.
		d := c
		d.in.Model = "claude-sonnet-5-5"
		gitIn(t, f.repo, "update-ref", "-d", ref)
		if code, _, errOut := f.kiln(t, d, "", false); code == 0 || !strings.Contains(errOut, "other inputs") {
			t.Errorf("with another model: exit status %d\n%s", code, errOut)
		}
		assertLeft(t, c.id, true, true)

		if code, out, errOut := f.kiln(t, c, "", true); code != 0 {
			t.Fatalf("after the reset: exit status %d\n%s\n%s", code, out, errOut)
		}
		f.assertDone(t, c)
	})

	t.Run("what Kiln did not make", func(t *testing.T) {
		t.Run("a container without kiln.run", func(t *testing.T) {
			c := f.newCase(t, "foreign-run")
			must(t, "create", "--name", "kiln-run-"+c.id, "--network", "none", f.proxyImage)
			before := must(t, "inspect", "--format", "{{.Id}} {{.State.Status}}", "kiln-run-"+c.id)
			if code, _, errOut := f.kiln(t, c, "", false); code == 0 || !strings.Contains(errOut, "no label kiln.run") {
				t.Errorf("exit status %d\n%s", code, errOut)
			}
			if got := must(t, "inspect", "--format", "{{.Id}} {{.State.Status}}", "kiln-run-"+c.id); got != before {
				t.Errorf("the container was touched: %s, was %s", got, before)
			}
			assertLeft(t, c.id, true, false)
			f.assertNoRecord(t, c)
		})

		t.Run("a network without kiln.run", func(t *testing.T) {
			c := f.newCase(t, "foreign-net")
			must(t, "network", "create", "--internal", "kiln-run-"+c.id)
			before := must(t, "network", "inspect", "--format", "{{.Id}}", "kiln-run-"+c.id)
			if code, _, errOut := f.kiln(t, c, "", false); code == 0 || !strings.Contains(errOut, "no label kiln.run") {
				t.Errorf("exit status %d\n%s", code, errOut)
			}
			if got := must(t, "network", "inspect", "--format", "{{.Id}}", "kiln-run-"+c.id); got != before {
				t.Errorf("the network was touched: %s, was %s", got, before)
			}
			assertLeft(t, c.id, false, true)
			f.assertNoRecord(t, c)
		})

		t.Run("a save container without kiln.run", func(t *testing.T) {
			c := f.newCase(t, "foreign-save")
			save := "kiln-save-" + c.id
			must(t, "create", "--name", save, "--network", "none", f.proxyImage)
			before := must(t, "inspect", "--format", "{{.Id}} {{.State.Status}}", save)
			if code, _, errOut := f.kiln(t, c, "", true); code == 0 || !strings.Contains(errOut, "no label kiln.run") {
				t.Errorf("exit status %d\n%s", code, errOut)
			}
			if got := must(t, "inspect", "--format", "{{.Id}} {{.State.Status}}", save); got != before {
				t.Errorf("the save container was touched: %s, was %s", got, before)
			}
			// Refused at the save, so the run and its .partial are kept.
			assertLeft(t, c.id, true, true)
			if _, err := os.Stat(filepath.Join(f.runs, c.id)); err == nil {
				t.Errorf("there is a record of %s", c.id)
			}
			must(t, "rm", save)
			if code, out, errOut := f.kiln(t, c, "", true); code != 0 {
				t.Fatalf("without it: exit status %d\n%s\n%s", code, out, errOut)
			}
			f.assertDone(t, c)
		})

		t.Run("a record without a readable result.json", func(t *testing.T) {
			c := f.newCase(t, "foreign-record")
			writeIn(t, filepath.Join(f.runs, c.id), "result.json", "not json", 0o644)
			if code, _, errOut := f.kiln(t, c, "", false); code == 0 || !strings.Contains(errOut, "no readable result.json") {
				t.Errorf("exit status %d\n%s", code, errOut)
			}
			if got := readAll(t, filepath.Join(f.runs, c.id, "result.json")); got != "not json" {
				t.Errorf("the record was touched: %q", got)
			}
			assertLeft(t, c.id, false, false)
		})
	})

	t.Run("the proxy is on no run network", func(t *testing.T) {
		got := must(t, "inspect", "--format", "{{json .NetworkSettings.Networks}}", f.proxy.Name)
		var networks map[string]json.RawMessage
		if err := json.Unmarshal([]byte(got), &networks); err != nil {
			t.Fatal(err)
		}
		if len(networks) != 1 || networks[ordinary] == nil {
			t.Errorf("the proxy is on %s, want only %s", got, ordinary)
		}
	})
}

// makeRepo makes the instance's copy: a bare repo with one commit, and another
// commit on top of it.
func (f *fixture) makeRepo(t *testing.T) {
	t.Helper()
	f.repo, f.commit = bareRepo(t, map[string]string{
		"go.mod":            "module example.com/try\n\ngo 1.27\n",
		"main.go":           "package main\n\nfunc main() {}\n",
		"CLAUDE.md":         "# Try\n\nThe marker is " + f.marker + ".\n",
		".gitignore":        "build/\n",
		"build/tracked.txt": "tracked under an ignored path\n",
		".gitattributes":    "kept.md export-ignore\n",
		"kept.md":           "kept, though export-ignore\n",
		"NOTES.md":          "# Notes\n",
		"run.sh": "set -eu\n" +
			"printf 'A line from the run.\\n' >> NOTES.md\n" +
			"printf 'ignored\\n' > build/new.txt\n" +
			"printf 'A note from the run.\\n' > /kiln/out/note.md\n" +
			"touch /tmp/waiting\n" +
			"while [ ! -e /tmp/gate ]; do sleep 0.1; done\n" +
			"go vet ./...\n" +
			"echo checked\n",
	})
	tree := gitIn(t, f.repo, "rev-parse", f.commit+"^{tree}")
	f.other = gitIn(t, f.repo, "commit-tree", "-p", f.commit, "-m", "other", tree)
}

// testCase is one run of the test, by its case name.
type testCase struct {
	name string
	id   string
	in   Inputs
}

// newCase makes the inputs of one case, and removes what its run leaves on the
// engine afterwards.
func (f *fixture) newCase(t *testing.T, name string) testCase {
	t.Helper()
	id := "steptest-" + f.id + "-" + name
	if len(id) > 40 {
		t.Fatalf("the run ID %s is too long", id)
	}
	instruction := filepath.Join(f.dir, name+".md")
	writeIn(t, f.dir, name+".md", "Run sh run.sh and report its result. kiln-case-"+name+"\n", 0o644)
	t.Cleanup(func() {
		dockerQuiet("rm", "--force", "kiln-save-"+id, "kiln-run-"+id)
		dockerQuiet("network", "disconnect", "--force", "kiln-run-"+id, f.proxy.Name)
		dockerQuiet("network", "rm", "kiln-run-"+id)
	})
	return testCase{name: name, id: id, in: Inputs{
		Repo: f.repo, Commit: f.commit, Branch: "try/" + name, ID: id, Instruction: instruction,
		Runs: f.runs, Image: f.runImage, Proxy: f.proxy.Name, Model: model, Effort: effort,
	}}
}

// start starts the test binary as `kiln run` for a case, with afterStep set by
// after, and returns the command and a channel that gets its exit status. With
// gate, it opens the run's gate whenever the run waits at it.
func (f *fixture) start(t *testing.T, c testCase, after string, gate bool) (*exec.Cmd, <-chan int) {
	t.Helper()
	inputs, err := json.Marshal(c.in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(), asKilnVar+"="+string(inputs), afterVar+"="+after)
	cmd.Stdout, cmd.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	exited := make(chan struct{})
	if gate {
		go func() {
			for {
				select {
				case <-exited:
					return
				case <-time.After(200 * time.Millisecond):
				}
				if _, err := docker(ctx, "exec", "kiln-run-"+c.id, "test", "-e", "/tmp/waiting"); err == nil {
					docker(ctx, "exec", "kiln-run-"+c.id, "touch", "/tmp/gate")
				}
			}
		}()
	}
	done := make(chan int, 1)
	go func() {
		defer cancel()
		err := cmd.Wait()
		close(exited)
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
	return cmd, done
}

// kiln runs the test binary as `kiln run` for a case to its end, and returns
// its exit status and what it printed.
func (f *fixture) kiln(t *testing.T, c testCase, after string, gate bool) (int, string, string) {
	t.Helper()
	cmd, done := f.start(t, c, after, gate)
	code := <-done
	return code, cmd.Stdout.(*bytes.Buffer).String(), cmd.Stderr.(*bytes.Buffer).String()
}

// waitAtGate waits until the run of a case waits at its gate.
func (f *fixture) waitAtGate(t *testing.T, c testCase) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := docker(t.Context(), "exec", "kiln-run-"+c.id, "test", "-e", "/tmp/waiting"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the run did not reach its gate")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// assertDone asserts what a case's run leaves when it is done.
func (f *fixture) assertDone(t *testing.T, c testCase) {
	t.Helper()
	ref := "refs/heads/" + c.in.Branch

	// The branch holds exactly one commit, the save, on top of the start.
	commits := strings.Fields(gitIn(t, f.repo, "rev-list", f.commit+".."+ref))
	if len(commits) != 1 {
		t.Fatalf("the branch holds %d commits on top of the start, want 1", len(commits))
	}
	save := commits[0]
	if got := gitIn(t, f.repo, "log", "-1", "--format=%P|%an <%ae>|%cn <%ce>|%s|%(trailers:key=Kiln-Run,valueonly)", save); got !=
		f.commit+"|Kiln <kiln@localhost>|Kiln <kiln@localhost>|chore: save run "+c.id+"|"+c.id {
		t.Errorf("the save is %q", got)
	}
	// Exactly the appended line: the ignored file is left out, and the
	// export-ignore and ignored-but-tracked files are kept.
	if got := gitIn(t, f.repo, "diff", "--numstat", f.commit, save); got != "1\t0\tNOTES.md" {
		t.Errorf("the save's change is %q, want one line added to NOTES.md", got)
	}
	if got := gitIn(t, f.repo, "show", save+":NOTES.md"); got != "# Notes\nA line from the run." {
		t.Errorf("NOTES.md is %q", got)
	}

	// The record.
	dir := filepath.Join(f.runs, c.id)
	f.assertNoPartial(t, c)
	for _, name := range []string{"output.jsonl", "stderr.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Error(err)
		}
	}
	if names := tarNames(t, filepath.Join(dir, "documents.tar")); !slices.Contains(names, "out/note.md") {
		t.Errorf("documents.tar holds %q, want out/note.md", names)
	}
	sessions := 0
	for _, n := range tarNames(t, filepath.Join(dir, "session.tar")) {
		if strings.HasSuffix(n, ".jsonl") {
			sessions++
		}
	}
	if sessions != 1 {
		t.Errorf("session.tar holds %d sessions, want 1", sessions)
	}
	var rec record
	if err := json.Unmarshal([]byte(readAll(t, filepath.Join(dir, "result.json"))), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Result == nil || rec.Result.Subtype != "success" || rec.ModelSeen != model || rec.Saved != save ||
		rec.Ending.ExitCode != 0 || rec.Inputs.Commit != f.commit || rec.Inputs.Effort != effort {
		t.Errorf("the record is %+v", rec)
	}

	// Nothing of the run is left.
	for _, what := range [][]string{{"ps", "--all"}, {"network", "ls"}} {
		if got := must(t, append(what, "--quiet", "--filter", "label=kiln.run="+c.id)...); got != "" {
			t.Errorf("docker %s with kiln.run=%s: %s", what[0], c.id, got)
		}
	}

	// The upstream saw one conversation: two requests, with the model, the
	// effort, the marker and the fake credential.
	logs := must(t, "logs", f.upstream)
	var requests []map[string]any
	for _, line := range strings.Split(logs, "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["case"] == "kiln-case-"+c.name {
			requests = append(requests, r)
		}
	}
	if len(requests) != 2 {
		t.Fatalf("the upstream saw %d requests of the case, want 2:\n%v", len(requests), requests)
	}
	for i, r := range requests {
		want := map[string]any{"method": "POST", "path": "/v1/messages", "model": model, "effort": effort,
			"x_api_key": f.credential, "marker": true, "tool_result": i == 1}
		for k, v := range want {
			if r[k] != v {
				t.Errorf("request %d: %s is %v, want %v", i+1, k, r[k], v)
			}
		}
	}
}

// assertNoRecord asserts that a case's run has no record, nor a partial one.
func (f *fixture) assertNoRecord(t *testing.T, c testCase) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(f.runs, c.id)); err == nil {
		t.Errorf("there is a record of %s", c.id)
	}
	f.assertNoPartial(t, c)
}

func (f *fixture) assertNoPartial(t *testing.T, c testCase) {
	t.Helper()
	if _, err := os.Stat(partialDir(f.runs, c.id)); err == nil {
		t.Errorf("there is a partial record of %s", c.id)
	}
}

// assertLeft asserts whether a run's container and network exist.
func assertLeft(t *testing.T, id string, container, network bool) {
	t.Helper()
	name := "kiln-run-" + id
	_, err := docker(t.Context(), "container", "inspect", name)
	if got := err == nil; got != container {
		t.Errorf("a container %s exists: %v, want %v", name, got, container)
	}
	_, err = docker(t.Context(), "network", "inspect", name)
	if got := err == nil; got != network {
		t.Errorf("a network %s exists: %v, want %v", name, got, network)
	}
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

// assertNoSave asserts that a run has no save container.
func assertNoSave(t *testing.T, id string) {
	t.Helper()
	if _, err := docker(t.Context(), "container", "inspect", "kiln-save-"+id); err == nil {
		t.Errorf("the save container kiln-save-%s is left", id)
	}
}

// tarNames lists the names in a tar file.
func tarNames(t *testing.T, name string) []string {
	t.Helper()
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var names []string
	tr := tar.NewReader(file)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

func readAll(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
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

// build builds an image with the repo's root as the context, and removes it
// afterwards.
func build(t *testing.T, tag, dockerfile string) {
	t.Helper()
	t.Cleanup(func() { dockerQuiet("rmi", tag) })
	must(t, "build", "--quiet", "--tag", tag, "--file", "../../"+dockerfile, "../..")
}
