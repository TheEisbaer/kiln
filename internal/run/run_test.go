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
	notifyStop()
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
	startTree  string // the tree of commit
	wantTree   string // the tree that the run's change gives, made without a baseline
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

	// The proxy and the upstream on the ordinary network, which the test
	// makes, so that KeepProxy finds it; TestRun of internal/seal has
	// KeepProxy make one.
	ordinary := name("ordinary")
	testNetwork(t, ordinary)
	f.upstream = name("upstream")
	f.proxy = seal.Proxy{Name: name("proxy"), Image: f.proxyImage, Network: ordinary,
		Upstream: "http://" + f.upstream + ":" + upstreamPort}
	t.Cleanup(func() { dockerQuiet("rm", "--force", f.proxy.Name) })
	if err := replaceProxy(t, f.proxy, f.credential); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dockerQuiet("rm", "--force", f.upstream) })
	must(t, "run", "--detach", "--name", f.upstream, "--network", ordinary, claudeImage, f.marker)

	f.makeRepo(t)

	// Every case has a run ID, a branch and an instruction of its own, and
	// the cases run side by side. The one that replaces a proxy has one of its
	// own.
	t.Run("cases", func(t *testing.T) {
		t.Run("happy path", func(t *testing.T) {
			t.Parallel()
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

			// The run's escape sequences reach the record, and not the terminal.
			if strings.ContainsAny(out+errOut, "\x1b\x07") {
				t.Errorf("the command printed a control character:\n%q\n%q", out, errOut)
			}
			if !strings.Contains(out, "text: Done.\uFFFD[2K") || !strings.Contains(errOut, "run stderr \uFFFD]0;kiln-title\uFFFD") {
				t.Errorf("the command printed\n%q\n%q", out, errOut)
			}
			dir := filepath.Join(f.runs, c.id)
			if got := readAll(t, filepath.Join(dir, "output.jsonl")); !strings.Contains(got, `Done.\u001b[2K`) {
				t.Error("output.jsonl does not hold the text's escape sequence")
			}
			if got := readAll(t, filepath.Join(dir, "stderr.txt")); !strings.Contains(got, "run stderr \x1b]0;kiln-title\x07") {
				t.Errorf("stderr.txt does not hold the escape sequence: %q", got)
			}
			// run.sh ran to its end, with the run image's toolchain.
			if got := toolResultsIn(t, filepath.Join(dir, "output.jsonl"))["toolu_kiln_1"]; !strings.Contains(got, "go version go1.") ||
				!strings.Contains(got, "checked") {
				t.Errorf("the Bash result is %q, want go's version and checked", got)
			}

			// The baseline, seen from inside the run: git works there.
			got := toolResultsIn(t, filepath.Join(dir, "output.jsonl"))["toolu_kiln_1"]
			for what, want := range map[string]string{
				"the start commit's tree": f.startTree,
				"the start's NOTES.md":    "# Notes\n",
				"git status":              " M NOTES.md\n",
				"git diff":                "1\t0\tNOTES.md",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("the Bash result has no %s (%q): %q", what, want, got)
				}
			}
			if strings.Contains(got, "build/") {
				t.Errorf("git status in the run shows an ignored path: %q", got)
			}

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
			t.Parallel()
			for _, step := range steps {
				t.Run("after "+step, func(t *testing.T) {
					t.Parallel()
					c := f.newCase(t, "kill-"+step)
					if step == "follow" {
						// While the run waits at its gate, with the proxy replaced
						// before it is started again: the new proxy is on no earlier
						// run's network until it is attached. The proxy is the
						// case's own, so that the other cases' runs keep theirs.
						proxy := f.proxy
						proxy.Name = name("proxy-" + c.name)
						t.Cleanup(func() { dockerQuiet("rm", "--force", proxy.Name) })
						if err := replaceProxy(t, proxy, f.credential); err != nil {
							t.Fatal(err)
						}
						c.in.Proxy = proxy.Name
						cmd, done := f.start(t, c, "", false)
						f.waitAtGate(t, c)
						cmd.Process.Kill()
						<-done
						if err := replaceProxy(t, proxy, f.credential); err != nil {
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
			t.Parallel()
			for i, step := range steps {
				t.Run("after "+step, func(t *testing.T) {
					t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
			c := f.newCase(t, "no-proxy")
			c.in.Proxy = name("no-such-proxy")
			if code, _, errOut := f.kiln(t, c, "", false); code == 0 {
				t.Errorf("exit status 0\n%s", errOut)
			}
			assertLeft(t, c.id, false, false)
		})

		t.Run("with no run image", func(t *testing.T) {
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
			t.Run("a container without kiln.run", func(t *testing.T) {
				t.Parallel()
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
				t.Parallel()
				c := f.newCase(t, "foreign-net")
				testNetwork(t, "kiln-run-"+c.id, "--internal")
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
				t.Parallel()
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
				t.Parallel()
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

		// A run that writes into its .git, deletes it or replaces it with a
		// symlink saves the same tree (assertDone).
		for _, name := range []string{"git-junk", "git-gone", "git-link"} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				c := f.newCase(t, name)
				if code, out, errOut := f.kiln(t, c, "", true); code != 0 {
					t.Fatalf("exit status %d\n%s\n%s", code, out, errOut)
				}
				f.assertDone(t, c)
			})
		}

		t.Run("a run with a list", func(t *testing.T) {
			t.Parallel()
			testWeb(t, f)
		})
		t.Run("laid over, killed, then started again", func(t *testing.T) {
			t.Parallel()
			testLaidOver(t, f)
		})
	})

	// The baseline only reads the instance's copy: it holds main and the cases'
	// branches, and nothing else.
	t.Run("the copy has no other ref", func(t *testing.T) {
		for _, ref := range strings.Fields(gitIn(t, f.repo, "for-each-ref", "--format=%(refname)")) {
			if ref != "refs/heads/main" && !strings.HasPrefix(ref, "refs/heads/try/") {
				t.Errorf("the copy has the ref %s", ref)
			}
		}
	})

	// After every case, whose runs attached the proxy to their networks.
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
			// What the baseline gives (item 2 of the spec), read by the happy path.
			"git rev-parse 'HEAD^{tree}'\n" +
			"git show HEAD:NOTES.md\n" +
			"git status --porcelain\n" +
			"git diff --numstat\n" +
			// What a run may do to its .git, which the save never reads.
			"case \"$(cat /kiln/instruction.md)\" in\n" +
			"*kiln-case-git-junk*)\n" +
			"  mkdir -p .git/info .git/hooks\n" +
			"  printf '*\\n' > .git/info/exclude\n" +
			"  printf 'NOTES.md filter=evil\\n' > .git/info/attributes\n" +
			"  printf '[filter \"evil\"]\\n\\tclean = echo EVIL\\n[core]\\n\\tworktree = /\\n' >> .git/config\n" +
			"  printf '#!/bin/sh\\nexit 1\\n' > .git/hooks/pre-commit\n" +
			"  chmod +x .git/hooks/pre-commit\n" +
			"  printf 'extra\\n' > .git/extra ;;\n" +
			"*kiln-case-git-gone*) rm -rf .git ;;\n" +
			"*kiln-case-git-link*) rm -rf .git && ln -s / .git ;;\n" +
			// What a laid-over run sees, read by the case "laid over".
			"*kiln-case-config*) cat CLAUDE.md && cat /kiln/card-config/CLAUDE.md ;;\n" +
			"esac\n" +
			"printf 'A note from the run.\\n' > /kiln/out/note.md\n" +
			// To the container's own standard error, which kiln run shows.
			"printf 'run stderr \\033]0;kiln-title\\007\\n' > /proc/1/fd/2\n" +
			"touch /tmp/waiting\n" +
			"while [ ! -e /tmp/gate ]; do sleep 0.1; done\n" +
			// The run image's toolchain, which the happy path asserts. Not go
			// vet, whose cold cache costs five seconds of CPU in every run.
			"go version\n" +
			"echo checked\n",
	})
	tree := gitIn(t, f.repo, "rev-parse", f.commit+"^{tree}")
	f.other = gitIn(t, f.repo, "commit-tree", "-p", f.commit, "-m", "other", tree)
	f.startTree = tree

	// The tree that the run's change gives with no baseline: a clone with the
	// line appended and everything added, as the save does.
	clone := filepath.Join(f.dir, "want")
	gitIn(t, f.dir, "clone", "--quiet", f.repo, clone)
	if err := os.WriteFile(filepath.Join(clone, "NOTES.md"), []byte("# Notes\nA line from the run.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "add", "--all", "--force")
	f.wantTree = gitIn(t, clone, "write-tree")
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
	failIfStopped(t)
	id := "steptest-" + f.id + "-" + name
	if len(id) > 40 {
		t.Fatalf("the run ID %s is too long", id)
	}
	instruction := filepath.Join(f.dir, name+".md")
	writeIn(t, f.dir, name+".md", "Run sh run.sh and report its result. kiln-case-"+name+"\n", 0o644)
	t.Cleanup(func() {
		dockerQuiet("rm", "--force", "kiln-save-"+id, "kiln-run-"+id, "kiln-wayout-"+id)
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
// gate, it opens the run's gate once the run waits at it.
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
				// The run waits at its gate once; an open gate stays open.
				if _, err := docker(ctx, "exec", "kiln-run-"+c.id, "test", "-e", "/tmp/waiting"); err == nil {
					if _, err := docker(ctx, "exec", "kiln-run-"+c.id, "touch", "/tmp/gate"); err == nil {
						return
					}
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

	// The same tree as without a baseline for the same change.
	if got := gitIn(t, f.repo, "rev-parse", save+"^{tree}"); got != f.wantTree {
		t.Errorf("the save's tree is %s, want %s, the tree of the change without a baseline", got, f.wantTree)
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
	data := readAll(t, filepath.Join(dir, "result.json"))
	if err := json.Unmarshal([]byte(data), &rec); err != nil {
		t.Fatal(err)
	}
	// A run without a list records no hosts and no way out.
	if !strings.Contains(data, `"hosts": []`) || strings.Contains(data, `"wayout"`) {
		t.Errorf("result.json of a run without a list:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "wayout.jsonl")); err == nil {
		t.Error("a run without a list has wayout.jsonl")
	}
	if rec.Result == nil || rec.Result.Subtype != "success" || rec.Result.SessionID == "" || rec.Result.TotalCostUSD <= 0 ||
		rec.ModelSeen != model || rec.Saved != save || rec.Ending.ExitCode != 0 || rec.Inputs.Commit != f.commit || rec.Inputs.Effort != effort {
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
	var lines []string // the case's lines of the upstream's log, both kinds
	for _, line := range strings.Split(logs, "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) != nil || r["case"] != "kiln-case-"+c.name {
			continue
		}
		lines = append(lines, line)
		if r["kind"] == "request" {
			requests = append(requests, r)
		}
	}
	explained := false
	explain := func() {
		if !explained {
			explained = true
			f.explain(t, c, lines)
		}
	}
	if len(requests) != 2 {
		explain()
		t.Fatalf("the upstream saw %d requests of the case, want 2", len(requests))
	}
	for i, r := range requests {
		want := map[string]any{"method": "POST", "path": "/v1/messages", "model": model, "effort": effort,
			"x_api_key": f.credential, "marker": true, "tool_result": i == 1}
		for k, v := range want {
			if r[k] != v {
				explain()
				t.Errorf("request %d: %s is %v, want %v", i+1, k, r[k], v)
			}
		}
	}
}

// explain prints what tells apart why a case's requests were not what the test
// expects: the upstream's lines of the case, with the time, the client's
// address, Claude Code's session, its SDK's retry count and the shape of each
// request, and how each answer went; the kinds of the events in the run's
// record, in order, and in full those that are not of the four ordinary kinds;
// the system entries of its session, which name the error behind a retry; the
// run's standard error; and the proxy's log.
func (f *fixture) explain(t *testing.T, c testCase, lines []string) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "the upstream's lines of kiln-case-%s:\n", c.name)
	for _, line := range lines {
		fmt.Fprintf(&b, "  %s\n", line)
	}

	dir := filepath.Join(f.runs, c.id)
	if output, err := os.ReadFile(filepath.Join(dir, "output.jsonl")); err != nil {
		fmt.Fprintf(&b, "output.jsonl: %v\n", err)
	} else {
		var kinds, unusual []string
		for _, line := range strings.Split(string(output), "\n") {
			var e struct{ Type, Subtype string }
			if line == "" || json.Unmarshal([]byte(line), &e) != nil {
				if line != "" {
					unusual = append(unusual, line)
				}
				continue
			}
			kind := e.Type
			if e.Type == "system" || e.Type == "result" {
				kind += "/" + e.Subtype
			}
			kinds = append(kinds, kind)
			switch {
			case kind == "system/init", e.Type == "assistant", e.Type == "user", e.Type == "result":
			default:
				unusual = append(unusual, line)
			}
		}
		fmt.Fprintf(&b, "the events of output.jsonl, in order: %s\n", strings.Join(kinds, ", "))
		for _, line := range unusual {
			if len(line) > 2000 {
				line = line[:2000] + "…"
			}
			fmt.Fprintf(&b, "  unusual: %s\n", line)
		}
	}
	// The session holds what output.jsonl does not: the error behind an
	// api_retry event, as a system entry of the kind api_error (read in a
	// session of Claude Code 2.1.326 whose connection was cut).
	if session, err := os.Open(filepath.Join(dir, "session.tar")); err != nil {
		fmt.Fprintf(&b, "session.tar: %v\n", err)
	} else {
		tr := tar.NewReader(session)
		for {
			h, err := tr.Next()
			if err != nil {
				if err != io.EOF {
					fmt.Fprintf(&b, "session.tar: %v\n", err)
				}
				break
			}
			if !strings.HasSuffix(h.Name, ".jsonl") {
				continue
			}
			data, _ := io.ReadAll(tr)
			for _, line := range strings.Split(string(data), "\n") {
				var e struct{ Type string }
				if json.Unmarshal([]byte(line), &e) == nil && e.Type == "system" {
					if len(line) > 2000 {
						line = line[:2000] + "…"
					}
					fmt.Fprintf(&b, "  session %s: %s\n", h.Name, line)
				}
			}
		}
		session.Close()
	}
	if stderr, err := os.ReadFile(filepath.Join(dir, "stderr.txt")); err != nil {
		fmt.Fprintf(&b, "stderr.txt: %v\n", err)
	} else {
		fmt.Fprintf(&b, "stderr.txt: %q\n", stderr)
	}
	// The proxy logs to its standard error.
	if proxy, err := exec.Command("docker", "logs", "--tail", "50", c.in.Proxy).CombinedOutput(); err != nil {
		fmt.Fprintf(&b, "the proxy's log: %v\n", err)
	} else {
		fmt.Fprintf(&b, "the proxy's log, its last 50 lines:\n%s\n", proxy)
	}
	t.Log(b.String())
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

// testNetwork creates a network of the test's own, which it removes
// afterwards, with an IPv4 subnet out of 10.231.0.0/16 chosen at random,
// tried again when another network has it. The engine's own pools have about
// thirty subnets for every network on it, and every run takes one of them
// (see check/in-image); the test's networks take none.
func testNetwork(t *testing.T, name string, args ...string) {
	t.Helper()
	t.Cleanup(func() { dockerQuiet("network", "rm", name) })
	for try := 0; ; try++ {
		b := make([]byte, 1)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		subnet := fmt.Sprintf("10.231.%d.0/24", b[0])
		_, err := docker(t.Context(), append(append([]string{"network", "create", "--subnet", subnet}, args...), name)...)
		if err == nil {
			return
		}
		if !strings.Contains(err.Error(), "overlaps") || try == 20 {
			t.Fatal(err)
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

// build builds an image with the repo's root as the context, and removes it
// afterwards.
func build(t *testing.T, tag, dockerfile string) {
	t.Helper()
	t.Cleanup(func() { dockerQuiet("rmi", tag) })
	must(t, "build", "--quiet", "--tag", tag, "--file", "../../"+dockerfile, "../..")
}

// replaceProxy starts the proxy, removing one of its name that is there, and
// pushes the credential to it, as a core does at its start.
func replaceProxy(t *testing.T, p seal.Proxy, credential string) error {
	t.Helper()
	dockerQuiet("rm", "--force", p.Name)
	if _, err := seal.KeepProxy(t.Context(), p); err != nil {
		return err
	}
	return seal.PushCredential(t.Context(), p.Name, credential)
}
