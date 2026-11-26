package tick

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/instructions"
	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/seal"
	"github.com/TheEisbaer/kiln/internal/settings"
	"github.com/TheEisbaer/kiln/internal/store"
)

// The models of the test's spec, implement and review runs. The stand-in
// names in system/init whichever it is given.
const (
	model          = "claude-haiku-5-5"
	implementModel = "claude-sonnet-5-5"
	reviewModel    = "claude-opus-5-5"
)

// The words in a card's text that change what the stand-in does.
const (
	gateWord     = "kiln-stand-in-gate"
	failWord     = "kiln-stand-in-fail"
	noSpecWord   = "kiln-stand-in-nospec"
	longSpecWord = "kiln-stand-in-longspec"
	escapeWord   = "kiln-stand-in-escape"
	goesBackWord = "kiln-stand-in-goesback"
	// alwaysBackWord makes every review of the card go back.
	alwaysBackWord = "kiln-stand-in-alwaysback"
	noChangeWord   = "kiln-stand-in-nochange"
	// configWord makes implement runs change the repo's Claude Code
	// configuration (testdata/claude/claude).
	configWord = "kiln-stand-in-config"
)

// The spec the stand-in writes with kiln-stand-in-escape.
const escapeSpec = "# Spec\n\x1b[2J\x1b]0;kiln\x07\r\xff\n"

// The spec the scripted upstream has the real Claude Code write in the case
// kiln-case-tick-spec (internal/run/testdata/claude).
const scriptedSpec = "# Spec of the scripted run\n\n## 1. What will be built\n\nNothing; kiln-case-tick-spec.\n"

// hosts is the list of hosts of the test's cores: a run has a way out, and its
// card waits for approval. The stand-in does not use it, and the name resolves
// nowhere, so nothing leaves the engine.
var hosts = []string{"public.kiln-test.example"}

// caseWord is a card's word kiln-case-<name>.
var caseWord = regexp.MustCompile(`kiln-case-[a-z0-9-]+`)

// assertSaveNamed asserts that a save's subject is the approved spec's title
// and its trailer Kiln-Run the implement run's ID.
func assertSaveNamed(t *testing.T, repo, commit, title, id string) {
	t.Helper()
	if got := gitIn(t, repo, "log", "-1", "--format=%s", commit); got != title {
		t.Errorf("the save %s has the subject %q, want %q", commit, got, title)
	}
	if got := gitIn(t, repo, "log", "-1", "--format=%(trailers:key=Kiln-Run,valueonly)", commit); strings.TrimSpace(got) != id {
		t.Errorf("the save %s has the trailer Kiln-Run %q, want %q", commit, got, id)
	}
}

// standInSpec is the spec the stand-in writes for a card's text, or nil for
// none.
func standInSpec(text string) *string {
	var spec string
	switch {
	case strings.Contains(text, noSpecWord):
		return nil
	case strings.Contains(text, longSpecWord):
		spec = strings.Repeat("x", 262145)
	case strings.Contains(text, escapeWord):
		spec = escapeSpec
	default:
		word := caseWord.FindString(text)
		spec = "# Spec " + word + "\n\nPull request title: `feat: " + word + "`\n\n## 1. What will be built\n\n## 2. How to tell it works\n\n" +
			"## 3. What was found\n\n## 4. Left out\n\n## 5. Questions\n"
	}
	return &spec
}

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
	// The way out's image and the outward network of the test's ways out.
	wayOutImage, outward string
	// The output.jsonl of the stand-in's run in the first case, which the
	// real case compares with its own.
	standInOutput []byte
}

// TestTick is a large test (ADR 0031, ADR 0048): it needs the container
// engine. It runs its own binary as `kiln core` with a tick of 50 ms, against
// a fresh data directory, instance and repo per case, and watches the store
// and the starts of the instance's containers. Every case but one runs the
// stand-in for Claude Code; the last runs the pinned Claude Code against
// internal/run's scripted upstream. Every run is a spec run with a list of
// hosts, unless a case says otherwise, and nothing of it is saved.
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
	f.wayOutImage, f.outward = name("wayout"), name("outward")
	build(t, f.proxyImage, "../..", "images/proxy/Dockerfile")
	build(t, f.wayOutImage, "../..", "images/wayout/Dockerfile")
	build(t, upstreamImage, "../..", "internal/run/testdata/claude/Dockerfile")
	build(t, f.runImage, "../..", "images/run/Dockerfile")
	build(t, f.standIn, "testdata/claude", "internal/tick/testdata/claude/Dockerfile", "--build-arg", "RUN="+f.runImage)
	// The seal creates the outward network with the first way out.
	t.Cleanup(func() { dockerQuiet("network", "rm", f.outward) })

	// The proxy and the upstream on the ordinary network, which the test
	// makes, so that KeepProxy finds it; TestRun of internal/seal has
	// KeepProxy make one.
	ordinary := name("ordinary")
	testNetwork(t, ordinary)
	f.upstream = name("upstream")
	f.proxy = seal.Proxy{Name: name("proxy"), Image: f.proxyImage, Network: ordinary,
		Upstream: "http://" + f.upstream + ":" + upstreamPort}
	t.Cleanup(func() { dockerQuiet("rm", "--force", f.proxy.Name) })
	if err := replaceProxy(t, f.proxy, "fake-credential-"+f.id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dockerQuiet("rm", "--force", f.upstream) })
	must(t, "run", "--detach", "--name", f.upstream, "--network", ordinary, upstreamImage, f.marker)

	t.Run("a card that waits gets one run", func(t *testing.T) {
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Add a line to NOTES.md. kiln-case-happy "+gateWord)
		core, done := f.core(t, c, "")
		f.waitAtGate(t, c)
		f.assertInstruction(t, c)
		must(t, "exec", "kiln-run-"+c.run, "touch", "/tmp/gate")
		f.waitEnded(t, c)
		f.stop(t, core, done)
		f.assertEnded(t, c, store.Finished)
		f.standInOutput = readFile(t, filepath.Join(c.data, "runs", c.run, "output.jsonl"))
	})

	// A core whose working directory is removed runs every command in a
	// directory it owns, and its card's run ends as it would (card 16).
	t.Run("a core started in a directory that is gone", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Add a line to NOTES.md.")
		core, done := f.start(t, child{Command: "core", Config: f.config(c), GoneDir: true})
		f.waitEnded(t, c)
		f.stop(t, core, done)
		f.assertEnded(t, c, store.Finished)
		if strings.Contains(core.Stderr.(*bytes.Buffer).String(), "working directory") {
			t.Errorf("the core says:\n%s", core.Stderr)
		}
	})

	t.Run("killed, then started again", func(t *testing.T) {
		t.Parallel()
		for _, point := range []string{"after-instruction", "before-add", "after-add", "following",
			"record", "record, damaged", "before-end", "after-end"} {
			t.Run(point, func(t *testing.T) {
				t.Parallel()
				c := f.newCase(t, f.standIn)
				text := "Add a line to NOTES.md."
				if point == "following" || point == "after-add" {
					text += " " + gateWord
				}
				f.card(t, c, text)
				switch point {
				case "record", "record, damaged":
					// Killed after the record, before ReadSpec.
					ch := child{Command: "core", Config: f.config(c), Hook: "run:record:exit"}
					if code, out, errOut := f.kiln(t, ch); code != exitAtPoint {
						t.Fatalf("the core exited with %d, want %d after the record\n%s%s", code, exitAtPoint, out, errOut)
					}
				case "following":
					core, done := f.core(t, c, "")
					f.waitAtGate(t, c)
					core.Process.Signal(syscall.SIGKILL)
					<-done
					must(t, "exec", "kiln-run-"+c.run, "touch", "/tmp/gate")
				default:
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
				case "after-instruction":
					if inFlight != nil {
						t.Errorf("a run is in flight: %+v", inFlight)
					}
					if got := containerState(t, c.run); got != "" {
						t.Errorf("the container is %q, want none", got)
					}
					if got := string(readFile(t, c.instruction())); got != c.wantInstruction() {
						t.Errorf("the instruction is %q", got)
					}
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
					// A placing broken off halfway leaves a prefix of the same
					// archive: some files written, the last of them cut short.
					// Here, as root, which a placing never writes as: a cut
					// NOTES.md, another CLAUDE.md, a cut instruction. Placing
					// again must write each as the commit has it.
					dir := t.TempDir()
					for name, data := range map[string]string{"NOTES.md": "# No", "CLAUDE.md": "other\n", "instruction.md": "Add a"} {
						if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					must(t, "cp", filepath.Join(dir, "NOTES.md"), "kiln-run-"+c.run+":/work/NOTES.md")
					must(t, "cp", filepath.Join(dir, "CLAUDE.md"), "kiln-run-"+c.run+":/work/CLAUDE.md")
					must(t, "cp", filepath.Join(dir, "instruction.md"), "kiln-run-"+c.run+":/kiln/instruction.md")
				case "record", "record, damaged", "before-end":
					if inFlight == nil {
						t.Error("no run is in flight after result.json")
					}
					if _, err := os.Stat(filepath.Join(c.data, "runs", c.run, "result.json")); err != nil {
						t.Error(err)
					}
					if got := containerState(t, c.run); got != "exited" {
						t.Errorf("the container is %q while the run is in flight, want exited", got)
					}
					if n := countSpecs(t, c); n != 0 {
						t.Errorf("%d specs in the store before EndRun, want none", n)
					}
					if point == "record, damaged" {
						damage(t, c)
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
				if point == "after-add" {
					// Placing again wrote each file as the commit has it.
					f.waitAtGate(t, c)
					for name, want := range map[string]string{"/work/NOTES.md": "# Notes\n",
						"/work/CLAUDE.md": "# Try\n\nThe marker is " + f.marker + ".\n"} {
						if got := must(t, "exec", "kiln-run-"+c.run, "cat", name); got != strings.TrimSpace(want) {
							t.Errorf("%s is %q after placing again, want %q", name, got, want)
						}
					}
					f.assertInstruction(t, c)
					must(t, "exec", "kiln-run-"+c.run, "touch", "/tmp/gate")
				}
				f.waitEnded(t, c)
				f.stop(t, core, done)
				if point == "record, damaged" {
					e := f.assertEndedAt(t, c, store.Finished, store.Developer, nil)
					if !strings.HasPrefix(e.Reason, "the run's documents.tar is damaged: ") {
						t.Errorf("the reason is %q, want the archive damaged", e.Reason)
					}
					return
				}
				f.assertEnded(t, c, store.Finished)
			})
		}
	})

	// From AddRun until the result is recorded, nothing removes the run's
	// container: a core that fails at a step, is interrupted or is killed
	// there leaves the run in flight with its container and network, and a
	// later core ends it finished, with one start in all. The steps are those
	// between AddRun and the first event of following, on the first start and
	// for a run in the store whose container never started.
	t.Run("a failure between AddRun and following", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name     string
			inStore  bool   // a first core was killed after AddRun, leaving the container created
			hook     string // the faulty core's Hook
			repo     string // the faulty core's --repo, when it differs
			image    string // the faulty core's --image, when it differs
			exitCode int    // the faulty core's exit status
			// The faulty core ends the run itself: a spec run in the store
			// uses the image neither for its container, which is there, nor
			// for a save, which it does not make.
			ends bool
		}{
			{name: "first start: the start, interrupted", hook: "tick:after-add:interrupt"},
			{name: "first start: after the start", hook: "run:start:fail", exitCode: exitAtPoint},
			{name: "in the store: step 0, with no repo", inStore: true, repo: "/nonexistent/repo.git", exitCode: exitAtPoint},
			{name: "in the store: placing again", inStore: true, hook: "run:place:fail", exitCode: exitAtPoint},
			{name: "in the store: the start, interrupted", inStore: true, hook: "run:place:interrupt"},
			{name: "in the store: after the start", inStore: true, hook: "run:start:fail", exitCode: exitAtPoint},
			{name: "in the store: with a missing image", inStore: true, image: "kiln-ticktest-no-such-image", exitCode: exitAtPoint, ends: true},
			{name: "in the store: killed after placing again", inStore: true, hook: "run:place:exit", exitCode: exitAtPoint},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				c := f.newCase(t, f.standIn)
				f.card(t, c, "Add a line to NOTES.md.")
				if tc.inStore {
					_, done := f.core(t, c, "after-add")
					if code := <-done; code != exitAtPoint {
						t.Fatalf("the first core exited with %d, want %d after AddRun", code, exitAtPoint)
					}
				}
				cfg := f.config(c)
				if tc.repo != "" {
					cfg.Repo = tc.repo
				}
				if tc.image != "" {
					cfg.Image = tc.image
				}
				// A core that does not end by its hook ends once its follower
				// has returned.
				ch := child{Command: "core", Config: cfg, Hook: tc.hook, ExitAt: "followed"}
				if code, out, errOut := f.kiln(t, ch); code != tc.exitCode {
					t.Fatalf("the faulty core exited with %d, want %d\n%s%s", code, tc.exitCode, out, errOut)
				}
				if tc.ends {
					f.assertEnded(t, c, store.Finished)
					return
				}

				s := f.open(t, c)
				inFlight, err := s.InFlight(1)
				s.Close()
				if err != nil || inFlight == nil || inFlight.ID != c.run {
					t.Errorf("the run in flight is %+v (%v), want %s", inFlight, err, c.run)
				}
				assertKept(t, c.run)

				core, done := f.core(t, c, "")
				f.waitEnded(t, c)
				f.stop(t, core, done)
				f.assertEnded(t, c, store.Finished)
			})
		}
	})

	// Where the window was: a run in the store whose container is created
	// again was removed first, and a kill before the new one existed lost the
	// run. The core is killed at the first removal of the run's container,
	// which must come only after the run has ended in the store.
	t.Run("killed at the first removal of a stored run's container", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Add a line to NOTES.md.")
		_, done := f.core(t, c, "after-add")
		if code := <-done; code != exitAtPoint {
			t.Fatalf("the first core exited with %d, want %d after AddRun", code, exitAtPoint)
		}
		destroyed := watchEvents(t, "label=kiln.instance="+c.instance, "destroy", time.Now())
		core, done := f.core(t, c, "")
		waitUntil(t, "the run's container is removed", func() bool { return destroyed.count("kiln-run-") > 0 })
		core.Process.Signal(syscall.SIGKILL)
		<-done

		s := f.open(t, c)
		inFlight, err := s.InFlight(1)
		s.Close()
		if err != nil || inFlight != nil {
			t.Errorf("the run's container was removed while the run was in flight: %+v (%v)", inFlight, err)
		}
		core, done = f.core(t, c, "")
		f.waitEnded(t, c)
		f.stop(t, core, done)
		f.assertEnded(t, c, store.Finished)
	})

	// A stored run's container that is used again gets the proxy on its
	// network, as one that runs does: a proxy replaced since is on no earlier
	// run's network. The stand-in never calls the proxy, so the attachment
	// itself is asserted, while the run waits at its gate. The proxy is the
	// case's own, so that the other cases' runs keep theirs.
	t.Run("a stored run started after the proxy was replaced", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		proxy := f.proxy
		proxy.Name = name("proxy-replaced")
		t.Cleanup(func() { dockerQuiet("rm", "--force", proxy.Name) })
		if err := replaceProxy(t, proxy, "fake-credential-"+f.id); err != nil {
			t.Fatal(err)
		}
		c.proxy = proxy.Name
		f.card(t, c, "Add a line to NOTES.md. "+gateWord)
		_, done := f.core(t, c, "after-add")
		if code := <-done; code != exitAtPoint {
			t.Fatalf("the first core exited with %d, want %d after AddRun", code, exitAtPoint)
		}
		if err := replaceProxy(t, proxy, "fake-credential-"+f.id); err != nil {
			t.Fatal(err)
		}
		if proxyOn(t, proxy.Name, "kiln-run-"+c.run) {
			t.Fatal("the replaced proxy is on the run's network already")
		}

		core, done := f.core(t, c, "")
		f.waitAtGate(t, c)
		if !proxyOn(t, proxy.Name, "kiln-run-"+c.run) {
			t.Error("the run started without the proxy on its network")
		}
		must(t, "exec", "kiln-run-"+c.run, "touch", "/tmp/gate")
		f.waitEnded(t, c)
		f.stop(t, core, done)
		f.assertEnded(t, c, store.Finished)
	})

	t.Run("a vanished run", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Add a line to NOTES.md. "+gateWord)
		core, done := f.core(t, c, "")
		f.waitAtGate(t, c)
		core.Process.Signal(syscall.SIGKILL)
		<-done
		must(t, "rm", "--force", "kiln-run-"+c.run)

		// The vanished run fails, and the rule runs it once more.
		core, done = f.core(t, c, "")
		first := c.run
		c.run = c.runOf(2)
		f.waitAtGate(t, c)
		must(t, "exec", "kiln-run-"+c.run, "touch", "/tmp/gate")
		f.waitState(t, c, finished, 2)
		f.stop(t, core, done)
		f.assertRun(t, c, 1, model, "high", c.commit, store.Failed)
		f.assertRun(t, c, 2, model, "high", c.commit, store.Finished)
		s := f.open(t, c)
		h, err := s.History(1)
		s.Close()
		if err != nil || len(h) != 5 {
			t.Fatalf("the history is %+v (%v), want five entries", h, err)
		}
		if e := h[2]; e.Run != first || !strings.Contains(e.Reason, "container is gone") {
			t.Errorf("the first run's entry is %+v, want that the container is gone", e)
		}
		if e := h[3]; e.Cause != store.CauseRule || e.State != waiting || e.Reason != store.RetryPrefix+run.ErrGone.Error() {
			t.Errorf("the rule's entry is %+v", e)
		}
		if e := h[4]; e.Run != c.run || e.State != finished {
			t.Errorf("the retry's entry is %+v", e)
		}
	})

	// The stand-in writes its spec before it fails; a failed run's is not read.
	// It fails on the retry as well, and the card waits for the developer.
	t.Run("a run that fails", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Add a line to NOTES.md. "+failWord)
		core, done := f.core(t, c, "")
		f.waitState(t, c, failed, 2)
		f.stop(t, core, done)
		f.assertRun(t, c, 1, model, "high", c.commit, store.Failed)
		f.assertRun(t, c, 2, model, "high", c.commit, store.Failed)
		if n := countSpecs(t, c); n != 0 {
			t.Errorf("%d rows in specs, want none", n)
		}
		e := f.lastEntry(t, c)
		if e.Cause != store.CauseRule || e.State != failed ||
			!strings.HasPrefix(e.Reason, "the retry failed as well, and the rule runs a failed run once more at most, so it waits for you; the first run: exit 1, no result, ") ||
			!strings.Contains(e.Reason, "; the retry: exit 1, no result, ") {
			t.Errorf("the last entry is %+v, want the rule's with both runs' reasons", e)
		}
	})

	// A finished run whose spec cannot be taken, or that had no web: the run
	// is finished, and the card goes under needs you with the reason.
	for _, tc := range []struct {
		name   string
		text   string
		hosts  bool   // the core has the list
		reason string // the reason's start
	}{
		{"no spec", "Write no spec. " + noSpecWord, true, "the run wrote no spec (/kiln/out/spec.md); exit 0, "},
		{"a spec too long", "Write a long spec. " + longSpecWord, true, "the spec is 262145 bytes, more than 262144; exit 0, "},
		{"without a list of hosts", "Write a spec. kiln-case-nolist", false, "the spec was written without the web: no list of hosts; exit 0, "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := f.newCase(t, f.standIn)
			c.noHosts = !tc.hosts
			f.card(t, c, tc.text)
			core, done := f.core(t, c, "")
			f.waitEnded(t, c)
			f.stop(t, core, done)
			var kept *string
			if !tc.hosts {
				kept = standInSpec(tc.text) // the spec is kept
			}
			e := f.assertEndedAt(t, c, store.Finished, store.Developer, kept)
			if !strings.HasPrefix(e.Reason, tc.reason) || !strings.HasSuffix(e.Reason, ", not saved") {
				t.Errorf("the reason is %q, want it to begin %q and end with not saved", e.Reason, tc.reason)
			}
		})
	}

	// The store keeps the spec exactly as written.
	t.Run("a spec with escapes and a byte that is not UTF-8", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Write a spec. "+escapeWord)
		core, done := f.core(t, c, "")
		f.waitEnded(t, c)
		f.stop(t, core, done)
		f.assertEnded(t, c, store.Finished)
	})

	t.Run("its way out gone while it runs", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Write a spec. kiln-case-gone "+gateWord)
		core, done := f.core(t, c, "")
		f.waitAtGate(t, c)
		must(t, "rm", "--force", "kiln-wayout-"+c.run)
		must(t, "exec", "kiln-run-"+c.run, "touch", "/tmp/gate")
		f.waitEnded(t, c)
		f.stop(t, core, done)
		e := f.assertEndedAt(t, c, store.Finished, store.Developer, standInSpec(c.text))
		if !strings.HasPrefix(e.Reason, "the spec was written without the web: its way out was gone; exit 0, ") {
			t.Errorf("the reason is %q, want that its way out was gone", e.Reason)
		}
	})

	t.Run("what the sweep removes, and what it leaves", func(t *testing.T) {
		t.Parallel()
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
		testNetwork(t, "kiln-run-"+mine, "--internal", "--label", "kiln.run="+mine, "--label", "kiln.instance="+c.instance)
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

	// The screen of a core shows the run in flight and its activity while
	// the run waits at its gate.
	t.Run("the screen while a run waits", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		c.listen = freeAddress(t)
		f.card(t, c, "Write a spec. kiln-case-screen "+gateWord)
		core, done := f.core(t, c, "")
		f.waitAtGate(t, c)
		var b struct {
			Cards []struct {
				ID       int64  `json:"id"`
				WaitsFor string `json:"waits_for"`
				Run      *struct {
					ID    string `json:"id"`
					Model string `json:"model"`
					Since string `json:"since"`
				} `json:"run"`
			} `json:"cards"`
		}
		getJSON(t, "http://"+c.listen+"/api/board", &b)
		if len(b.Cards) != 1 || b.Cards[0].Run == nil || b.Cards[0].Run.ID != c.run || b.Cards[0].Run.Model != model ||
			b.Cards[0].Run.Since == "" || b.Cards[0].WaitsFor != "run" {
			t.Errorf("the board shows %+v", b.Cards)
		}
		type activity struct {
			Kind, Tool, Text string
		}
		var card struct {
			Activity []activity `json:"activity"`
		}
		want := []activity{{"text", "", "Reading the card."}, {"tool", "Bash", "Read the notes"}}
		waitUntil(t, "the card's page shows the run's activity", func() bool {
			getJSON(t, "http://"+c.listen+"/api/cards/1", &card)
			return len(card.Activity) >= len(want)
		})
		if fmt.Sprint(card.Activity) != fmt.Sprint(want) {
			t.Errorf("the activity is %+v, want %+v", card.Activity, want)
		}
		must(t, "exec", "kiln-run-"+c.run, "touch", "/tmp/gate")
		f.waitEnded(t, c)
		getJSON(t, "http://"+c.listen+"/api/cards/1", &card)
		if len(card.Activity) != 0 {
			t.Errorf("the activity of an ended run is still shown: %+v", card.Activity)
		}
		f.stop(t, core, done)
		f.assertEnded(t, c, store.Finished)
	})

	t.Run("a second core or a kiln card while one runs", func(t *testing.T) {
		t.Parallel()
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

	// After approval: an implement run, which saves a commit on the card's
	// branch, and a review run, after which the card waits for approval.
	t.Run("approved, implemented and reviewed", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Add a line to NOTES.md. kiln-case-loop")
		f.specAndApprove(t, c)
		core, done := f.core(t, c, "")
		f.waitState(t, c, reviewPassed, 3)
		f.stop(t, core, done)

		tip := f.assertLoop(t, c, []string{"implement/run by developer", "review/run by run", "review/approval by run"},
			[]string{"spec 1", "report 2", "pull-request 2", "review 3"})
		f.assertRun(t, c, 2, implementModel, "medium", c.commit, store.Finished)
		f.assertRun(t, c, 3, reviewModel, "high", tip, store.Finished)
		if n := gitIn(t, c.repo, "rev-list", "--count", c.commit+".."+tip); n != "1" {
			t.Errorf("%s commits on the branch, want 1", n)
		}
		if parent := gitIn(t, c.repo, "rev-parse", tip+"^"); parent != c.commit {
			t.Errorf("the save's parent is %s, want main's %s", parent, c.commit)
		}
		assertSaveNamed(t, c.repo, tip, "feat: kiln-case-loop", c.runOf(2))
		// The implement run's change is saved, the spec run's and the review
		// run's are not.
		if got := gitIn(t, c.repo, "show", tip+":NOTES.md"); got != "# Notes\nA line from the implement run." {
			t.Errorf("NOTES.md on the branch is %q", got)
		}

		// What each stage was told, each part under its heading.
		spec := *standInSpec(c.text)
		implement := string(readFile(t, c.instructionOf(2)))
		if want := instructions.Implement + "## The card\n\n" + c.text + "\n\n## The approved spec\n\n" + fenced(spec, "markdown"); implement != want {
			t.Errorf("the implement run was told\n%s\nwant\n%s", implement, want)
		}
		review := string(readFile(t, c.instructionOf(3)))
		diff := gitIn(t, c.repo, "diff", c.commit, tip) + "\n"
		if want := instructions.Review + "## The card\n\n" + c.text + "\n\n## The approved spec\n\n" + fenced(spec, "markdown") +
			"\n## The change\n\n```diff\n" + diff + "```\n"; review != want {
			t.Errorf("the review run was told\n%s\nwant\n%s", review, want)
		}
		if !strings.Contains(diff, "+A line from the implement run.") {
			t.Errorf("the diff is\n%s", diff)
		}
	})

	// Spec, approval, implement and review, which the core publishes by itself
	// once the review passes: the stand-in for the publish command "merges" by
	// moving origin's main to the commit it is given, and after the next fetch
	// the card is done, merged. No key, no token and no Git host: origin is a bare repo here.
	t.Run("published and merged", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		origin := filepath.Join(t.TempDir(), "origin.git")
		gitIn(t, filepath.Dir(origin), "clone", "--quiet", "--bare", c.repo, origin)
		gitIn(t, c.repo, "remote", "set-url", "origin", origin)
		dir := t.TempDir()
		f.card(t, c, "Add a line to NOTES.md. kiln-case-publish")
		f.specAndApprove(t, c)
		cfg := f.config(c)
		cfg.PublishCommand = `set -eu
env > ` + shellWord(dir) + `/env
cp "$KILN_TEXT_FILE" ` + shellWord(dir) + `/text
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git --git-dir="$KILN_REPO" push --quiet origin "$KILN_COMMIT:refs/heads/main"
echo "https://git.example/pull/$KILN_CARD"`
		core, done := f.start(t, child{Command: "core", Config: cfg})
		f.waitState(t, c, store.State{Stage: store.Done, WaitsFor: store.Nothing}, 3)
		f.stop(t, core, done)

		tip := f.assertLoop(t, c, []string{"implement/run by developer", "review/run by run", "review/approval by run",
			"review/host by rule", "review/host by rule", "done/nothing by host"},
			[]string{"spec 1", "report 2", "pull-request 2", "review 3"})
		e := f.lastEntry(t, c)
		if e.Reason != "merged" {
			t.Errorf("the last entry is %+v, want merged", e)
		}
		commit := gitIn(t, c.repo, "rev-parse", "refs/kiln/publish/card-1")
		if main := gitIn(t, origin, "rev-parse", "refs/heads/main"); main != commit {
			t.Errorf("origin's main is %s, want the published %s", main, commit)
		}
		if got := gitIn(t, c.repo, "rev-parse", "refs/heads/main"); got != commit {
			t.Errorf("the copy's main is %s after the fetch, want %s", got, commit)
		}
		if got := gitIn(t, c.repo, "rev-parse", commit+"^{tree}"); got != gitIn(t, c.repo, "rev-parse", tip+"^{tree}") {
			t.Errorf("the published tree is not the branch's")
		}
		env := string(readFile(t, filepath.Join(dir, "env")))
		for _, want := range []string{"KILN_CARD=1\n", "KILN_TITLE=feat: kiln-case-publish\n", "KILN_BRANCH=card/1-kiln-case-publish\n",
			"KILN_COMMIT=" + commit + "\n", "KILN_REPO=" + c.repo + "\n"} {
			if !strings.Contains(env, want) {
				t.Errorf("the command was not told %q:\n%s", want, env)
			}
		}
		if text := string(readFile(t, filepath.Join(dir, "text"))); text != "Adds a line to NOTES.md (kiln-case-publish).\n\nChecked by reading NOTES.md.\n" {
			t.Errorf("the pull request's text is %q", text)
		}
		s := f.open(t, c)
		defer s.Close()
		if pub, err := s.Publication(1); err != nil || pub.Address != "https://git.example/pull/1" || pub.Commit != commit {
			t.Errorf("the publication is %+v (%v)", pub, err)
		}
	})

	// A review that sends the change back sends the card to implement by the
	// rule, on one core: a second implement run is told the review, runs on
	// the stronger model, starts from the branch's tip and saves a second
	// commit, which the next review passes.
	t.Run("a review that sends the change back", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		mainTrees := watchMainTrees(t, c)
		f.card(t, c, "Add a line to NOTES.md. kiln-case-back "+goesBackWord)
		f.specAndApprove(t, c)
		core, done := f.core(t, c, "")
		f.waitState(t, c, reviewPassed, 5)
		f.stop(t, core, done)

		// A card that changes no configuration is laid over in no run
		// (ADR 0025): no /kiln/card-config, no part on it, no label.
		for n := 2; n <= 5; n++ {
			if seen := f.seen(t, c, n); !strings.HasSuffix(seen, "== card-config absent\n") {
				t.Errorf("run %d saw\n%s", n, seen)
			}
			if strings.Contains(string(readFile(t, c.instructionOf(n))), "## The repo's Claude Code configuration") {
				t.Errorf("run %d was told of the configuration", n)
			}
		}
		if got := mainTrees.laidOver(); len(got) > 0 {
			t.Errorf("containers with the label kiln.main-tree: %q", got)
		}

		tip := f.assertLoop(t, c, []string{"implement/run by developer", "review/run by run", "review/developer by run",
			"implement/run by rule", "review/run by run", "review/approval by run"},
			[]string{"spec 1", "report 2", "pull-request 2", "review 3", "report 4", "pull-request 4", "review 5"})
		first := gitIn(t, c.repo, "rev-parse", tip+"^")
		f.assertRun(t, c, 4, run.SentBackModel, "medium", first, store.Finished)
		s := f.open(t, c)
		h, err := s.History(1)
		s.Close()
		if err != nil {
			t.Fatal(err)
		}
		if e := h[len(h)-4]; !strings.HasPrefix(e.Reason, "the review sends the change back; exit 0, ") || e.Run != c.runOf(3) {
			t.Errorf("the review's entry is %+v, want the review sending the change back", e)
		}
		if e := h[len(h)-3]; e.Reason != "the review sent the change back; implement runs again by the rule, send-back 1 of 2" {
			t.Errorf("the rule's entry is %+v", e)
		}
		if n := gitIn(t, c.repo, "rev-list", "--count", c.commit+".."+tip); n != "2" {
			t.Errorf("%s commits on the branch, want 2", n)
		}
		if parent := gitIn(t, c.repo, "rev-parse", tip+"^"); parent != first {
			t.Errorf("the second save's parent is %s, want the first save %s", parent, first)
		}
		// Every save of a card carries the approved spec's title.
		assertSaveNamed(t, c.repo, first, "feat: kiln-case-back", c.runOf(2))
		assertSaveNamed(t, c.repo, tip, "feat: kiln-case-back", c.runOf(4))
		if got := gitIn(t, c.repo, "show", tip+":NOTES.md"); got != "# Notes\nA line from the implement run.\nA fix after the review." {
			t.Errorf("NOTES.md on the branch is %q", got)
		}
		told := string(readFile(t, c.instructionOf(4)))
		if want := "\n## The last review\n\n" + fenced(f.document(t, c, 3), "markdown"); !strings.HasSuffix(told, want) || !strings.Contains(want, "Verdict: goes back") {
			t.Errorf("the second implement run was told\n%s\nwant it to end with\n%s", told, want)
		}
		if strings.Contains(string(readFile(t, c.instructionOf(2))), "## The last review") {
			t.Error("the first implement run was told a review")
		}
	})

	// A card whose branch changes the repo's Claude Code configuration, through
	// a review that sends it back: the review run and the second implement run
	// work under main's version (ADR 0025, rule 2), and the branch keeps the
	// card's, with the fix the second implement run made in /kiln/card-config.
	t.Run("a card that changes the repo's Claude Code configuration", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		mainFiles := map[string]string{
			"CLAUDE.md":              "# Try\n\nThe marker is " + f.marker + ".\n",
			"NOTES.md":               "# Notes\n",
			"run.sh":                 "set -eu\nprintf 'A line from the run.\\n' >> NOTES.md\necho checked\n",
			"pkg/AGENTS.md":          "Main's AGENTS.md.\n",
			".claude/hooks/check.sh": "Main's hook.\n",
			".claude/hooks/gone.sh":  "Main's other hook.\n",
		}
		c.repo, c.commit = bareRepo(t, mainFiles)
		c.refs, c.objects = copyHolds(t, c.repo)
		mainTrees := watchMainTrees(t, c)
		f.card(t, c, "Add a line to NOTES.md. kiln-case-config "+goesBackWord+" "+configWord)
		f.specAndApprove(t, c)
		core, done := f.core(t, c, "")
		f.waitState(t, c, reviewPassed, 5)
		f.stop(t, core, done)

		tip := f.assertLoop(t, c, []string{"implement/run by developer", "review/run by run", "review/developer by run",
			"implement/run by rule", "review/run by run", "review/approval by run"},
			[]string{"spec 1", "report 2", "pull-request 2", "review 3", "report 4", "pull-request 4", "review 5"})

		// The first implement run starts from main and is not laid over; the
		// review and the second implement run see main's configuration.
		if seen := f.seen(t, c, 2); !strings.HasSuffix(seen, "== card-config absent\n") {
			t.Errorf("the first implement run saw\n%s", seen)
		}
		wantSeen := "== CLAUDE.md\n" + mainFiles["CLAUDE.md"] + "== pkg/AGENTS.md\n" + mainFiles["pkg/AGENTS.md"] +
			"== .claude/hooks/check.sh\n" + mainFiles[".claude/hooks/check.sh"] +
			"== .claude\n.claude\n.claude/hooks\n.claude/hooks/check.sh\n.claude/hooks/gone.sh\n== card-config present\n"
		list := "```text\nM\t.claude/hooks/check.sh\nD\t.claude/hooks/gone.sh\nA\t.claude/skills/new/SKILL.md\nM\tCLAUDE.md\nM\tpkg/AGENTS.md\n```\n"
		for _, n := range []int{3, 4, 5} {
			if seen := f.seen(t, c, n); seen != wantSeen {
				t.Errorf("run %d saw\n%s\nwant\n%s", n, seen, wantSeen)
			}
			told := string(readFile(t, c.instructionOf(n)))
			if !strings.Contains(told, "\n## The repo's Claude Code configuration\n\n") || !strings.Contains(told, list) {
				t.Errorf("run %d was told\n%s\nwant the configuration's part with\n%s", n, told, list)
			}
		}
		if got := mainTrees.laidOver(); !slices.Equal(got, []string{"kiln-run-" + c.runOf(3), "kiln-run-" + c.runOf(4), "kiln-run-" + c.runOf(5)}) {
			t.Errorf("the laid-over containers are %q, want those of runs 3 to 5", got)
		}

		// The branch keeps the card's configuration, with the fix made in
		// /kiln/card-config and without the change made in /work.
		for path, want := range map[string]string{
			"CLAUDE.md":                   "The card's CLAUDE.md.\nA fix of the configuration.",
			"pkg/AGENTS.md":               "The card's AGENTS.md.",
			".claude/hooks/check.sh":      "The card's hook.",
			".claude/skills/new/SKILL.md": "The card's skill.",
			"NOTES.md":                    "# Notes\nA line from the implement run.\nA fix after the review.",
		} {
			if got := gitIn(t, c.repo, "show", tip+":"+path); got != want {
				t.Errorf("%s on the branch is %q, want %q", path, got, want)
			}
		}
		if files := gitIn(t, c.repo, "ls-tree", "-r", "--name-only", tip); strings.Contains(files, "gone.sh") {
			t.Errorf("the branch has the removed hook:\n%s", files)
		}
	})

	// A card sent back to spec keeps its branch, while its spec run starts
	// from the head of main: the spec run starts, ends with a spec, and
	// leaves the branch as it was for the implement run that follows.
	t.Run("sent back to spec with a branch", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Add a line to NOTES.md. kiln-case-tospec "+alwaysBackWord)
		f.specAndApprove(t, c)
		core, done := f.core(t, c, "")
		// Two send-backs by the rule, and the third review stops the card.
		f.waitState(t, c, reviewNeedsYou, 7)
		f.stop(t, core, done)
		if e := f.lastEntry(t, c); e.Reason != "the review sent the change back a third time; the rule sends a change back twice at most, so it waits for you" {
			t.Errorf("the last entry is %+v, want the rule's stop", e)
		}
		branch := gitIn(t, c.repo, "rev-parse", "refs/heads/kiln/card-1")

		s := f.open(t, c)
		_, err := s.Decide(1, store.ToSpec, "")
		s.Close()
		if err != nil {
			t.Fatal(err)
		}
		core, done = f.core(t, c, "")
		f.waitState(t, c, finished, 8)
		f.stop(t, core, done)
		f.assertRun(t, c, 8, model, "high", c.commit, store.Finished)
		if !strings.HasPrefix(f.document(t, c, 8), "# Spec kiln-case-tospec") {
			t.Errorf("the new spec is %q", f.document(t, c, 8))
		}
		if got := gitIn(t, c.repo, "rev-parse", "refs/heads/kiln/card-1"); got != branch {
			t.Errorf("the branch is at %s, and was at %s", got, branch)
		}
	})

	// The rule of card 8 in the later stages: a kill between a run's save and
	// its record, and one between its review read and EndRun, leave the run
	// in flight; a later core ends it where the table says, with one commit
	// on the branch and one document of the run.
	for _, tc := range []struct {
		name   string
		killed child  // the core that is killed, without its Config
		run    int    // the number of the run in flight after the kill
		docs   int    // the documents kept after the kill
		saved  bool   // the branch is there after the kill
		record bool   // the run's result.json is there after the kill
		kind   string // the run's document
	}{
		{"killed in an implement run, after its save", child{Hook: "run:save:exit"}, 2, 1, true, false, "report"},
		{"killed in a review run, before its end", child{ExitAt: "review/before-end"}, 3, 3, true, true, "review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := f.newCase(t, f.standIn)
			f.card(t, c, "Add a line to NOTES.md. kiln-case-kill")
			f.specAndApprove(t, c)
			ch := tc.killed
			ch.Command, ch.Config = "core", f.config(c)
			if code, out, errOut := f.kiln(t, ch); code != exitAtPoint {
				t.Fatalf("the core exited with %d, want %d\n%s%s", code, exitAtPoint, out, errOut)
			}

			s := f.open(t, c)
			inFlight, err := s.InFlight(1)
			s.Close()
			if err != nil || inFlight == nil || inFlight.ID != c.runOf(tc.run) {
				t.Fatalf("the run in flight is %+v (%v), want %s", inFlight, err, c.runOf(tc.run))
			}
			if got := containerState(t, c.runOf(tc.run)); got != "exited" {
				t.Errorf("the container is %q while the run is in flight, want exited", got)
			}
			if _, err := os.Stat(filepath.Join(c.data, "runs", c.runOf(tc.run), "result.json")); (err == nil) != tc.record {
				t.Errorf("result.json after the kill: %v, want it there: %v", err, tc.record)
			}
			if n := countSpecs(t, c); n != tc.docs {
				t.Errorf("%d documents after the kill, want %d", n, tc.docs)
			}
			saved := gitIn(t, c.repo, "for-each-ref", "--format=%(objectname)", "refs/heads/kiln/card-1")
			if (saved != "") != tc.saved {
				t.Errorf("the branch after the kill is at %q", saved)
			}

			core, done := f.core(t, c, "")
			f.waitState(t, c, reviewPassed, 3)
			f.stop(t, core, done)
			tip := f.assertLoop(t, c, []string{"implement/run by developer", "review/run by run", "review/approval by run"},
				[]string{"spec 1", "report 2", "pull-request 2", "review 3"})
			if tip != saved {
				t.Errorf("the branch is at %s, and was at %s after the kill", tip, saved)
			}
			if n := gitIn(t, c.repo, "rev-list", "--count", c.commit+".."+tip); n != "1" {
				t.Errorf("%s commits on the branch, want 1", n)
			}
			if got := f.document(t, c, tc.run); !strings.Contains(got, map[string]string{"report": "# Report", "review": "Verdict: passes"}[tc.kind]) {
				t.Errorf("the %s is %q", tc.kind, got)
			}
		})
	}

	t.Run("an implement run that changes nothing", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		f.card(t, c, "Change nothing. kiln-case-same "+noChangeWord)
		f.specAndApprove(t, c)
		core, done := f.core(t, c, "")
		f.waitState(t, c, implementNeedsYou, 2)
		f.stop(t, core, done)
		e := f.lastEntry(t, c)
		if !strings.HasPrefix(e.Reason, "the run changed nothing; exit 0, ") || !strings.HasSuffix(e.Reason, ", no change") {
			t.Errorf("the reason is %q, want that the run changed nothing", e.Reason)
		}
		if at := gitIn(t, c.repo, "for-each-ref", "refs/heads/kiln/card-1"); at != "" {
			t.Errorf("refs/heads/kiln/card-1 exists: %s", at)
		}
		if got := f.document(t, c, 2); !strings.HasPrefix(got, "# Report kiln-case-same") {
			t.Errorf("the report is %q", got)
		}
		f.assertRun(t, c, 2, implementModel, "medium", c.commit, store.Finished)
	})

	// Three cards are in flight at the same moment, each with its container,
	// through a spec run and then an implement run and a review run. The core
	// is killed while three runs are followed, in spec and in implement, and
	// started again: every run ends once, with its document, every save is
	// made once, and no card stays in flight (card 17).
	t.Run("three runs at once, killed and started again", func(t *testing.T) {
		t.Parallel()
		c := f.newCase(t, f.standIn)
		cards := []int64{1, 2, 3}
		for _, card := range cards {
			ch := child{Command: "card", Config: Config{Data: c.data}, Text: fmt.Sprintf("Add a line to NOTES.md. kiln-case-three-%d %s", card, gateWord)}
			if code, out, errOut := f.kiln(t, ch); code != 0 || strings.TrimSpace(out) != fmt.Sprint(card) {
				t.Fatalf("kiln card: exit status %d\n%s\n%s", code, out, errOut)
			}
		}
		runOf := func(card int64, n int) string { return fmt.Sprintf("%s-%d-%d", c.instance, card, n) }
		// killedAtGates waits until the nth run of every card waits at its gate,
		// kills the core there, and opens the gates while no core runs.
		killedAtGates := func(n int) {
			t.Helper()
			core, done := f.core(t, c, "")
			waitUntil(t, fmt.Sprintf("run %d of every card waits at its gate", n), func() bool {
				for _, card := range cards {
					if _, err := docker(t.Context(), "exec", "kiln-run-"+runOf(card, n), "test", "-e", "/tmp/waiting"); err != nil {
						return false
					}
				}
				return true
			})
			running := must(t, "ps", "--filter", "label=kiln.instance="+c.instance, "--filter", "status=running", "--format", "{{.Names}}")
			if k := strings.Count(running, "kiln-run-"); k != 3 {
				t.Errorf("%d run containers of the instance run at once, want 3:\n%s", k, running)
			}
			// Kiln's own repo sets no memory or CPUs: every run has the
			// defaults and a cpuset of 2.
			for _, card := range cards {
				assertRunLimits(t, "kiln-run-"+runOf(card, n), "4294967296", 2)
			}
			core.Process.Signal(syscall.SIGKILL)
			<-done
			for _, card := range cards {
				must(t, "exec", "kiln-run-"+runOf(card, n), "touch", "/tmp/gate")
			}
		}
		// until starts a core, waits until every card is in the state with
		// nothing of the instance left, opening the gates of the cards' nth
		// runs as they come, and stops the core.
		until := func(want store.State, gates int) {
			t.Helper()
			core, done := f.core(t, c, "")
			waitUntil(t, fmt.Sprintf("every card is in %s waiting for %s", want.Stage, want.WaitsFor), func() bool {
				for _, card := range cards {
					if gates > 0 {
						dockerQuiet("exec", "kiln-run-"+runOf(card, gates), "touch", "/tmp/gate")
					}
				}
				s := f.open(t, c)
				defer s.Close()
				for _, card := range cards {
					if got, err := s.Card(card); err != nil || got.State != want {
						return false
					}
				}
				return instanceLeft(t, c.instance) == ""
			})
			f.stop(t, core, done)
		}

		killedAtGates(1)
		until(finished, 0)
		s := f.open(t, c)
		for _, card := range cards {
			if _, err := s.Decide(card, store.Approve, ""); err != nil {
				t.Fatal(err)
			}
		}
		s.Close()
		killedAtGates(2)
		until(reviewPassed, 3)

		s = f.open(t, c)
		defer s.Close()
		if runs, err := s.InFlightRuns(); err != nil || len(runs) != 0 {
			t.Errorf("runs in flight: %v (%v)", runs, err)
		}
		for _, card := range cards {
			word := fmt.Sprintf("kiln-case-three-%d", card)
			runs, err := s.Runs(card)
			if err != nil || len(runs) != 3 {
				t.Fatalf("card %d has the runs %+v (%v), want 3", card, runs, err)
			}
			docs, err := s.Documents(card)
			if err != nil || len(docs) != 4 {
				t.Fatalf("card %d has the documents %+v (%v), want 4", card, docs, err)
			}
			for i, kind := range []string{store.KindSpec, store.KindReport, store.KindPullRequest, store.KindReview} {
				n := map[int]int{0: 1, 1: 2, 2: 2, 3: 3}[i]
				id := runOf(card, n)
				if d := docs[i]; d.Kind != kind || d.Run != id || d.Text != readTarEntry(t, filepath.Join(c.data, "runs", id, "documents.tar"), "out/"+kind+".md") {
					t.Errorf("document %d of card %d is %+v, want the %s of %s as written", i+1, card, d, kind, id)
				}
			}
			for i := range 3 {
				id := runOf(card, i+1)
				if r := runs[i]; r.ID != id || r.Ending != store.Finished {
					t.Errorf("run %d of card %d is %+v, want %s finished", i+1, card, r.RunRow, id)
				}
				// Each run's container started once.
				if n := c.starts.count("kiln-run-" + id); n != 1 {
					t.Errorf("the container of run %s started %d times, want 1", id, n)
				}
			}
			if !strings.Contains(docs[0].Text, word) {
				t.Errorf("the spec of card %d is %q", card, docs[0].Text)
			}
			branch := fmt.Sprintf("refs/heads/kiln/card-%d", card)
			tip := gitIn(t, c.repo, "rev-parse", branch)
			if n := gitIn(t, c.repo, "rev-list", "--count", c.commit+".."+tip); n != "1" {
				t.Errorf("%s commits on %s, want 1", n, branch)
			}
			assertSaveNamed(t, c.repo, tip, "feat: "+word, runOf(card, 2))
		}
		if n := c.starts.count("kiln-run-"); n != 9 {
			t.Errorf("the instance's run containers started %d times, want 9", n)
		}
	})

	t.Run("a repo with its own toolchain", func(t *testing.T) {
		t.Parallel()
		testToolchain(t, f)
	})

	t.Run("a refused credential is renewed, and the run goes on", func(t *testing.T) {
		t.Parallel()
		testRefused(t, f, name)
	})

	t.Run("one run of the real Claude Code", func(t *testing.T) {
		t.Parallel()
		if f.standInOutput == nil {
			t.Fatal("needs the stand-in's output of the first case")
		}
		c := f.newCase(t, f.runImage)
		f.card(t, c, "Run sh run.sh, then write the spec. kiln-case-tick-spec")
		core, done := f.core(t, c, "")
		f.waitEnded(t, c)
		f.stop(t, core, done)
		// Claude Code wrote the spec with Write, outside /work.
		spec := scriptedSpec
		f.assertEndedAt(t, c, store.Finished, store.Approval, &spec)

		// The card's text reached Claude Code with the instruction.
		reached := false
		for _, line := range strings.Split(must(t, "logs", f.upstream), "\n") {
			var r map[string]any
			if json.Unmarshal([]byte(line), &r) == nil && r["kind"] == "request" && r["case"] == "kiln-case-tick-spec" {
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

// testToolchain runs runs of a repo with its own toolchain and a cache
// (toolchain.go): a toy toolchain Dockerfile outside the repo that holds no
// cache, a toy cache Dockerfile whose command fills /opt/toy-cache from the
// repo's lock.txt, with a .dockerignore that lets only lock.txt into the
// context, and the stand-in layer, testdata/layer, with the stand-in for
// Claude Code. The first card's run is on kiln-run-<repo>, reads the cache as
// uid 1000 and cannot write into it, and has the cache's volume as its one
// mount, read-only; the fill had the repo's memory and the default network.
// A commit of a file that is not an input leaves the image and the volume as
// they are for the second card; a commit of lock.txt keeps the image and
// fills a new volume for the third, which reads the new text. A run of Kiln's
// own repo has no mount.
func testToolchain(t *testing.T, f *fixture) {
	c := f.newCase(t, f.standIn)
	repo := "ticktest-" + f.id
	toolchain, runImage, cacheImage, fill := toolchainImageOf(repo), runImageOf(repo), seal.CacheImage(repo), "kiln-fill-"+repo
	var ids []string
	t.Cleanup(func() {
		removeInstance(f.proxy.Name, c.instance)
		dockerQuiet("rm", "--force", fill)
		if volumes, err := docker(context.Background(), "volume", "ls", "--quiet", "--filter", "label=kiln.cache="+repo); err == nil {
			for _, v := range strings.Fields(volumes) {
				dockerQuiet("volume", "rm", v)
			}
		}
		dockerQuiet("rmi", runImage, toolchain, cacheImage)
		for _, id := range ids {
			dockerQuiet("rmi", id)
		}
	})
	imageIDs := func() string {
		t.Helper()
		run := must(t, "image", "inspect", "--format", "{{.Id}}", runImage)
		ids = append(ids, run, must(t, "image", "inspect", "--format", "{{.Id}}", toolchain),
			must(t, "image", "inspect", "--format", "{{.Id}}", cacheImage))
		return run
	}
	dir := t.TempDir()
	dockerfile, cacheFile := filepath.Join(dir, "toy.Dockerfile"), filepath.Join(dir, "toy-cache.Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM kiln-toolchain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, []byte("ARG TOOLCHAIN\nFROM ${TOOLCHAIN}\nCOPY lock.txt /opt/lock.txt\n"+
		`CMD ["sh", "-c", "set -eu; cp /opt/lock.txt /opt/toy-cache/lock.txt; chmod -R a+rX /opt/toy-cache"]`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	layer := filepath.Join(dir, "layer")
	if err := os.MkdirAll(layer, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"testdata/layer/Dockerfile", "testdata/claude/claude"} {
		if err := os.WriteFile(filepath.Join(layer, filepath.Base(name)), readFile(t, name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commitMain(t, c.repo, map[string]string{"lock.txt": "lock one\n", ".dockerignore": "*\n!lock.txt\n"})
	kilnCopy, _ := bareRepo(t, map[string]string{"NOTES.md": "# Notes of kiln\n"})
	cfg := f.config(c)
	cfg.Repo, cfg.Hosts = "", nil
	cfg.Repos = []settings.Repo{{Name: store.KilnRepo, Copy: kilnCopy, Own: true},
		{Name: repo, Copy: c.repo, Toolchain: &settings.Toolchain{Dockerfile: dockerfile,
			Cache: &settings.Cache{Dockerfile: cacheFile, Inputs: []string{"lock.txt"}, Path: "/opt/toy-cache"}},
			Memory: "6g", CPUs: 1}}
	cfg.Layer = layer
	core, done := f.start(t, child{Command: "core", Config: cfg})

	// atGate writes a card of a repo, with the word, and waits until its run
	// waits at its gate; end lets that run end and waits until it has.
	atGate := func(repo, word string) (string, int64) {
		t.Helper()
		s := f.open(t, c)
		card, err := s.CreateIn(repo, fmt.Sprintf("Add a line to NOTES.md. %s %s", word, gateWord), 0)
		if err == nil {
			_, err = s.Move(card.ID, waiting, store.CauseDeveloper)
		}
		s.Close()
		if err != nil {
			t.Fatal(err)
		}
		container := fmt.Sprintf("kiln-run-%s-%d-1", c.instance, card.ID)
		waitUntil(t, fmt.Sprintf("the run of card %d waits at its gate", card.ID), func() bool {
			_, err := docker(t.Context(), "exec", container, "test", "-e", "/tmp/waiting")
			return err == nil
		})
		return container, card.ID
	}
	end := func(container string, card int64) {
		t.Helper()
		must(t, "exec", container, "touch", "/tmp/gate")
		waitUntil(t, fmt.Sprintf("card %d waits no longer for a run", card), func() bool {
			s := f.open(t, c)
			cd, err := s.Card(card)
			s.Close()
			return err == nil && cd.WaitsFor != store.Run && instanceLeft(t, c.instance) == ""
		})
	}
	volumeName := regexp.MustCompile(`^kiln-cache-` + regexp.QuoteMeta(repo) + `-[0-9a-f]{64}$`)
	// cardRuns writes card n of the repo, waits until its run waits at its
	// gate, and returns its container's image, what uid 1000 reads of the
	// cache, and the cache's volume, its one mount, read-only, which uid 1000
	// cannot write into; then it lets the run end. The repo's run has its
	// memory, 6g, and its 1 CPU.
	cardRuns := func(n int) (string, string, string) {
		t.Helper()
		container, card := atGate(repo, fmt.Sprintf("kiln-case-toolchain-%d", n))
		assertRunLimits(t, container, "6442450944", 1)
		image := must(t, "inspect", "--format", "{{.Config.Image}}", container)
		cache := must(t, "exec", "--user", "1000", container, "cat", "/opt/toy-cache/lock.txt")
		if _, err := docker(t.Context(), "exec", "--user", "1000", container, "touch", "/opt/toy-cache/x"); err == nil ||
			!strings.Contains(err.Error(), "Read-only file system") {
			t.Errorf("run %d: a write into the cache gave %v, want Read-only file system", n, err)
		}
		var mounts []struct {
			Type, Name, Destination string
			RW                      bool
		}
		if err := json.Unmarshal([]byte(must(t, "inspect", "--format", "{{json .Mounts}}", container)), &mounts); err != nil {
			t.Fatal(err)
		}
		volume := ""
		if len(mounts) != 1 || mounts[0].Type != "volume" || !volumeName.MatchString(mounts[0].Name) ||
			mounts[0].Destination != "/opt/toy-cache" || mounts[0].RW {
			t.Errorf("run %d has the mounts %+v, want the cache's volume alone, read-only, at /opt/toy-cache", n, mounts)
		} else {
			volume = mounts[0].Name
		}
		end(container, card)
		return image, cache, volume
	}

	image, cache, first := cardRuns(1)
	if image != runImage || cache != "lock one" {
		t.Errorf("the first run is on %q and reads %q, want %s and lock one", image, cache, runImage)
	}
	if got := must(t, "inspect", "--format", "{{.HostConfig.Memory}} {{.HostConfig.NetworkMode}} {{.State.Status}} {{.State.ExitCode}} "+
		"{{range .Mounts}}{{.Name}} {{.RW}}{{end}}", fill); got != "6442450944 bridge exited 0 "+first+" true" {
		t.Errorf("the fill %s is %q, want 6g, bridge, exited 0 and the volume %s read-write", fill, got, first)
	}
	firstImage := imageIDs()
	// A run of Kiln's own repo in the same core has the defaults, and no
	// mount.
	container, card := atGate(store.KilnRepo, "kiln-case-toolchain-kiln")
	assertRunLimits(t, container, "4294967296", 2)
	if mounts := must(t, "inspect", "--format", "{{json .Mounts}}", container); mounts != "[]" {
		t.Errorf("Kiln's own run has the mounts %s, want none", mounts)
	}
	end(container, card)
	commitMain(t, c.repo, map[string]string{"NOTES.md": "# Notes\nA line on main.\n"})
	if image, cache, volume := cardRuns(2); image != runImage || cache != "lock one" || volume != first {
		t.Errorf("the second run is on %q with %s and reads %q, want %s with %s and lock one", image, volume, cache, runImage, first)
	}
	if id := imageIDs(); id != firstImage {
		t.Errorf("a commit of a file that is not an input made a new run image, %s after %s", id, firstImage)
	}
	// An unlabelled volume of the cache's name pattern, and a container that
	// was never started and mounts the first volume: neither is removed.
	unlabelled := seal.CacheVolume(repo, strings.Repeat("0", 64))
	hold := "kiln-hold-" + f.id
	must(t, "volume", "create", unlabelled)
	t.Cleanup(func() {
		dockerQuiet("rm", "--force", hold)
		dockerQuiet("volume", "rm", unlabelled)
	})
	must(t, "create", "--name", hold, "--mount", "type=volume,source="+first+",target=/x", toolchain, "true")
	commitMain(t, c.repo, map[string]string{"lock.txt": "lock two\n"})
	if image, cache, volume := cardRuns(3); image != runImage || cache != "lock two" || volume == first || volume == "" {
		t.Errorf("the third run is on %q with %s and reads %q, want %s with a new volume and lock two", image, volume, cache, runImage)
	}
	if id := imageIDs(); id != firstImage {
		t.Errorf("a commit of lock.txt, the cache's input alone, made a new run image, %s after %s", id, firstImage)
	}
	// The first volume is held by the created container, the unlabelled one
	// is not Kiln's: both stay after the fill of the second volume.
	cacheVolumes := func() []string {
		t.Helper()
		return strings.Fields(must(t, "volume", "ls", "--filter", "label=kiln.cache="+repo, "--format", "{{.Name}}"))
	}
	second := ""
	for _, v := range cacheVolumes() {
		if v != first {
			second = v
		}
	}
	if got := cacheVolumes(); len(got) != 2 || second == "" {
		t.Errorf("the cache volumes are %v after the fill of the second, want the first and the second", got)
	}
	if got := must(t, "volume", "ls", "--quiet", "--filter", "name="+unlabelled); got != unlabelled {
		t.Errorf("the unlabelled volume %s is not there: %q", unlabelled, got)
	}
	f.stop(t, core, done)
	out := core.Stdout.(*bytes.Buffer).String()
	if strings.Count(out, "kiln core: repo "+repo+": built "+runImage+"\n") != 1 ||
		strings.Count(out, "kiln core: repo "+repo+": filled kiln-cache-"+repo+"-") != 2 {
		t.Errorf("the core did not build %s once and fill the cache twice:\n%s", runImage, out)
	}
	if strings.Contains(out, "removed the old cache volume") {
		t.Errorf("the core removed a volume while a container held it:\n%s", out)
	}
	// With the container gone, a second core's start removes the first
	// volume and nothing else.
	must(t, "rm", hold)
	core, done = f.start(t, child{Command: "core", Config: cfg})
	waitUntil(t, "the first volume is removed", func() bool {
		return slices.Equal(cacheVolumes(), []string{second})
	})
	f.stop(t, core, done)
	if out := core.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "kiln core: repo "+repo+": removed the old cache volume "+first+"\n") {
		t.Errorf("the second core did not say it removed %s:\n%s", first, out)
	}
	if got := must(t, "volume", "ls", "--quiet", "--filter", "name="+unlabelled); got != unlabelled {
		t.Errorf("the unlabelled volume %s was removed: %q", unlabelled, got)
	}
	if got := must(t, "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}} {{range .Mounts}}{{.Name}}{{end}}", fill); got != "exited 0 "+second {
		t.Errorf("the fill %s is %q, want exited 0 mounting %s", fill, got, second)
	}
}

// assertRunLimits asserts a run container's memory, with swap the same, its
// CPUs and its cpuset of as many CPUs.
func assertRunLimits(t *testing.T, container, memory string, cpus int) {
	t.Helper()
	got := strings.Fields(must(t, "inspect", "--format", "{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}} {{.HostConfig.NanoCpus}} {{.HostConfig.CpusetCpus}}", container))
	want := []string{memory, memory, fmt.Sprint(cpus) + "000000000"}
	if len(got) != 4 || !slices.Equal(got[:3], want) || len(strings.Split(got[3], ",")) != cpus {
		t.Errorf("%s has the memory, memory with swap, CPUs and cpuset %q, want %q and a cpuset of %d CPUs", container, got, want, cpus)
	}
}

// testRefused is the whole path of a refused credential (ADR 0040), with the
// real Claude Code, a proxy that the case's core starts, and a stand-in
// credential command that prints a first credential, which the scripted
// upstream refuses after the run's first request, and a second one from then
// on. The run ends finished after its retries, with one push at the core's
// start and one after the refusal.
func testRefused(t *testing.T, f *fixture, name func(string) string) {
	proxy := name("proxy-refused")
	t.Cleanup(func() { dockerQuiet("rm", "--force", proxy) })
	c := f.newCase(t, f.runImage)
	c.proxy = proxy
	dir := t.TempDir()
	command := filepath.Join(dir, "credential.sh")
	refused, renewed := "fake-credential-"+f.id+"-refused", "fake-credential-"+f.id+"-renewed"
	if err := os.WriteFile(command, []byte(`d=$(dirname "$0")
echo run >>"$d/runs"
if [ "$(grep -c run "$d/runs")" -eq 1 ]; then echo `+refused+`; else echo `+renewed+`; fi
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c.credentialCommand = "sh " + shellWord(command)

	f.card(t, c, "Run sh run.sh, then write the spec. kiln-case-tick-refused")
	core, done := f.core(t, c, "")
	f.waitEnded(t, c)
	f.stop(t, core, done)
	spec := scriptedSpec
	f.assertEndedAt(t, c, store.Finished, store.Approval, &spec)

	if got := strings.Count(string(readFile(t, filepath.Join(dir, "runs"))), "run\n"); got != 2 {
		t.Errorf("the credential command ran %d times, want 2: at the start and after the refusal", got)
	}
	retries := 0
	for _, e := range readEvents(t, readFile(t, filepath.Join(c.data, "runs", c.run, "output.jsonl"))) {
		if e["type"] == "system" && e["subtype"] == "api_retry" && e["error_status"] == float64(401) {
			retries++
		}
	}
	if retries == 0 {
		t.Error("the run's output has no retry after a 401, so the test proves nothing")
	}
	var answers []string
	for _, line := range strings.Split(must(t, "logs", f.upstream), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["kind"] == "request" && r["case"] == "kiln-case-tick-refused" {
			key, _ := r["x_api_key"].(string)
			answers = append(answers, fmt.Sprint(r["answer"], " ", strings.TrimPrefix(key, "fake-credential-"+f.id)))
		}
	}
	// The first answer with the first credential, then refusals of it, and
	// from the second credential on none.
	first := slices.Index(answers, "refused -refused")
	renewedAt := slices.IndexFunc(answers, func(a string) bool { return strings.HasSuffix(a, " -renewed") })
	if len(answers) == 0 || answers[0] != "bash -refused" || first < 0 || renewedAt < first ||
		slices.Contains(answers[renewedAt:], "refused -renewed") || answers[len(answers)-1] != "done -renewed" {
		t.Errorf("the upstream answered %q, want bash, then refusals, then the rest with the second credential", answers)
	}
	if out := core.Stdout.(*bytes.Buffer).String() + core.Stderr.(*bytes.Buffer).String(); strings.Contains(out, refused) ||
		strings.Contains(out, renewed) {
		t.Errorf("the core printed the credential:\n%s", out)
	}
	if inspect := must(t, "inspect", proxy); strings.Contains(inspect, refused) || strings.Contains(inspect, renewed) {
		t.Error("the credential is in docker inspect of the proxy")
	}
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
	text     string // card 1's text
	noHosts  bool   // the core has no list of hosts
	listen   string // the address its core serves the screen on, if any
	proxy    string // the proxy of its core, when it is not the test's
	// The credential command of its core, when it is not the test's echo.
	credentialCommand string
	starts            *lines // the names of the instance's containers, once per start
	saves             *lines // the names of the run's containers, its save's included, once per start
	// What the instance's copy holds before the run: its refs and its objects.
	refs, objects string
}

// instruction is the file of the run's instruction in the data directory.
func (c *testCase) instruction() string {
	return filepath.Join(c.data, "instructions", c.run+".md")
}

// wantInstruction is what a spec run of card 1 is told.
func (c *testCase) wantInstruction() string {
	return instructions.Spec + "## The card\n\n" + c.text
}

// runOf is the ID of the nth run of card 1.
func (c *testCase) runOf(n int) string { return fmt.Sprintf("%s-1-%d", c.instance, n) }

// instructionOf is the file of the nth run's instruction.
func (c *testCase) instructionOf(n int) string {
	return filepath.Join(c.data, "instructions", c.runOf(n)+".md")
}

// move moves card 1 as the developer, while no core runs.
func (f *fixture) move(t *testing.T, c *testCase, to store.State) {
	t.Helper()
	s := f.open(t, c)
	defer s.Close()
	if _, err := s.Move(1, to, store.CauseDeveloper); err != nil {
		t.Fatal(err)
	}
}

// specAndApprove has a core give card 1 its spec run, and approves the spec
// as the developer once the core has stopped.
func (f *fixture) specAndApprove(t *testing.T, c *testCase) {
	t.Helper()
	core, done := f.core(t, c, "")
	f.waitState(t, c, finished, 1)
	f.stop(t, core, done)
	f.move(t, c, store.State{Stage: store.Implement, WaitsFor: store.Run})
}

// waitState waits until card 1 is in the state with nothing of the instance
// left on the engine, and fails when the instance's run containers started
// more than runs times.
func (f *fixture) waitState(t *testing.T, c *testCase, want store.State, runs int) {
	t.Helper()
	waitUntil(t, fmt.Sprintf("card 1 is in %s waiting for %s", want.Stage, want.WaitsFor), func() bool {
		if n := c.starts.count("kiln-run-"); n > runs {
			t.Fatalf("the instance's run containers started %d times, want %d", n, runs)
		}
		s := f.open(t, c)
		card, err := s.Card(1)
		s.Close()
		return err == nil && card.State == want && instanceLeft(t, c.instance) == ""
	})
	if n := c.starts.count("kiln-run-"); n != runs {
		t.Errorf("the instance's run containers started %d times, want %d", n, runs)
	}
}

// lastEntry is card 1's last entry.
func (f *fixture) lastEntry(t *testing.T, c *testCase) store.Entry {
	t.Helper()
	s := f.open(t, c)
	defer s.Close()
	h, err := s.History(1)
	if err != nil || len(h) == 0 {
		t.Fatal(h, err)
	}
	return h[len(h)-1]
}

// assertRun asserts the nth run of card 1.
func (f *fixture) assertRun(t *testing.T, c *testCase, n int, model, effort, commit string, ending store.Ending) {
	t.Helper()
	s := f.open(t, c)
	defer s.Close()
	want := store.RunRow{ID: c.runOf(n), Card: 1, Commit: commit, Model: model, Effort: effort, Ending: ending}
	if r, err := s.Run(c.runOf(n)); err != nil || r != want {
		t.Errorf("run %d is %+v (%v), want %+v", n, r, err, want)
	}
}

// document is the text of the document the nth run of card 1 wrote, the first
// of an implement run's two: its report.
func (f *fixture) document(t *testing.T, c *testCase, n int) string {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(c.data, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var text string
	if err := db.QueryRow("SELECT text FROM specs WHERE run = ? ORDER BY rowid LIMIT 1", c.runOf(n)).Scan(&text); err != nil {
		t.Fatalf("the document of run %d: %v", n, err)
	}
	return text
}

// assertLoop asserts the end of a case whose card 1 went past its spec: its
// entries after the spec's approval, its documents in order as "<kind> <run's
// number>", each run's document exactly as its record has it, every run
// ended, and nothing of the instance left. It returns the branch's tip.
func (f *fixture) assertLoop(t *testing.T, c *testCase, entries, documents []string) string {
	t.Helper()
	s := f.open(t, c)
	defer s.Close()
	h, err := s.History(1)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range h {
		got = append(got, fmt.Sprintf("%s/%s by %s", e.Stage, e.WaitsFor, e.Cause))
	}
	want := append([]string{"backlog/developer by developer", "spec/run by developer", "spec/approval by run"}, entries...)
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("the entries are %q, want %q", got, want)
	}
	// Each run is a document's run; an implement run has two documents.
	runs := map[string]bool{}
	for _, d := range documents {
		_, n, _ := strings.Cut(d, " ")
		runs[n] = true
	}
	if n, err := s.RunCount(1); err != nil || n != len(runs) {
		t.Errorf("card 1 has %d runs (%v), want %d", n, err, len(runs))
	}
	if r, err := s.InFlight(1); err != nil || r != nil {
		t.Errorf("a run is in flight: %+v (%v)", r, err)
	}

	db, err := sql.Open("sqlite3", filepath.Join(c.data, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT kind, run, text FROM specs WHERE card = 1 ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var docs []string
	for rows.Next() {
		var kind, run, text string
		if err := rows.Scan(&kind, &run, &text); err != nil {
			t.Fatal(err)
		}
		n := strings.TrimPrefix(run, c.instance+"-1-")
		docs = append(docs, kind+" "+n)
		// The document is the one its run wrote.
		name := "out/" + kind + ".md"
		if written := readTarEntry(t, filepath.Join(c.data, "runs", run, "documents.tar"), name); written != text {
			t.Errorf("the %s of run %s is kept as %q, written as %q", kind, n, text, written)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(docs, ", ") != strings.Join(documents, ", ") {
		t.Errorf("the documents are %q, want %q", docs, documents)
	}
	if left := instanceLeft(t, c.instance); left != "" {
		t.Errorf("left of the instance: %s", left)
	}
	return gitIn(t, c.repo, "rev-parse", "refs/heads/kiln/card-1")
}

// readTarEntry reads an entry of a tar.
func readTarEntry(t *testing.T, name, entry string) string {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("no %s in %s: %v", entry, name, err)
		}
		if h.Name == entry {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return string(data)
		}
	}
}

// newCase makes a case's data directory with its store, and its repo, and
// removes whatever of its instance is left on the engine afterwards.
func (f *fixture) newCase(t *testing.T, image string) *testCase {
	t.Helper()
	failIfStopped(t)
	c := &testCase{data: filepath.Join(t.TempDir(), "data"), image: image}
	s := f.open(t, c)
	var err error
	c.instance, err = s.Instance()
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	c.run = c.instance + "-1-1"
	t.Cleanup(func() { removeInstance(cmp.Or(c.proxy, f.proxy.Name), c.instance) })
	c.starts = watchEvents(t, "label=kiln.instance="+c.instance, "start", time.Now().Add(-5*time.Second))
	c.saves = watchEvents(t, "label=kiln.run="+c.run, "start", time.Now().Add(-5*time.Second))
	c.repo, c.commit = bareRepo(t, map[string]string{
		"CLAUDE.md": "# Try\n\nThe marker is " + f.marker + ".\n",
		"NOTES.md":  "# Notes\n",
		"run.sh":    "set -eu\nprintf 'A line from the run.\\n' >> NOTES.md\necho checked\n",
	})
	c.refs, c.objects = copyHolds(t, c.repo)
	return c
}

// copyHolds is what a bare repo holds: its refs, and the count of its objects.
func copyHolds(t *testing.T, repo string) (refs, objects string) {
	t.Helper()
	return gitIn(t, repo, "for-each-ref"), gitIn(t, repo, "count-objects", "-v")
}

// config is the configuration of a case's core.
func (f *fixture) config(c *testCase) Config {
	cfg := Config{Data: c.data, Repo: c.repo, Model: model, ImplementModel: implementModel, ReviewModel: reviewModel,
		Image: c.image, Proxy: cmp.Or(c.proxy, f.proxy.Name), ProxyImage: f.proxy.Image, ProxyNetwork: f.proxy.Network,
		Upstream: f.proxy.Upstream, CredentialCommand: cmp.Or(c.credentialCommand, "echo fake-credential-"+f.id),
		Listen: c.listen, Runs: DefaultRuns}
	if !c.noHosts {
		cfg.Hosts, cfg.WayOutImage, cfg.WayOutNetwork = hosts, f.wayOutImage, f.outward
	}
	return cfg
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
	c.text = text
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
		if n := c.starts.count("kiln-run-"); n > 1 {
			t.Fatalf("the instance's run containers started %d times, want 1", n)
		}
		s := f.open(t, c)
		card, err := s.Card(1)
		s.Close()
		return err == nil && card.WaitsFor != store.Run && instanceLeft(t, c.instance) == ""
	})
}

// assertEnded asserts the end state of a case with one run of card 1, ended
// as given: finished, with the stand-in's spec kept and the card waiting for
// approval, or failed, with no spec and the card under needs you. It returns
// the entry that ended the run.
func (f *fixture) assertEnded(t *testing.T, c *testCase, ending store.Ending) store.Entry {
	t.Helper()
	if ending == store.Finished {
		return f.assertEndedAt(t, c, ending, store.Approval, standInSpec(c.text))
	}
	return f.assertEndedAt(t, c, ending, store.Developer, nil)
}

// assertEndedAt asserts the end state of a case with one run of card 1, ended
// as given, with the card waiting for want and the spec kept, or none when it
// is nil. Nothing of the run is in the instance's copy, and its save
// container never started. It returns the entry that ended the run.
func (f *fixture) assertEndedAt(t *testing.T, c *testCase, ending store.Ending, want store.WaitsFor, spec *string) store.Entry {
	t.Helper()
	s := f.open(t, c)
	defer s.Close()
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
	if r, err := s.Run(c.run); err != nil || r.Ending != ending || r.Commit != c.commit || r.Model != model || r.Effort != "high" {
		t.Errorf("the run is %+v (%v), want %s from %s", r, err, ending, c.commit)
	}

	// The spec, exactly as written, in one row under the run's ID; or none.
	switch got, err := s.Spec(1); {
	case spec == nil && !errors.Is(err, sql.ErrNoRows):
		t.Errorf("a spec is kept (%v), want none", err)
	case spec != nil && (err != nil || got.Run != c.run || got.Commit != c.commit || got.Text != *spec):
		t.Errorf("the spec kept is %d bytes from %q (%v), want %d bytes from %s", len(got.Text), got.Run, err, len(*spec), c.run)
	}
	if n, want := countSpecs(t, c), map[bool]int{false: 0, true: 1}[spec != nil]; n != want {
		t.Errorf("%d rows in specs, want %d", n, want)
	}

	// Nothing of the run in the instance's copy: no branch, no new object.
	if refs, objects := copyHolds(t, c.repo); refs != c.refs || objects != c.objects {
		t.Errorf("the copy holds\n%s\n%s\nwas\n%s\n%s", refs, objects, c.refs, c.objects)
	}
	if at := gitIn(t, c.repo, "for-each-ref", "refs/heads/kiln/card-1"); at != "" {
		t.Errorf("refs/heads/kiln/card-1 exists: %s", at)
	}
	if n := c.saves.count("kiln-save-"); n != 0 {
		t.Errorf("the run's save container started %d times, want never", n)
	}
	if !strings.HasSuffix(last.Reason, ", not saved") && !strings.Contains(last.Reason, "container is gone") {
		t.Errorf("the reason %q does not end with not saved", last.Reason)
	}
	if data, err := os.ReadFile(filepath.Join(c.data, "runs", c.run, "result.json")); err == nil {
		var rec struct{ Saved string }
		if err := json.Unmarshal(data, &rec); err != nil || rec.Saved != "not saved" {
			t.Errorf("result.json has the save %q (%v), want not saved", rec.Saved, err)
		}
	} else if !strings.Contains(last.Reason, "container is gone") {
		t.Error(err)
	}

	// The run's instruction, written once, is kept.
	if got := string(readFile(t, c.instruction())); got != c.wantInstruction() {
		t.Errorf("the instruction is %q, want %q", got, c.wantInstruction())
	}

	if left := instanceLeft(t, c.instance); left != "" {
		t.Errorf("left of the instance: %s", left)
	}
	if n := c.starts.count("kiln-run-"); n != 1 {
		t.Errorf("the instance's run containers started %d times, want 1", n)
	}
	return last
}

// assertInstruction asserts, while the run waits at its gate, that it was
// told the installed Kiln's instruction, then the card's text.
func (f *fixture) assertInstruction(t *testing.T, c *testCase) {
	t.Helper()
	got := must(t, "exec", "kiln-run-"+c.run, "cat", "/kiln/instruction.md")
	first, _, _ := strings.Cut(instructions.Spec, "\n")
	if !strings.HasPrefix(got, first+"\n") || !strings.HasSuffix(got, "\n## The card\n\n"+c.text) {
		t.Errorf("the run was told\n%s\nwant %q first and %q last", got, first, "## The card\n\n"+c.text)
	}
}

// countSpecs counts the rows in a case's specs.
func countSpecs(t *testing.T, c *testCase) int {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(c.data, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM specs").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// damage cuts the record's documents.tar off inside the spec's data, as a
// disk or a hand on the record would.
func damage(t *testing.T, c *testCase) {
	t.Helper()
	name := filepath.Join(c.data, "runs", c.run, "documents.tar")
	data := readFile(t, name)
	r := bytes.NewReader(data)
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("no out/spec.md in documents.tar: %v", err)
		}
		if h.Name == "out/spec.md" {
			cut := int64(len(data)) - int64(r.Len()) + h.Size/2
			t.Logf("documents.tar of %d bytes, cut to %d, inside the spec of %d bytes", len(data), cut, h.Size)
			if err := os.Truncate(name, cut); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			t.Fatal(err)
		}
	}
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

// proxyOn reports whether a proxy container is attached to a network.
func proxyOn(t *testing.T, proxy, network string) bool {
	t.Helper()
	var networks map[string]json.RawMessage
	if err := json.Unmarshal([]byte(must(t, "inspect", "--format", "{{json .NetworkSettings.Networks}}", proxy)), &networks); err != nil {
		t.Fatal(err)
	}
	_, ok := networks[network]
	return ok
}

// assertKept asserts that a run's container and network are there.
func assertKept(t *testing.T, id string) {
	t.Helper()
	if containerState(t, id) == "" {
		t.Error("the run's container is gone")
	}
	if _, err := docker(t.Context(), "network", "inspect", "kiln-run-"+id); err != nil {
		t.Error("the run's network is gone")
	}
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

// count counts the lines that begin with prefix.
func (l *lines) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range strings.Fields(l.buf.String()) {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// watchEvents follows the engine's events of one kind for the containers that
// match a filter, such as label=kiln.instance=<instance>, from since on, and
// collects the containers' names. The events are followed as they come: the
// engine keeps only the last few hundred for a later --since, and other work
// on it can push one out of them.
func watchEvents(t *testing.T, filter, event string, since time.Time) *lines {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := &lines{}
	cmd := exec.CommandContext(ctx, "docker", "events", "--since", strconv.FormatInt(since.Unix(), 10),
		"--filter", "type=container", "--filter", filter, "--filter", "event="+event,
		"--format", "{{.Actor.Attributes.name}}")
	cmd.Stdout = l
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	return l
}

// seen is what the stand-in saw of the repo's Claude Code configuration in
// the nth run of card 1, from its record.
func (f *fixture) seen(t *testing.T, c *testCase, n int) string {
	t.Helper()
	return readTarEntry(t, filepath.Join(c.data, "runs", c.runOf(n), "documents.tar"), "out/seen.txt")
}

// mainTrees follows the starts of an instance's run containers with their
// label kiln.main-tree, as "<name>=<label>".
type mainTrees struct{ *lines }

// watchMainTrees follows a case's run containers from now on.
func watchMainTrees(t *testing.T, c *testCase) mainTrees {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := &lines{}
	cmd := exec.CommandContext(ctx, "docker", "events", "--since", strconv.FormatInt(time.Now().Add(-5*time.Second).Unix(), 10),
		"--filter", "type=container", "--filter", "label=kiln.instance="+c.instance, "--filter", "event=start",
		"--format", `{{.Actor.Attributes.name}}={{index .Actor.Attributes "kiln.main-tree"}}`)
	cmd.Stdout = l
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	return mainTrees{l}
}

// laidOver are the names of the run containers that started with the label,
// in order.
func (m mainTrees) laidOver() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var names []string
	for _, line := range strings.Fields(m.buf.String()) {
		if name, tree, _ := strings.Cut(line, "="); strings.HasPrefix(name, "kiln-run-") && tree != "" {
			names = append(names, name)
		}
	}
	return names
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

// freeAddress is a loopback address with a port that is free now.
func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// getJSON asks the screen's API for url and decodes its answer into v.
func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("GET %s: %v", url, err)
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
	write(t, work, files)
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

// build builds an image from a context, relative to the package, with a
// Dockerfile relative to the repo's root, and removes it afterwards.
func build(t *testing.T, tag, context, dockerfile string, args ...string) {
	t.Helper()
	t.Cleanup(func() { dockerQuiet("rmi", tag) })
	must(t, append(append([]string{"build", "--quiet", "--tag", tag, "--file", "../../" + dockerfile}, args...), context)...)
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
