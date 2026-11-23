package tick

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/launcher"
	"github.com/TheEisbaer/kiln/internal/store"
)

// The tests of the core's update (update.go): a core under a launcher builds
// the program of main's head, copies the store and asks to be replaced; the
// previous core puts the copy back after a failed update.

// kilnModule is the module of the test binary's build information, which a
// repo of Kiln's own names.
const kilnModule = "github.com/TheEisbaer/kiln"

// kilnBench is a bench whose repo has a go.mod with a module, and no origin,
// and whose core's version is main's first head, A.
type kilnBench struct {
	*bench
	a string
}

// newKilnBench makes a kilnBench whose go.mod names the module.
func newKilnBench(t *testing.T, module string) *kilnBench {
	t.Helper()
	b := newBench(t, 3)
	repo, commit := bareRepo(t, map[string]string{"NOTES.md": "# Notes\n", "go.mod": "module " + module + "\n\ngo 1.27\n"})
	gitIn(t, repo, "remote", "remove", "origin")
	b.cfg.Repo, b.commit = repo, commit
	b.cfg.Version, b.cfg.ToolchainImage = commit, "kiln-toolchain"
	return &kilnBench{bench: b, a: commit}
}

// move commits files on main of the repo, on top of from, or of main's head
// when from is empty, and returns the commit.
func (k *kilnBench) move(t *testing.T, from string, files map[string]string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	gitIn(t, filepath.Dir(work), "clone", "--quiet", k.cfg.Repo, work)
	if from != "" {
		gitIn(t, work, "checkout", "--quiet", "--orphan", "other")
		gitIn(t, work, "rm", "--quiet", "-r", "--cached", ".")
	}
	write(t, work, files)
	gitIn(t, work, "add", "--all")
	gitIn(t, work, "commit", "--quiet", "--message", "moved")
	gitIn(t, work, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/main")
	return gitIn(t, work, "rev-parse", "HEAD")
}

// programs is the bench's directory of programs.
func (k *kilnBench) programs() string { return filepath.Join(k.cfg.Data, "programs") }

// twoCards writes two cards in the backlog, and returns their texts.
func (k *kilnBench) twoCards(t *testing.T) []string {
	t.Helper()
	k.with(t, func(s *store.Store) {
		for _, text := range []string{"The first card.", "The second card."} {
			if _, err := s.Create(text); err != nil {
				t.Fatal(err)
			}
		}
	})
	return []string{"The first card.", "The second card."}
}

// cardsIn are the texts of the cards of a store's file, all of them read.
func cardsIn(t *testing.T, path string) []string {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	board, err := s.Board()
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, c := range board {
		texts = append(texts, c.Text)
	}
	return texts
}

// handing is a core in the test's process with a socket for its launcher.
type handing struct {
	ours *os.File
	done chan error
	out  *syncBuffer
	stop func()
}

// startHanding starts a core of the bench on the engine, with a tick of 5 ms,
// and a launcher on a socket unless none is asked for.
func startHanding(t *testing.T, cfg Config, e *fakeEngine, withLauncher bool) *handing {
	t.Helper()
	ours, theirs := socketPair(t)
	ctx, cancel := context.WithCancel(t.Context())
	h := &handing{ours: ours, done: make(chan error, 1), out: &syncBuffer{}, stop: cancel}
	var l reporter
	if withLauncher {
		l = &socketLauncher{f: theirs, gone: make(chan struct{})}
	}
	go func() { h.done <- runCore(ctx, cfg, h.out, h.out, e, 5*time.Millisecond, realClock{}, l) }()
	t.Cleanup(func() {
		cancel()
		<-h.done
		h.done <- nil
	})
	return h
}

// line reads the core's next line to its launcher, and fails without one.
func (h *handing) line(t *testing.T) string {
	t.Helper()
	line, err := readLine(h.ours, waitLimit)
	if err != nil {
		t.Fatalf("the core wrote %q, %v to its launcher\n%s", line, err, h.out)
	}
	return line
}

// noLine asserts that the core writes nothing to its launcher for d.
func (h *handing) noLine(t *testing.T, d time.Duration) {
	t.Helper()
	if line, err := readLine(h.ours, d); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the core wrote %q, %v to its launcher\n%s", line, err, h.out)
	}
}

// returned waits for the core to return, and wants nil.
func (h *handing) returned(t *testing.T) {
	t.Helper()
	select {
	case err := <-h.done:
		h.done <- err
		if err != nil {
			t.Fatalf("the core returned %v\n%s", err, h.out)
		}
	case <-time.After(waitLimit):
		t.Fatalf("the core did not return\n%s", h.out)
	}
}

// ended stops the core, as Ctrl-C does, and waits for it.
func (h *handing) ended(t *testing.T) {
	t.Helper()
	h.stop()
	h.returned(t)
}

// standInProgram is what the stand-in build writes for a version.
func standInProgram(version string) string { return "#!/bin/sh\necho " + version + "\n" }

// leftovers are the files in the programs and data directories that a step
// writes before its rename.
func leftovers(t *testing.T, k *kilnBench) []string {
	t.Helper()
	var left []string
	for _, pattern := range []string{"*.new", "*.restore"} {
		for _, dir := range []string{k.programs(), k.cfg.Data} {
			m, err := filepath.Glob(filepath.Join(dir, pattern))
			if err != nil {
				t.Fatal(err)
			}
			left = append(left, m...)
		}
	}
	return left
}

// TestUpdate moves main of Kiln's own repo past the core's version: the core
// builds the program of the new head, copies the store with both cards beside
// its version, asks to be replaced by the program and returns, and the run in
// flight goes on in its container. A change to the images' files is said.
func TestUpdate(t *testing.T) {
	for _, c := range []struct {
		name   string
		files  map[string]string
		images string
	}{
		{"the program", map[string]string{"NOTES.md": "# Notes\nMoved.\n"}, ""},
		{"the images", map[string]string{"images/run/Dockerfile": "FROM scratch\n"}, "(images/run/Dockerfile); the update builds only the program, rebuild the images by hand"},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newKilnBench(t, kilnModule)
			setFetchEvery(t, 20*time.Millisecond)
			var inFlight int64
			k.with(t, func(s *store.Store) {
				inFlight = k.card(t, s, store.Spec)
				if _, err := s.Create("A card in the backlog."); err != nil {
					t.Fatal(err)
				}
			})
			e := newFakeEngine()
			h := startHanding(t, k.cfg, e, true)
			if line := h.line(t); line != "healthy" {
				t.Fatalf("the core wrote %q", line)
			}
			waitUntil(t, "the run starts", func() bool { return slices.Contains(e.snapshot().started, inFlight) })
			b := k.move(t, "", c.files)
			program := filepath.Join(k.programs(), "kiln-"+b)
			if line := h.line(t); line != "replace "+program {
				t.Fatalf("the core wrote %q, want replace %s\n%s", line, program, h.out)
			}
			h.returned(t)

			if got := string(readFile(t, program)); got != standInProgram(b) {
				t.Errorf("the program is %q", got)
			}
			if fi, err := os.Stat(program); err != nil || fi.Mode().Perm() != 0o700 {
				t.Errorf("the program: %v, %v", fi, err)
			}
			if got := cardsIn(t, filepath.Join(k.programs(), "kiln-"+k.a+".db")); len(got) != 2 {
				t.Errorf("the copy holds %q", got)
			}
			e.mu.Lock()
			builds, removed, status := slices.Clone(e.builds), slices.Clone(e.removed), e.containers[fmt.Sprintf("%s-%d-1", k.instance, inFlight)]
			e.mu.Unlock()
			if len(builds) != 1 || builds[0].Version != b || builds[0].Image != "kiln-toolchain" || builds[0].Instance != k.instance ||
				builds[0].GOOS != runtime.GOOS || builds[0].GOARCH != runtime.GOARCH {
				t.Errorf("the builds: %+v", builds)
			}
			if status != "running" || len(removed) != 0 {
				t.Errorf("the run's container is %q, and the runs removed are %v", status, removed)
			}
			if left := leftovers(t, k); len(left) != 0 {
				t.Errorf("left behind: %v", left)
			}
			out := h.out.String()
			for _, want := range []string{
				"kiln core: main is at " + b + ", past " + k.a + "; building the core of " + b + "\n",
				"kiln core: asked to be replaced by " + program + "; the core ends, and the runs go on\n",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("the core did not say %q:\n%s", want, out)
				}
			}
			if said := strings.Contains(out, "the images' files changed between "+k.a+" and "+b+" "+c.images); c.images != "" && !said {
				t.Errorf("the core did not say that the images' files changed:\n%s", out)
			} else if c.images == "" && strings.Contains(out, "the images' files") {
				t.Errorf("the core said the images' files changed:\n%s", out)
			}
		})
	}
}

// waitRounds waits for n ticks of the core, and for n rounds of fetching.
func waitRounds(t *testing.T, e *fakeEngine, n int) {
	t.Helper()
	from := time.Now()
	e.waitTicks(t, n)
	waitUntil(t, fmt.Sprintf("%d rounds of fetching", n), func() bool { return time.Since(from) > time.Duration(n)*fetchEvery })
}

// assertNoUpdate asserts that the core built nothing and made no programs.
func assertNoUpdate(t *testing.T, k *kilnBench, e *fakeEngine) {
	t.Helper()
	e.mu.Lock()
	builds := len(e.builds)
	e.mu.Unlock()
	if builds != 0 {
		t.Errorf("the core built %d times", builds)
	}
	if _, err := os.Stat(k.programs()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the programs directory: %v", err)
	}
}

// TestNoUpdate leaves the core as it is: in another repo; when main's head is
// the core's version; and when main no longer holds the version.
func TestNoUpdate(t *testing.T) {
	for _, c := range []struct {
		name, module string
		move         func(t *testing.T, k *kilnBench)
	}{
		{"another repo", "example.com/other", func(t *testing.T, k *kilnBench) {
			k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
		}},
		{"the same commit", kilnModule, func(*testing.T, *kilnBench) {}},
		{"not an ancestor", kilnModule, func(t *testing.T, k *kilnBench) {
			k.move(t, "orphan", map[string]string{"go.mod": "module " + kilnModule + "\n"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newKilnBench(t, c.module)
			setFetchEvery(t, 20*time.Millisecond)
			k.twoCards(t)
			e := newFakeEngine()
			h := startHanding(t, k.cfg, e, true)
			if line := h.line(t); line != "healthy" {
				t.Fatalf("the core wrote %q", line)
			}
			c.move(t, k)
			waitRounds(t, e, 10)
			h.noLine(t, 50*time.Millisecond)
			h.ended(t)
			assertNoUpdate(t, k, e)
			if out := h.out.String(); strings.Contains(out, "update") || strings.Contains(out, "building") {
				t.Errorf("the core said:\n%s", out)
			}
		})
	}
}

// TestUpdateWithoutLauncher moves main of Kiln's own repo under a core
// without a launcher: it says once that it does not update itself, and
// builds nothing.
func TestUpdateWithoutLauncher(t *testing.T) {
	k := newKilnBench(t, kilnModule)
	setFetchEvery(t, 20*time.Millisecond)
	k.twoCards(t)
	e := newFakeEngine()
	h := startHanding(t, k.cfg, e, false)
	e.waitTicks(t, 1)
	b := k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
	waitRounds(t, e, 10)
	k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved again.\n"})
	waitRounds(t, e, 10)
	h.ended(t)
	assertNoUpdate(t, k, e)
	line := "kiln core: main is at " + b + ", past this core's version; a core without a launcher does not update itself\n"
	if out := h.out.String(); strings.Count(out, "a core without a launcher does not update itself") != 1 || !strings.Contains(out, line) {
		t.Errorf("the core said:\n%s", out)
	}
}

// TestUpdateByHand has a core built by hand: main moving alone does not
// update it, and once one of its cards has merged it updates, with the copy
// kiln-by-hand.db.
func TestUpdateByHand(t *testing.T) {
	p := newPublishing(t)
	p.cfg.ToolchainImage = "kiln-toolchain"
	setFetchEvery(t, 20*time.Millisecond)
	gomod := map[string]string{"go.mod": "module " + kilnModule + "\n"}
	p.moveOrigin(t, gomod)
	e := newFakeEngine()
	h := startHanding(t, p.cfg, e, true)
	if line := h.line(t); line != "healthy" {
		t.Fatalf("the core wrote %q", line)
	}
	main := p.moveOrigin(t, map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
	waitUntil(t, "main is fetched", func() bool { return gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/main") == main })
	waitRounds(t, e, 10)
	h.noLine(t, 50*time.Millisecond)
	h.ended(t)
	if e.mu.Lock(); len(e.builds) != 0 {
		t.Errorf("the core built %d times before a card merged", len(e.builds))
	}
	e.mu.Unlock()

	p.mode(t, 0, "merge")
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		p.publish(t, s, card, false)
	})
	h = startHanding(t, p.cfg, e, true)
	if line := h.line(t); line != "healthy" {
		t.Fatalf("the core wrote %q", line)
	}
	line := h.line(t)
	b := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/main")
	if want := "replace " + filepath.Join(p.cfg.Data, "programs", "kiln-"+b); line != want {
		t.Fatalf("the core wrote %q, want %q\n%s", line, want, h.out)
	}
	h.returned(t)
	if !strings.Contains(h.out.String(), fmt.Sprintf("card %d: merged\n", card)) ||
		!strings.Contains(h.out.String(), "this core was built by hand, so it cannot tell whether the images' files changed") {
		t.Errorf("the core said:\n%s", h.out)
	}
	if got := cardsIn(t, filepath.Join(p.cfg.Data, "programs", "kiln-by-hand.db")); len(got) != 1 {
		t.Errorf("the copy holds %q", got)
	}
}

// TestNoWriteAfterCopy writes a card through the screen once the store is
// copied: the write is refused, and the copy and the store hold the same
// cards.
func TestNoWriteAfterCopy(t *testing.T) {
	k := newKilnBench(t, kilnModule)
	setFetchEvery(t, 20*time.Millisecond)
	k.cfg.Listen = freeAddress(t)
	k.twoCards(t)
	var posted error
	atPoint = func(point string) {
		if point != "update-copy-new" {
			return
		}
		// The screen is stopped: the write does not even reach it.
		resp, err := http.Post("http://"+k.cfg.Listen+"/api/cards", "application/json", strings.NewReader(`{"text":"after the copy"}`))
		if err == nil {
			resp.Body.Close()
			posted = fmt.Errorf("answered %s", resp.Status)
			return
		}
		posted = fmt.Errorf("refused: %v", err)
	}
	t.Cleanup(func() { atPoint = nil })
	e := newFakeEngine()
	h := startHanding(t, k.cfg, e, true)
	if line := h.line(t); line != "healthy" {
		t.Fatalf("the core wrote %q", line)
	}
	k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
	if line := h.line(t); !strings.HasPrefix(line, "replace ") {
		t.Fatalf("the core wrote %q", line)
	}
	h.returned(t)
	if posted == nil || !strings.HasPrefix(posted.Error(), "refused") {
		t.Errorf("a card written after the copy: %v", posted)
	}
	copied := cardsIn(t, filepath.Join(k.programs(), "kiln-"+k.a+".db"))
	if kept := cardsIn(t, filepath.Join(k.cfg.Data, "store.db")); !slices.Equal(copied, kept) || len(kept) != 2 {
		t.Errorf("the copy holds %q, the store %q", copied, kept)
	}
}

// TestUpdateRemovesByName fills the programs directory before an update: the
// programs and copies more than one step back go, and every other name, a
// directory, the running core's program and copy and the new program stay.
func TestUpdateRemovesByName(t *testing.T) {
	k := newKilnBench(t, kilnModule)
	setFetchEvery(t, 20*time.Millisecond)
	k.twoCards(t)
	c, d := strings.Repeat("c", 40), strings.Repeat("d", 64)
	dir := k.programs()
	if err := os.MkdirAll(filepath.Join(dir, "kiln-"+d), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kiln-" + c, "kiln-" + c + ".db", "kiln-by-hand.db", "notes.txt", "kiln-" + c + ".bak", "kiln-" + k.a} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e := newFakeEngine()
	h := startHanding(t, k.cfg, e, true)
	if line := h.line(t); line != "healthy" {
		t.Fatalf("the core wrote %q", line)
	}
	b := k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
	if line := h.line(t); !strings.HasPrefix(line, "replace ") {
		t.Fatalf("the core wrote %q", line)
	}
	h.returned(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := []string{"kiln-" + c + ".bak", "kiln-" + d, "kiln-" + k.a, "kiln-" + k.a + ".db", "kiln-" + b, "notes.txt"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the directory holds %q, want %q", got, want)
	}
}

// TestModulePath reads the module path of a go.mod.
func TestModulePath(t *testing.T) {
	for gomod, want := range map[string]string{
		"module github.com/TheEisbaer/kiln\n\ngo 1.27\n": kilnModule,
		"// a comment\nmodule \"example.com/quoted\"\n":  "example.com/quoted",
		"module example.com/commented // the module\n":   "example.com/commented",
		"go 1.27\n\nrequire example.com/x v1.0.0\n":      "",
		"": "",
		"// module example.com/not\nmodule example.com/real\n": "example.com/real",
		"modules example.com/not\n":                            "",
	} {
		if got := modulePath(gomod); got != want {
			t.Errorf("modulePath(%q) = %q, want %q", gomod, got, want)
		}
	}
}

// launched is the test binary as a core under a stand-in launcher: a socket
// as its file descriptor 3, and KILN_LAUNCHER=3.
type launched struct {
	cmd            *exec.Cmd
	ours           *os.File
	done           chan int
	stdout, stderr *syncBuffer
}

// launch starts the test binary as a core under a stand-in launcher, with
// extra variables.
func launch(t *testing.T, ch child, env ...string) *launched {
	t.Helper()
	spec, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	ours, theirs := socketPair(t)
	l := &launched{cmd: exec.Command(os.Args[0]), ours: ours, done: make(chan int, 1), stdout: &syncBuffer{}, stderr: &syncBuffer{}}
	l.cmd.Env = append(append(os.Environ(), asKilnVar+"="+string(spec), "KILN_LAUNCHER=3"), env...)
	l.cmd.Stdout, l.cmd.Stderr = l.stdout, l.stderr
	l.cmd.ExtraFiles = []*os.File{theirs}
	if err := l.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	theirs.Close()
	go func() {
		err := l.cmd.Wait()
		var exit *exec.ExitError
		switch {
		case err == nil:
			l.done <- 0
		case errors.As(err, &exit):
			l.done <- exit.ExitCode()
		default:
			l.done <- -1
		}
	}()
	t.Cleanup(func() {
		l.cmd.Process.Signal(syscall.SIGKILL)
		code := <-l.done
		l.done <- code
	})
	return l
}

// exited waits for the core to exit, and wants the status.
func (l *launched) exited(t *testing.T, want int) {
	t.Helper()
	select {
	case code := <-l.done:
		l.done <- code
		if code != want {
			t.Fatalf("the core exited with %d, want %d\n%s%s", code, want, l.stdout, l.stderr)
		}
	case <-time.After(waitLimit):
		t.Fatalf("the core did not exit\n%s%s", l.stdout, l.stderr)
	}
}

// until reads the core's lines to its launcher until one begins with prefix,
// and returns it.
func (l *launched) until(t *testing.T, prefix string) string {
	t.Helper()
	for {
		line, err := readLine(l.ours, waitLimit)
		if err != nil {
			t.Fatalf("the core wrote %q, %v, and no line %q\n%s%s", line, err, prefix, l.stdout, l.stderr)
		}
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
}

// copyStore makes the copy of the bench's store beside its version, as the
// hand-over does.
func (k *kilnBench) copyStore(t *testing.T) string {
	t.Helper()
	if err := os.MkdirAll(k.programs(), 0o700); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(k.programs(), "kiln-"+k.a+".db")
	s, lock, err := openStore(k.cfg.Data)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := s.CopyAndClose(copied); err != nil {
		t.Fatal(err)
	}
	return copied
}

// playNewer plays a newer core on the bench's store: it moves the store
// forward a version, with a table and a card of its own, and leaves a hot
// journal beside it, as a kill in the middle of a large transaction of that
// core's would.
func (k *kilnBench) playNewer(t *testing.T) {
	t.Helper()
	path := filepath.Join(k.cfg.Data, "store.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		fmt.Sprintf("PRAGMA user_version = %d", v+1),
		"CREATE TABLE newer (id INTEGER PRIMARY KEY, data BLOB) STRICT",
		"INSERT INTO cards (text, stage, waits_for) VALUES ('A card of the newer core.', 'backlog', 'developer')",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	// A transaction larger than the page cache writes the journal and then
	// pages of the file; the two files are taken as they are in the middle
	// of it, and the transaction is rolled back.
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{"PRAGMA cache_size = 2", "BEGIN",
		"INSERT INTO newer (data) SELECT randomblob(4000) FROM (WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 200) SELECT i FROM n)",
		"UPDATE cards SET text = 'changed in the middle'"} {
		if _, err := conn.ExecContext(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	file, journal := readFile(t, path), readFile(t, path+"-journal")
	if _, err := conn.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	db.Close()
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-journal", journal, 0o600); err != nil {
		t.Fatal(err)
	}
}

// failedEnv are the variables the launcher gives the previous program after
// the update to b failed.
func (k *kilnBench) failedEnv(b string) []string {
	return []string{envUpdateFailed + "=the new core " + filepath.Join(k.programs(), "kiln-"+b) + " did not report healthy within 2m0s",
		envUpdateFailedProgram + "=" + filepath.Join(k.programs(), "kiln-"+b)}
}

// TestRestore starts the previous core after a failed update to main's head,
// over a store the newer core moved forward and left a journal beside: it
// says why, puts the copy back before it opens the store, reports, reads
// every card of the copy, and does not update to the failed commit again. Its
// children see neither variable.
func TestRestore(t *testing.T) {
	k := newKilnBench(t, kilnModule)
	cards := k.twoCards(t)
	copied := k.copyStore(t)
	k.playNewer(t)
	b := k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
	probe := filepath.Join(t.TempDir(), "probe")
	builds := filepath.Join(t.TempDir(), "builds")
	k.cfg.CredentialCommand = "env > " + shellWord(probe) + "; echo stand-in-credential"
	l := launch(t, child{Command: "core", Config: k.cfg, NoEngine: true, Builds: builds}, k.failedEnv(b)...)
	l.until(t, "healthy")
	// Ten ticks of the core, and more than ten rounds of fetching.
	if line, err := readLine(l.ours, 50*testInterval); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the core wrote %q, %v to its launcher", line, err)
	}
	l.cmd.Process.Signal(os.Interrupt)
	l.exited(t, 0)

	out := l.stdout.String()
	for _, want := range []string{
		"kiln core: the update failed: the new core " + filepath.Join(k.programs(), "kiln-"+b) + " did not report healthy within 2m0s\n",
		"kiln core: the store is put back from " + copied + "\n",
		"kiln core: " + b + " failed as an update; this core waits for main to move past it\n",
	} {
		if strings.Count(out, want) != 1 {
			t.Errorf("the core did not say %q once:\n%s%s", want, out, l.stderr)
		}
	}
	if got := cardsIn(t, filepath.Join(k.cfg.Data, "store.db")); !slices.Equal(got, cards) {
		t.Errorf("the store holds %q, want %q", got, cards)
	}
	if _, err := os.Stat(filepath.Join(k.cfg.Data, "store.db-journal")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the journal: %v", err)
	}
	if _, err := os.Stat(builds); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the core built: %v", err)
	}
	if env := string(readFile(t, probe)); strings.Contains(env, envUpdateFailed) {
		t.Errorf("the credential command saw:\n%s", env)
	}
	if left := leftovers(t, k); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// TestUpdateKilled kills the core at each step of the update and of the
// restore: the store opens and holds every card afterwards, and the same core
// started again finishes the step.
func TestUpdateKilled(t *testing.T) {
	for _, point := range []string{"update-built-new", "update-built", "update-stopped", "update-copy-new", "update-copied", "update-removed"} {
		t.Run(point, func(t *testing.T) {
			k := newKilnBench(t, kilnModule)
			cards := k.twoCards(t)
			b := k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
			builds := filepath.Join(t.TempDir(), "builds")
			ch := child{Command: "core", Config: k.cfg, NoEngine: true, Builds: builds, Hook: "tick:" + point + ":exit"}
			launch(t, ch).exited(t, exitAtPoint)
			if got := cardsIn(t, filepath.Join(k.cfg.Data, "store.db")); !slices.Equal(got, cards) {
				t.Errorf("after the kill the store holds %q", got)
			}
			left := map[string]string{"update-built-new": "kiln-" + b + ".new", "update-copy-new": "kiln-" + k.a + ".db.new"}[point]
			if left != "" {
				if _, err := os.Stat(filepath.Join(k.programs(), left)); err != nil {
					t.Errorf("after the kill: %v", err)
				}
			}
			before := strings.Count(string(readFile(t, builds)), "\n")

			ch.Hook = ""
			l := launch(t, ch)
			program := filepath.Join(k.programs(), "kiln-"+b)
			if line := l.until(t, "replace "); line != "replace "+program {
				t.Errorf("the core wrote %q", line)
			}
			l.exited(t, 0)
			if got := cardsIn(t, filepath.Join(k.programs(), "kiln-"+k.a+".db")); !slices.Equal(got, cards) {
				t.Errorf("the copy holds %q", got)
			}
			if got := string(readFile(t, program)); got != standInProgram(b) {
				t.Errorf("the program is %q", got)
			}
			after := strings.Count(string(readFile(t, builds)), "\n")
			if want := map[bool]int{true: before + 1, false: before}[point == "update-built-new"]; after != want {
				t.Errorf("the core built %d times, then %d", before, after)
			}
			if left := leftovers(t, k); len(left) != 0 {
				t.Errorf("left behind: %v", left)
			}
		})
	}
	for _, point := range []string{"restore-new", "restore-journal", "restore-done"} {
		t.Run(point, func(t *testing.T) {
			k := newKilnBench(t, kilnModule)
			cards := k.twoCards(t)
			copied := k.copyStore(t)
			k.playNewer(t)
			b := k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
			ch := child{Command: "core", Config: k.cfg, NoEngine: true, Hook: "tick:" + point + ":exit"}
			launch(t, ch, k.failedEnv(b)...).exited(t, exitAtPoint)
			if got := cardsIn(t, copied); !slices.Equal(got, cards) {
				t.Errorf("after the kill the copy holds %q", got)
			}
			if point == "restore-done" {
				if got := cardsIn(t, filepath.Join(k.cfg.Data, "store.db")); !slices.Equal(got, cards) {
					t.Errorf("after the kill the store holds %q", got)
				}
			}

			ch.Hook = ""
			l := launch(t, ch, k.failedEnv(b)...)
			l.until(t, "healthy")
			l.cmd.Process.Signal(os.Interrupt)
			l.exited(t, 0)
			if got := cardsIn(t, filepath.Join(k.cfg.Data, "store.db")); !slices.Equal(got, cards) {
				t.Errorf("the store holds %q", got)
			}
			if _, err := os.Stat(filepath.Join(k.cfg.Data, "store.db-journal")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the journal: %v", err)
			}
			if left := leftovers(t, k); len(left) != 0 {
				t.Errorf("left behind: %v", left)
			}
		})
	}
}

// TestUpdateThroughLauncher runs the update through the real launcher, with
// the test binary as the core and a new program that exits at once: the
// launcher replaces the core, falls back to it after the new one has failed,
// and the core puts its copy back and reports. Only one replace is asked.
func TestUpdateThroughLauncher(t *testing.T) {
	k := newKilnBench(t, kilnModule)
	cards := k.twoCards(t)
	b := k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
	spec, err := json.Marshal(child{Command: "core", Config: k.cfg, NoEngine: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(asKilnVar, string(spec))
	// The launcher gives its cores its own standard output, which the test
	// reads.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, was := &syncBuffer{}, os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = was })
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		io.Copy(stdout, r)
	}()
	signals := make(chan os.Signal, 1)
	stderr := &syncBuffer{}
	done := make(chan int, 1)
	go func() { done <- launcher.Run(os.Args[0], []string{"--data", k.cfg.Data}, stderr, signals) }()
	program := filepath.Join(k.programs(), "kiln-"+b)
	waitUntil(t, "the previous program reports after the failed update", func() bool {
		s := stderr.String()
		i := strings.Index(s, "started the previous program "+os.Args[0]+" after the failed update")
		return i >= 0 && strings.Contains(s[i:], "reported healthy")
	})
	// The previous program goes on, and does not update to the failed commit.
	time.Sleep(20 * testInterval)
	signals <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("the launcher ended with %d\n%s", code, stderr)
		}
	case <-time.After(waitLimit):
		t.Fatalf("the launcher did not end\n%s", stderr)
	}
	os.Stdout = was
	w.Close()
	<-copied
	r.Close()
	for _, want := range []string{
		"kiln core: the update failed: the new core " + program + " exited with exit status 0 before it reported healthy\n",
		"kiln core: the store is put back from " + filepath.Join(k.programs(), "kiln-"+k.a+".db") + "\n",
		"kiln core: " + b + " failed as an update; this core waits for main to move past it\n",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("the cores did not say %q:\n%s", want, stdout)
		}
	}
	s := stderr.String()
	if n := strings.Count(s, "asks to be replaced by "+program); n != 1 || strings.Count(s, "asks to be replaced") != 1 {
		t.Errorf("the core asked %d times to be replaced:\n%s", n, s)
	}
	if !strings.Contains(s, "is replaced by "+program) || !strings.Contains(s, "the previous program "+os.Args[0]+" is started") {
		t.Errorf("the launcher said:\n%s", s)
	}
	if got := cardsIn(t, filepath.Join(k.cfg.Data, "store.db")); !slices.Equal(got, cards) {
		t.Errorf("the store holds %q", got)
	}
}
