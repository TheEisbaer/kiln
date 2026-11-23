package launcher

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The launcher's tests run their own binary as fake cores: a start of
// `<program> core <args>` with fakeDirVar in its environment does what the
// plan in that directory says for its program's name and its start number,
// and appends a line to the start log.

// fakeDirVar names the directory of a test's plan, start log and markers.
const fakeDirVar = "KILN_LAUNCHER_TEST_DIR"

// waitLimit bounds every wait of the test.
const waitLimit = 30 * time.Second

// termStatus is what a fake exits with on SIGTERM, unless it ignores it.
const termStatus = 7

func TestMain(m *testing.M) {
	if dir := os.Getenv(fakeDirVar); dir != "" && len(os.Args) > 1 && os.Args[1] == "core" {
		os.Exit(fake(dir))
	}
	os.Exit(m.Run())
}

// start is one line of the start log.
type start struct {
	Program       string    `json:"program"`
	N             int       `json:"n"`
	Args          []string  `json:"args"`
	Launcher      *string   `json:"launcher"`
	Failed        *string   `json:"failed"`
	FailedProgram *string   `json:"failed_program"`
	At            time.Time `json:"at"`
}

func lookup(name string) *string {
	if v, ok := os.LookupEnv(name); ok {
		return &v
	}
	return nil
}

// fake is a fake core: it logs its start and carries out its plan, one
// action after another, and exits 0 at the plan's end.
func fake(dir string) int {
	name := filepath.Base(os.Args[0])
	n := 1
	for _, s := range readStarts(dir) {
		if s.Program == name {
			n++
		}
	}
	s := start{Program: name, N: n, Args: os.Args[1:], Launcher: lookup(launcherVar), Failed: lookup(failedVar),
		FailedProgram: lookup(failedProgramVar), At: time.Now()}
	line, _ := json.Marshal(s)
	f, err := os.OpenFile(filepath.Join(dir, "starts.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 99
	}
	f.Write(append(line, '\n'))
	f.Close()

	var plan map[string][]string
	data, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err == nil {
		err = json.Unmarshal(data, &plan)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 99
	}
	actions, ok := plan[fmt.Sprintf("%s/%d", name, n)]
	if !ok {
		actions = plan[name+"/*"]
	}
	mark := func(what string) {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%d-%s", name, n, what)), nil, 0o600)
	}
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	ignore := false
	go func() {
		for range terms {
			mark("term")
			if !ignore {
				os.Exit(termStatus)
			}
		}
	}()
	c, err := FromEnv()
	if err != nil || c == nil {
		fmt.Fprintln(os.Stderr, "fake core: no launcher:", err)
		return 98
	}
	for _, a := range actions {
		verb, arg, _ := strings.Cut(a, " ")
		switch verb {
		case "healthy":
			err = c.Report()
		case "replace":
			err = c.AskReplace(arg)
		case "write":
			_, err = c.f.Write([]byte(arg + "\n"))
		case "exit":
			code, _ := strconv.Atoi(arg)
			return code
		case "kill":
			syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		case "sleep":
			d, _ := time.ParseDuration(arg)
			time.Sleep(d)
		case "ignore-term":
			ignore = true
		case "hang":
			select {}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake core:", err)
			return 97
		}
	}
	return 0
}

// readStarts reads a start log.
func readStarts(dir string) []start {
	var starts []start
	data, _ := os.ReadFile(filepath.Join(dir, "starts.jsonl"))
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var s start
		if json.Unmarshal(sc.Bytes(), &s) == nil {
			starts = append(starts, s)
		}
	}
	return starts
}

// syncBuffer is the launcher's output, read while it runs.
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

// bench is a test's directory with the programs A and B, hard links of the
// test's binary, so regular files of the test's user.
type bench struct {
	dir, a, b string
	out       *syncBuffer
	signals   chan os.Signal
}

func newBench(t *testing.T) *bench {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b := &bench{dir: t.TempDir(), out: &syncBuffer{}, signals: make(chan os.Signal, 1)}
	b.a, b.b = filepath.Join(b.dir, "A"), filepath.Join(b.dir, "B")
	for _, p := range []string{b.a, b.b} {
		if err := os.Link(self, p); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(fakeDirVar, b.dir)
	short(t, &backoffBase, 10*time.Millisecond)
	short(t, &healthLimit, waitLimit)
	return b
}

// short sets one of the launcher's times for the test.
func short(t *testing.T, v *time.Duration, d time.Duration) {
	was := *v
	*v = d
	t.Cleanup(func() { *v = was })
}

// plan writes what the fakes do, by <program>/<start number> or
// <program>/*.
func (b *bench) plan(t *testing.T, plan map[string][]string) {
	t.Helper()
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.dir, "plan.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// launch runs the launcher on A in a goroutine, and returns its status.
func (b *bench) launch(t *testing.T, args ...string) <-chan int {
	t.Helper()
	if args == nil {
		args = []string{"--data", "d"}
	}
	done := make(chan int, 1)
	go func() { done <- Run(b.a, args, b.out, b.signals) }()
	return done
}

// run runs the launcher on A to its end, and returns its status.
func (b *bench) run(t *testing.T, args ...string) int {
	t.Helper()
	return b.await(t, b.launch(t, args...))
}

func (b *bench) await(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(waitLimit):
		t.Fatalf("the launcher did not end; it said:\n%s", b.out)
		return 0
	}
}

// starts is the start log, as <program>/<n>.
func (b *bench) starts() []start { return readStarts(b.dir) }

func (b *bench) names() []string {
	var names []string
	for _, s := range b.starts() {
		names = append(names, fmt.Sprintf("%s/%d", s.Program, s.N))
	}
	return names
}

func (b *bench) wantStarts(t *testing.T, want ...string) {
	t.Helper()
	if got := b.names(); !slices.Equal(got, want) {
		t.Errorf("the starts were %v, want %v; the launcher said:\n%s", got, want, b.out)
	}
}

func (b *bench) waitSaid(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !strings.Contains(b.out.String(), text) {
		if time.Now().After(deadline) {
			t.Fatalf("the launcher did not say %q; it said:\n%s", text, b.out)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (b *bench) marked(name string) bool {
	_, err := os.Stat(filepath.Join(b.dir, name))
	return err == nil
}

func str(p *string) string {
	if p == nil {
		return "<unset>"
	}
	return *p
}

// TestStartsTheCore checks that the launcher starts `<program> core` with its
// own arguments, the socket's variable and none of the update's, even when
// its own environment has them.
func TestStartsTheCore(t *testing.T) {
	b := newBench(t)
	t.Setenv(launcherVar, "7")
	t.Setenv(failedVar, "from the test")
	t.Setenv(failedProgramVar, "/from/the/test")
	b.plan(t, map[string][]string{"A/1": {"healthy", "exit 0"}})
	if code := b.run(t, "--data", "d", "--x"); code != 0 {
		t.Errorf("the launcher ended with %d", code)
	}
	starts := b.starts()
	if len(starts) != 1 {
		t.Fatalf("starts: %v", b.names())
	}
	s := starts[0]
	if !slices.Equal(s.Args, []string{"core", "--data", "d", "--x"}) {
		t.Errorf("the core's arguments were %q", s.Args)
	}
	if str(s.Launcher) != "3" || s.Failed != nil || s.FailedProgram != nil {
		t.Errorf("the core's variables: KILN_LAUNCHER %s, KILN_UPDATE_FAILED %s, KILN_UPDATE_FAILED_PROGRAM %s",
			str(s.Launcher), str(s.Failed), str(s.FailedProgram))
	}
	for _, said := range []string{"kiln launch: started " + b.a + " (pid ", "reported healthy", "exited with exit status 0", "ended on purpose"} {
		if !strings.Contains(b.out.String(), said) {
			t.Errorf("the launcher did not say %q:\n%s", said, b.out)
		}
	}

	var usage bytes.Buffer
	if code := Main(nil, &usage); code != 2 || !strings.Contains(usage.String(), "kiln launch <the flags of kiln core>") {
		t.Errorf("Main(nil): %d, %q", code, usage.String())
	}
}

// TestReplacedCoreStays replaces A by B, which reports and then crashes: B is
// the running core, started again after the back-off and told of no failed
// update.
func TestReplacedCoreStays(t *testing.T) {
	b := newBench(t)
	short(t, &backoffBase, 50*time.Millisecond)
	b.plan(t, map[string][]string{
		"A/1": {"healthy", "replace " + b.b, "exit 0"},
		"B/1": {"healthy", "exit 1"},
		"B/2": {"healthy", "exit 0"},
	})
	if code := b.run(t); code != 0 {
		t.Errorf("the launcher ended with %d", code)
	}
	b.wantStarts(t, "A/1", "B/1", "B/2")
	starts := b.starts()
	if len(starts) == 3 {
		if starts[2].Failed != nil || starts[2].FailedProgram != nil {
			t.Errorf("B was started again with KILN_UPDATE_FAILED %s", str(starts[2].Failed))
		}
		if gap := starts[2].At.Sub(starts[1].At); gap < backoffBase {
			t.Errorf("B was started again after %s, before the back-off of %s", gap, backoffBase)
		}
	}
	if !strings.Contains(b.out.String(), "started "+b.b+" on probation") {
		t.Errorf("the launcher did not say B is on probation:\n%s", b.out)
	}
}

// TestFailedUpdate replaces A by a B that exits, hangs or cannot be started
// before its report: A is started again, told why and which program failed.
func TestFailedUpdate(t *testing.T) {
	cases := []struct {
		name   string
		b      func(b *bench) string // the program to replace A by
		plan   []string              // B's
		reason func(b *bench) string
		before func(t *testing.T)
		after  func(t *testing.T, b *bench)
	}{
		{
			name: "exits",
			plan: []string{"exit 1"},
			reason: func(b *bench) string {
				return "the new core " + b.b + " exited with exit status 1 before it reported healthy"
			},
		},
		{
			name: "killed",
			plan: []string{"kill"},
			reason: func(b *bench) string {
				return "the new core " + b.b + " was killed by SIGKILL before it reported healthy"
			},
		},
		{
			name:   "hangs",
			plan:   []string{"hang"},
			reason: func(b *bench) string { return "the new core " + b.b + " did not report healthy within 300ms" },
			before: func(t *testing.T) { short(t, &healthLimit, 300*time.Millisecond) },
			after: func(t *testing.T, b *bench) {
				if !b.marked("B-1-term") {
					t.Error("B did not get SIGTERM")
				}
			},
		},
		{
			name:   "hangs and ignores SIGTERM",
			plan:   []string{"ignore-term", "hang"},
			reason: func(b *bench) string { return "the new core " + b.b + " did not report healthy within 300ms" },
			before: func(t *testing.T) {
				short(t, &healthLimit, 300*time.Millisecond)
				short(t, &killGrace, 200*time.Millisecond)
			},
			after: func(t *testing.T, b *bench) {
				if !b.marked("B-1-term") || !strings.Contains(b.out.String(), b.b+" (pid ") ||
					!strings.Contains(b.out.String(), "was killed by SIGKILL") {
					t.Errorf("B was not given SIGTERM and then killed:\n%s", b.out)
				}
			},
		},
		{
			name:   "reports too late and exits 0",
			plan:   []string{"ignore-term", "sleep 500ms", "healthy", "exit 0"},
			reason: func(b *bench) string { return "the new core " + b.b + " did not report healthy within 300ms" },
			before: func(t *testing.T) {
				short(t, &healthLimit, 300*time.Millisecond)
				short(t, &killGrace, 2*time.Second)
			},
			after: func(t *testing.T, b *bench) {
				if !strings.Contains(b.out.String(), "after its time ran out; ignored") {
					t.Errorf("the late report was not ignored:\n%s", b.out)
				}
			},
		},
		{
			name:   "reports too late and exits 1",
			plan:   []string{"ignore-term", "sleep 500ms", "healthy", "exit 1"},
			reason: func(b *bench) string { return "the new core " + b.b + " did not report healthy within 300ms" },
			before: func(t *testing.T) {
				short(t, &healthLimit, 300*time.Millisecond)
				short(t, &killGrace, 2*time.Second)
			},
			after: func(t *testing.T, b *bench) {
				if !strings.Contains(b.out.String(), "after its time ran out; ignored") {
					t.Errorf("the late report was not ignored:\n%s", b.out)
				}
			},
		},
		{
			name: "cannot be started",
			b: func(b *bench) string {
				p := filepath.Join(b.dir, "C")
				os.WriteFile(p, []byte("not a program\n"), 0o644)
				return p
			},
			reason: func(b *bench) string { return "the new core " + filepath.Join(b.dir, "C") + " could not be started: " },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			if tc.before != nil {
				tc.before(t)
			}
			replacement := b.b
			if tc.b != nil {
				replacement = tc.b(b)
			}
			b.plan(t, map[string][]string{
				"A/1": {"healthy", "replace " + replacement, "exit 0"},
				"B/1": tc.plan,
				"A/2": {"healthy", "exit 0"},
			})
			if code := b.run(t); code != 0 {
				t.Errorf("the launcher ended with %d", code)
			}
			want := []string{"A/1", "B/1", "A/2"}
			if tc.b != nil {
				want = []string{"A/1", "A/2"}
			}
			b.wantStarts(t, want...)
			starts := b.starts()
			last := starts[len(starts)-1]
			if !strings.HasPrefix(str(last.Failed), tc.reason(b)) || str(last.FailedProgram) != replacement {
				t.Errorf("A was told KILN_UPDATE_FAILED %q and KILN_UPDATE_FAILED_PROGRAM %q, want %q and %q",
					str(last.Failed), str(last.FailedProgram), tc.reason(b), replacement)
			}
			if tc.after != nil {
				tc.after(t, b)
			}
		})
	}
}

// TestFallingBack follows the previous program after a failed update: a
// death by a signal before its report starts it again, with the update's
// variables, three times; a non-zero exit ends the launcher; once it has
// reported, a crash starts it again without them.
func TestFallingBack(t *testing.T) {
	failed := func(b *bench) map[string][]string {
		return map[string][]string{"A/1": {"healthy", "replace " + b.b, "exit 0"}, "B/1": {"exit 1"}}
	}
	t.Run("killed four times", func(t *testing.T) {
		b := newBench(t)
		plan := failed(b)
		plan["A/*"] = []string{"kill"}
		b.plan(t, plan)
		if code := b.run(t); code != 137 {
			t.Errorf("the launcher ended with %d, want 137", code)
		}
		b.wantStarts(t, "A/1", "B/1", "A/2", "A/3", "A/4", "A/5")
		for _, s := range b.starts()[2:] {
			if s.Failed == nil || str(s.FailedProgram) != b.b {
				t.Errorf("A/%d was started with KILN_UPDATE_FAILED %s and KILN_UPDATE_FAILED_PROGRAM %s", s.N, str(s.Failed), str(s.FailedProgram))
			}
		}
		if !strings.Contains(b.out.String(), "the core "+b.a+" ended with SIGKILL before it reported healthy; Kiln has stopped") {
			t.Errorf("the launcher did not say it stopped:\n%s", b.out)
		}
	})
	t.Run("exits 1", func(t *testing.T) {
		b := newBench(t)
		plan := failed(b)
		plan["A/2"] = []string{"exit 1"}
		b.plan(t, plan)
		if code := b.run(t); code != 1 {
			t.Errorf("the launcher ended with %d, want 1", code)
		}
		b.wantStarts(t, "A/1", "B/1", "A/2")
	})
	t.Run("reports, then crashes", func(t *testing.T) {
		b := newBench(t)
		plan := failed(b)
		plan["A/2"] = []string{"healthy", "exit 1"}
		plan["A/3"] = []string{"healthy", "exit 0"}
		b.plan(t, plan)
		if code := b.run(t); code != 0 {
			t.Errorf("the launcher ended with %d", code)
		}
		b.wantStarts(t, "A/1", "B/1", "A/2", "A/3")
		if starts := b.starts(); len(starts) == 4 && (starts[3].Failed != nil || starts[3].FailedProgram != nil) {
			t.Errorf("A/3 was started with KILN_UPDATE_FAILED %s", str(starts[3].Failed))
		}
	})
}

// TestEndsBeforeReport ends the launcher with a first start that ends before
// its report, without starting it again.
func TestEndsBeforeReport(t *testing.T) {
	for _, tc := range []struct {
		plan []string
		code int
		said string
	}{
		{[]string{"exit 2"}, 2, "ended with exit status 2 before it reported healthy; Kiln has stopped"},
		{[]string{"kill"}, 137, "ended with SIGKILL before it reported healthy; Kiln has stopped"},
	} {
		b := newBench(t)
		b.plan(t, map[string][]string{"A/1": tc.plan})
		if code := b.run(t); code != tc.code {
			t.Errorf("%v: the launcher ended with %d, want %d", tc.plan, code, tc.code)
		}
		b.wantStarts(t, "A/1")
		if !strings.Contains(b.out.String(), tc.said) {
			t.Errorf("%v: the launcher did not say %q:\n%s", tc.plan, tc.said, b.out)
		}
	}
}

// TestBackoff starts a crashing core again after a wait that doubles to its
// cap, and back at the base once a core has run long enough.
func TestBackoff(t *testing.T) {
	t.Run("doubles to the cap", func(t *testing.T) {
		b := newBench(t)
		short(t, &backoffBase, 20*time.Millisecond)
		short(t, &backoffCap, 80*time.Millisecond)
		b.plan(t, map[string][]string{"A/*": {"healthy", "exit 1"}, "A/7": {"healthy", "exit 0"}})
		if code := b.run(t); code != 0 {
			t.Errorf("the launcher ended with %d", code)
		}
		starts := b.starts()
		if len(starts) != 7 {
			t.Fatalf("starts: %v", b.names())
		}
		for i, want := range []time.Duration{20, 40, 80, 80, 80, 80} {
			want *= time.Millisecond
			if gap := starts[i+1].At.Sub(starts[i].At); gap < want {
				t.Errorf("start %d came %s after the one before, want at least %s", i+2, gap, want)
			}
		}
	})
	t.Run("reset", func(t *testing.T) {
		b := newBench(t)
		short(t, &backoffBase, 50*time.Millisecond)
		short(t, &backoffCap, time.Second)
		short(t, &backoffReset, 200*time.Millisecond)
		b.plan(t, map[string][]string{
			"A/*": {"healthy", "exit 1"},
			"A/4": {"healthy", "sleep 250ms", "exit 1"},
			"A/5": {"healthy", "exit 0"},
		})
		if code := b.run(t); code != 0 {
			t.Errorf("the launcher ended with %d", code)
		}
		starts := b.starts()
		if len(starts) != 5 {
			t.Fatalf("starts: %v", b.names())
		}
		// Without the reset the wait would be 400 ms, after the 250 ms of A/4.
		if gap := starts[4].At.Sub(starts[3].At); gap > 550*time.Millisecond {
			t.Errorf("A/5 came %s after A/4, which ran past the reset", gap)
		}
	})
}

// TestReplaceChecked refuses a request to be replaced that is not for an
// absolute path to a regular file of the launcher's user, or that comes
// before the core's report.
func TestReplaceChecked(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(t *testing.T, b *bench) string
	}{
		{"relative", func(t *testing.T, b *bench) string { return "B" }},
		{"directory", func(t *testing.T, b *bench) string { return b.dir }},
		{"symlink", func(t *testing.T, b *bench) string {
			p := filepath.Join(b.dir, "link")
			if err := os.Symlink(b.b, p); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"another user's", func(t *testing.T, b *bench) string {
			was := uid
			uid = func() int { return os.Getuid() + 1 }
			t.Cleanup(func() { uid = was })
			return b.b
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			b.plan(t, map[string][]string{
				"A/1": {"healthy", "replace " + tc.path(t, b), "exit 0"},
				"A/2": {"healthy", "exit 0"},
			})
			if code := b.run(t); code != 0 {
				t.Errorf("the launcher ended with %d", code)
			}
			b.wantStarts(t, "A/1", "A/2")
			if !strings.Contains(b.out.String(), "refused to replace the core by") {
				t.Errorf("the launcher did not say it refused:\n%s", b.out)
			}
		})
	}
	t.Run("before the report", func(t *testing.T) {
		b := newBench(t)
		b.plan(t, map[string][]string{"A/1": {"replace " + b.b, "exit 0"}})
		if code := b.run(t); code != 0 {
			t.Errorf("the launcher ended with %d", code)
		}
		b.wantStarts(t, "A/1")
		if !strings.Contains(b.out.String(), "refused to replace the core by") || !strings.Contains(b.out.String(), "before it reported healthy; Kiln has stopped") {
			t.Errorf("the launcher did not refuse and stop:\n%s", b.out)
		}
	})
	t.Run("a line it does not know", func(t *testing.T) {
		b := newBench(t)
		b.plan(t, map[string][]string{"A/1": {"write hello\x1b[2J", "healthy", "exit 0"}})
		if code := b.run(t); code != 0 {
			t.Errorf("the launcher ended with %d", code)
		}
		if out := b.out.String(); !strings.Contains(out, `ignored: "hello\x1b[2J"`) || strings.Contains(out, "\x1b") {
			t.Errorf("the launcher did not print the line made printable:\n%q", out)
		}
	})
}

// TestSignals ends the running core and the launcher on a signal, and starts
// nothing after it.
func TestSignals(t *testing.T) {
	t.Run("ends the core", func(t *testing.T) {
		b := newBench(t)
		b.plan(t, map[string][]string{"A/1": {"healthy", "hang"}})
		done := b.launch(t)
		b.waitSaid(t, "reported healthy")
		b.signals <- syscall.SIGTERM
		if code := b.await(t, done); code != termStatus {
			t.Errorf("the launcher ended with %d, want the core's %d", code, termStatus)
		}
		if !b.marked("A-1-term") {
			t.Error("A did not get SIGTERM")
		}
		b.wantStarts(t, "A/1")
	})
	t.Run("kills a core that ignores it", func(t *testing.T) {
		b := newBench(t)
		short(t, &stopGrace, 300*time.Millisecond)
		b.plan(t, map[string][]string{"A/1": {"ignore-term", "healthy", "hang"}})
		done := b.launch(t)
		b.waitSaid(t, "reported healthy")
		b.signals <- syscall.SIGINT
		if code := b.await(t, done); code != 137 {
			t.Errorf("the launcher ended with %d, want 137", code)
		}
		if !b.marked("A-1-term") {
			t.Error("A did not get SIGTERM")
		}
	})
	t.Run("between replace and exit", func(t *testing.T) {
		b := newBench(t)
		b.plan(t, map[string][]string{"A/1": {"healthy", "replace " + b.b, "hang"}})
		done := b.launch(t)
		b.waitSaid(t, "asks to be replaced")
		b.signals <- syscall.SIGTERM
		if code := b.await(t, done); code != termStatus {
			t.Errorf("the launcher ended with %d", code)
		}
		b.wantStarts(t, "A/1")
	})
	t.Run("during the back-off", func(t *testing.T) {
		b := newBench(t)
		short(t, &backoffBase, time.Minute)
		b.plan(t, map[string][]string{"A/1": {"healthy", "exit 1"}})
		done := b.launch(t)
		b.waitSaid(t, "started again in 1m0s")
		b.signals <- syscall.SIGHUP
		if code := b.await(t, done); code != 0 {
			t.Errorf("the launcher ended with %d", code)
		}
		b.wantStarts(t, "A/1")
	})
}

// TestTouchesNothing keeps the launcher to its job (ADR 0036, rule 1): none
// of its files but the tests imports the store, the seal, the tick, a run,
// the screen or an HTTP server.
func TestTouchesNothing(t *testing.T) {
	forbidden := []string{
		"github.com/TheEisbaer/kiln/internal/store", "github.com/TheEisbaer/kiln/internal/seal",
		"github.com/TheEisbaer/kiln/internal/tick", "github.com/TheEisbaer/kiln/internal/run",
		"github.com/TheEisbaer/kiln/internal/web", "net/http",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		imports, err := importsOf(f)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range imports {
			if slices.Contains(forbidden, imp) {
				t.Errorf("%s imports %s", f, imp)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no file of the launcher was checked")
	}
}

// importsOf reads the imports of a Go file, without running anything.
func importsOf(file string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var imports []string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, err
		}
		imports = append(imports, path)
	}
	return imports, nil
}
