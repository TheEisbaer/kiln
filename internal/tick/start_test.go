package tick

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/store"
)

// The tests of the core's start: the comparison before the report, the
// report on the socket to the launcher, and nothing done outside the store
// before it (ADR 0036, rules 2 and 3).

// socketPair is a socket pair: the test's end, non-blocking so that a read
// can have a deadline, and the core's end.
func socketPair(t *testing.T) (ours, theirs *os.File) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}
	ours, theirs = os.NewFile(uintptr(fds[0]), "the test's end"), os.NewFile(uintptr(fds[1]), "the core's end")
	t.Cleanup(func() { ours.Close(); theirs.Close() })
	return ours, theirs
}

// readLine reads one line from the test's end within d, or says why not.
func readLine(f *os.File, d time.Duration) (string, error) {
	f.SetReadDeadline(time.Now().Add(d))
	var line []byte
	b := make([]byte, 1)
	for {
		if _, err := f.Read(b); err != nil {
			return string(line), err
		}
		if b[0] == '\n' {
			return string(line), nil
		}
		line = append(line, b[0])
	}
}

// socketLauncher stands in for the launcher in a core in the test's process:
// its report goes to the core's end of a socket pair, as a launcher's
// client writes it.
type socketLauncher struct {
	f    *os.File
	gone chan struct{}
}

func (s *socketLauncher) Report() error {
	_, err := s.f.Write([]byte("healthy\n"))
	return err
}

func (s *socketLauncher) Gone() <-chan struct{} { return s.gone }

// syncBuffer is a core's output, read while it runs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestChildrenSeeNoLauncher starts the test's binary as a core with a socket
// as its file descriptor 3 and KILN_LAUNCHER=3, as kiln launch does: the core
// reports healthy on it, its credential command, a child of sh -c, sees
// neither the variable nor the descriptor, and the core ends, without an
// error, once the launcher's end is closed.
func TestChildrenSeeNoLauncher(t *testing.T) {
	b := newBench(t, 1)
	probe := filepath.Join(t.TempDir(), "probe")
	cfg := b.cfg
	cfg.CredentialCommand = `if [ -n "${KILN_LAUNCHER+x}" ]; then e=set; else e=unset; fi; ` +
		`if (: <&3) 2>/dev/null; then f=open; else f=closed; fi; echo "env=$e fd3=$f" > ` + shellWord(probe) + `; echo stand-in-credential`
	spec, err := json.Marshal(child{Command: "core", Config: cfg, NoEngine: true})
	if err != nil {
		t.Fatal(err)
	}
	ours, theirs := socketPair(t)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), asKilnVar+"="+string(spec), "KILN_LAUNCHER=3")
	var stdout, stderr syncBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.ExtraFiles = []*os.File{theirs}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	theirs.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-done
	})
	if line, err := readLine(ours, waitLimit); line != "healthy" || err != nil {
		t.Fatalf("the core wrote %q, %v, not healthy\n%s%s", line, err, &stdout, &stderr)
	}
	waitUntil(t, "the credential command has run", func() bool {
		data, err := os.ReadFile(probe)
		return err == nil && bytes.HasSuffix(data, []byte("\n"))
	})
	if got := strings.TrimSpace(string(readFile(t, probe))); got != "env=unset fd3=closed" {
		t.Errorf("a child of the core saw %q", got)
	}

	ours.Close()
	start := time.Now()
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Errorf("the core ended with %v once the launcher was gone\n%s%s", err, &stdout, &stderr)
		}
	case <-time.After(waitLimit):
		t.Fatalf("the core did not end once the launcher was gone\n%s%s", &stdout, &stderr)
	}
	// A binary built with -race sleeps a second at its exit.
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the core took %s to end once the launcher was gone", took)
	}
	if stderr.String() != "" {
		t.Errorf("the core printed errors:\n%s", &stderr)
	}
	if !strings.Contains(stdout.String(), "the launcher is gone") {
		t.Errorf("the core did not say the launcher is gone:\n%s", &stdout)
	}
}

// TestLauncherVariable refuses a KILN_LAUNCHER that kiln launch does not set.
func TestLauncherVariable(t *testing.T) {
	b := newBench(t, 1)
	t.Setenv("KILN_LAUNCHER", "4")
	err := startCore(t.Context(), b.cfg, &syncBuffer{}, &syncBuffer{}, newFakeEngine(), time.Millisecond, realClock{})
	if err == nil || err.Error() != `KILN_LAUNCHER is "4"; only kiln launch sets it, to 3` {
		t.Errorf("a core with KILN_LAUNCHER=4: %v", err)
	}
}

// TestNothingBeforeReport holds a core at its report: it has read the store
// and the container engine, and has started no run, kept no proxy, pushed no
// credential, run neither the credential nor the publish command, written no
// instruction, reported nothing and accepted no move; its screen answers
// GET. Once it goes on, it reports, keeps the proxy, starts the run,
// publishes the card and accepts moves.
func TestNothingBeforeReport(t *testing.T) {
	p := newPublishing(t)
	cred := filepath.Join(t.TempDir(), "cred")
	p.cfg.CredentialCommand = "touch " + shellWord(cred) + "; echo stand-in-credential"
	p.cfg.Listen = freeAddress(t)
	var spec, inFlight, published int64
	p.with(t, func(s *store.Store) {
		spec = p.bench.card(t, s, store.Spec)
		inFlight = p.bench.card(t, s, store.Spec)
		if err := s.AddRun(store.RunRow{ID: fmt.Sprintf("%s-%d-1", p.instance, inFlight), Card: inFlight, Commit: p.commit,
			Model: "m", Effort: "high"}, MaxRuns); err != nil {
			t.Fatal(err)
		}
		published = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		p.publish(t, s, published, false)
	})

	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	atPoint = func(point string) {
		if point == "before-report" {
			close(reached)
			<-release
		}
	}
	t.Cleanup(func() { atPoint = nil })
	ours, theirs := socketPair(t)
	e := newFakeEngine()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	out := &syncBuffer{}
	go func() {
		done <- runCore(ctx, p.cfg, out, out, e, 5*time.Millisecond, realClock{}, &socketLauncher{f: theirs, gone: make(chan struct{})})
	}()
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		cancel()
		<-done
	})
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("the core returned %v before its report\n%s", err, out)
	case <-time.After(waitLimit):
		t.Fatalf("the core did not reach its report\n%s", out)
	}

	e.mu.Lock()
	lists, inspects, carried, keeps, pushes, removed := e.lists, e.inspects, len(e.carried), len(e.keeps), len(e.pushes), len(e.removed)
	e.mu.Unlock()
	if lists == 0 || inspects == 0 {
		t.Errorf("before its report the core listed the runs %d times and inspected %d", lists, inspects)
	}
	if carried != 0 || keeps != 0 || pushes != 0 || removed != 0 {
		t.Errorf("before its report the core carried %d runs, kept the proxy %d times, pushed %d credentials and removed %d runs",
			carried, keeps, pushes, removed)
	}
	if n := p.calls(t); n != 0 {
		t.Errorf("before its report the publish command ran %d times", n)
	}
	if _, err := os.Stat(cred); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("before its report the credential command ran: %v", err)
	}
	if files, _ := os.ReadDir(filepath.Join(p.cfg.Data, "instructions")); len(files) != 0 {
		t.Errorf("before its report the core wrote instructions: %v", files)
	}
	if line, err := readLine(ours, 50*time.Millisecond); line != "" || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("before its report the core wrote %q, %v", line, err)
	}
	screen := "http://" + p.cfg.Listen
	post := func(path, body string) (int, string) {
		t.Helper()
		resp, err := http.Post(screen+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var answer struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&answer)
		return resp.StatusCode, answer.Error
	}
	const sentence = "Kiln is starting, and accepts moves once it has compared the store, the container engine and the Git host"
	if code, said := post("/api/cards", `{"text":"a new card"}`); code != 503 || said != sentence {
		t.Errorf("POST /api/cards before the report: %d %q", code, said)
	}
	if code, said := post(fmt.Sprintf("/api/cards/%d/decide", spec), `{"action":"drop"}`); code != 503 || said != sentence {
		t.Errorf("a move before the report: %d %q", code, said)
	}
	var board map[string]any
	getJSON(t, screen+"/api/board", &board)

	once.Do(func() { close(release) })
	if line, err := readLine(ours, waitLimit); line != "healthy" || err != nil {
		t.Fatalf("the core wrote %q, %v after its report\n%s", line, err, out)
	}
	waitUntil(t, "the proxy is kept", func() bool { e.mu.Lock(); defer e.mu.Unlock(); return len(e.keeps) > 0 })
	waitUntil(t, "the spec run starts", func() bool { return slices.Contains(e.snapshot().started, spec) })
	waitUntil(t, "the card is published", func() bool { return p.calls(t) > 0 })
	if code, said := post("/api/cards", `{"text":"a new card"}`); code != 201 {
		t.Errorf("POST /api/cards after the report: %d %q", code, said)
	}
	if !strings.Contains(out.String(), "kiln core: compared the store, the container engine and the Git host; healthy\n") {
		t.Errorf("the core did not say it is healthy:\n%s", out)
	}
}

// TestComparisonWaits compares again while the container engine does not
// answer, saying so once, and reports without a Git host that answers.
func TestComparisonWaits(t *testing.T) {
	t.Run("the engine", func(t *testing.T) {
		b := newBench(t, 1)
		e := newFakeEngine()
		e.listFails = 2
		r := b.start(t, e, 1)
		waitUntil(t, "the proxy is kept", func() bool { e.mu.Lock(); defer e.mu.Unlock(); return len(e.keeps) > 0 })
		e.mu.Lock()
		lists := e.lists
		e.mu.Unlock()
		r.stop(t)
		out := r.out.String()
		if n := strings.Count(out, "kiln core: the comparison before the report: the container engine: the engine does not answer"); n != 1 {
			t.Errorf("the engine's error was printed %d times:\n%s", n, out)
		}
		if lists < 3 || !strings.Contains(out, "; healthy\n") {
			t.Errorf("the core kept the proxy after %d lists of the runs:\n%s", lists, out)
		}
	})
	t.Run("the Git host", func(t *testing.T) {
		b := newBench(t, 1)
		gitIn(t, b.cfg.Repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
		e := newFakeEngine()
		r := b.start(t, e, 1)
		waitUntil(t, "the proxy is kept", func() bool { e.mu.Lock(); defer e.mu.Unlock(); return len(e.keeps) > 0 })
		r.stop(t)
		out := r.out.String()
		if !strings.Contains(out, "kiln core: the comparison before the report: main is not read at the Git host, which does not hold the report: git ls-remote") ||
			!strings.Contains(out, "; healthy\n") {
			t.Errorf("the core did not report past the Git host:\n%s", out)
		}
	})
}

// TestVersion gives the board the commit the core was built from, and
// nothing for a value that is not one.
func TestVersion(t *testing.T) {
	commit := strings.Repeat("0123456789abcdef", 3)[:40]
	for given, want := range map[string]string{
		commit:                   commit,
		"":                       "",
		"v1.2.3":                 "",
		strings.ToUpper(commit):  "",
		commit[:39]:              "",
		commit + "0":             "",
		"built by hand, it says": "",
	} {
		b := newBench(t, 1)
		b.cfg.Listen = freeAddress(t)
		b.cfg.Version = given
		e := newFakeEngine()
		r := b.start(t, e, 1)
		var board struct {
			Version *string `json:"version"`
		}
		getJSON(t, "http://"+b.cfg.Listen+"/api/board", &board)
		if board.Version == nil || *board.Version != want {
			t.Errorf("the version %q: the board says %v, want %q", given, board.Version, want)
		}
		r.stop(t)
	}
}
