package tick

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	files := imageTree()
	files["NOTES.md"], files["go.mod"] = "# Notes\n", "module "+module+"\n\ngo 1.27\n"
	repo, commit := bareRepo(t, files)
	gitIn(t, repo, "remote", "remove", "origin")
	b.cfg.Repo, b.commit = repo, commit
	b.cfg.Version = commit
	withImages(&b.cfg)
	return &kilnBench{bench: b, a: commit}
}

// imageTree are the files the images are built from, beside go.mod.
func imageTree() map[string]string {
	files := map[string]string{"go.sum": ""}
	for _, im := range []string{"toolchain", "proxy", "wayout", "run"} {
		files["images/"+im+"/Dockerfile"] = "FROM scratch\n"
	}
	return files
}

// withImages names the images of the update in a bench's configuration, as
// cmd/core does.
func withImages(cfg *Config) {
	cfg.ToolchainImage, cfg.ProxyImage, cfg.WayOutImage, cfg.RunImage = "kiln-toolchain", "kiln-proxy", "kiln-wayout", "kiln-run"
}

// imageNames are the images of the update, in the order it builds them.
var imageNames = []string{"kiln-toolchain", "kiln-proxy", "kiln-wayout", "kiln-run"}

// withoutKeeps are a stand-in engine's events without those of KeepProxy.
func withoutKeeps(events []string) []string {
	return slices.DeleteFunc(slices.Clone(events), func(e string) bool { return strings.HasPrefix(e, "keep ") })
}

// plainNames are the references of a book's images with the tag latest.
func plainNames(b imageBook) []string {
	var plain []string
	for ref := range b.Images {
		if strings.HasSuffix(ref, ":latest") {
			plain = append(plain, ref)
		}
	}
	return plain
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
// flight goes on in its container. The toolchain image is built first, under
// the new head's tag, and the program in it; when the images' files changed,
// the proxy's, the way out's and the run image are built after it, and no
// plain name changes.
func TestUpdate(t *testing.T) {
	for _, c := range []struct {
		name   string
		files  map[string]string
		images string // the file the core says changed, "" for none
	}{
		{"the program", map[string]string{"NOTES.md": "# Notes\nMoved.\n"}, ""},
		{"the run image", map[string]string{"images/run/Dockerfile": "FROM scratch\nUSER 1000\n"}, "images/run/Dockerfile"},
		{"the proxy", map[string]string{"cmd/proxy/x.go": "package main\n"}, "cmd/proxy/x.go"},
		{"the way out", map[string]string{"cmd/wayout/x.go": "package main\n"}, "cmd/wayout/x.go"},
		{"go.sum", map[string]string{"go.sum": "example.com/x v1.0.0 h1:x=\n"}, "go.sum"},
		{"go.mod", map[string]string{"go.mod": "module " + kilnModule + "\n\ngo 1.27\n\nrequire example.com/x v1.0.0\n"}, "go.mod"},
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
			tag := b[:12]
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
			imageBuilds, book := slices.Clone(e.imageBuilds), e.book
			e.mu.Unlock()
			toolchain := "kiln-toolchain:" + tag
			if len(builds) != 1 || builds[0].Version != b || builds[0].Image != toolchain || builds[0].Instance != k.instance ||
				builds[0].GOOS != runtime.GOOS || builds[0].GOARCH != runtime.GOARCH {
				t.Errorf("the builds: %+v", builds)
			}
			built := imageNames[:1]
			if c.images != "" {
				built = imageNames
			}
			var want []string
			for _, name := range built {
				want = append(want, "build "+name+":"+tag)
			}
			want = append(want, "program "+toolchain)
			if got := withoutKeeps(book.Events); !slices.Equal(got, want) {
				t.Errorf("the engine's events are %q, want %q", got, want)
			}
			for _, ib := range imageBuilds {
				var wantFiles []string
				switch ib.Name {
				case "kiln-toolchain":
					wantFiles = []string{"go.mod", "go.sum", "images/toolchain/Dockerfile"}
				case "kiln-run":
					wantFiles = []string{"Dockerfile"}
				}
				files := slices.Sorted(slices.Values(ib.entries))
				if wantFiles != nil && !slices.Equal(files, wantFiles) {
					t.Errorf("the context of %s holds %q, want %q", ib.Name, files, wantFiles)
				}
				if wantFiles == nil && (!slices.Contains(files, "NOTES.md") || !slices.Contains(files, "images/run/Dockerfile")) {
					t.Errorf("the context of %s is not the whole commit: %q", ib.Name, files)
				}
				wantNone, wantToolchain := ib.Name == "kiln-proxy" || ib.Name == "kiln-wayout", toolchain
				if ib.Name == "kiln-toolchain" {
					wantToolchain = ""
				}
				if ib.Tag != tag || ib.NoNetwork != wantNone || ib.Toolchain != wantToolchain {
					t.Errorf("the build of %s: %+v", ib.Name, ib.Image)
				}
			}
			if plain := plainNames(book); len(plain) != 0 {
				t.Errorf("the update made the plain names %q", plain)
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
			said := "kiln core: the images' files changed between " + k.a + " and " + b + " (" + c.images + "); building the images as <name>:" + tag + "\n"
			if c.images != "" && !strings.Contains(out, said) {
				t.Errorf("the core did not say %q:\n%s", said, out)
			} else if c.images == "" && strings.Contains(out, "the images' files") {
				t.Errorf("the core said the images' files changed:\n%s", out)
			}
		})
	}
}

// TestUpdateImageFails fails the build of the way out's image: the core says
// so once, builds neither the run image nor the program, does not hand over,
// and builds nothing more for the same head; once main moves, it builds
// again, under the new head's tag.
func TestUpdateImageFails(t *testing.T) {
	k := newKilnBench(t, kilnModule)
	setFetchEvery(t, 20*time.Millisecond)
	k.twoCards(t)
	e := newFakeEngine()
	e.imageFails["kiln-wayout"] = true
	h := startHanding(t, k.cfg, e, true)
	if line := h.line(t); line != "healthy" {
		t.Fatalf("the core wrote %q", line)
	}
	b := k.move(t, "", map[string]string{"cmd/wayout/x.go": "package main\n"})
	said := "kiln core: the core of " + b + " was not built: the image kiln-wayout:" + b[:12] +
		": the image's build failed, as the test has it; this core goes on as " + k.a + "\n"
	waitUntil(t, "the build fails", func() bool { return strings.Contains(h.out.String(), said) })
	waitRounds(t, e, 10)
	h.noLine(t, 50*time.Millisecond)
	want := []string{"build kiln-toolchain:" + b[:12], "build kiln-proxy:" + b[:12], "failed kiln-wayout:" + b[:12]}
	if got := withoutKeeps(e.events()); !slices.Equal(got, want) {
		t.Errorf("the engine's events are %q, want %q", got, want)
	}
	if n := strings.Count(h.out.String(), "was not built"); n != 1 {
		t.Errorf("the core said %d times that the build failed:\n%s", n, h.out)
	}

	e.mu.Lock()
	delete(e.imageFails, "kiln-wayout")
	e.mu.Unlock()
	c := k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
	if line := h.line(t); line != "replace "+filepath.Join(k.programs(), "kiln-"+c) {
		t.Fatalf("the core wrote %q\n%s", line, h.out)
	}
	h.returned(t)
	got := withoutKeeps(e.events())[len(want):]
	want = nil
	for _, name := range imageNames {
		want = append(want, "build "+name+":"+c[:12])
	}
	// The hand-over removes the tags of the build that failed.
	want = append(want, "program kiln-toolchain:"+c[:12], "rm kiln-toolchain:"+b[:12], "rm kiln-proxy:"+b[:12])
	if !slices.Equal(got, want) {
		t.Errorf("after main moved the engine's events are %q, want %q", got, want)
	}
}

// promotable puts each image under its plain name at old-<image> and under
// the version's tag at new-<image>, and returns the book.
func promotable(version string) imageBook {
	b := imageBook{Images: map[string]string{}}
	for _, name := range imageNames {
		x := strings.TrimPrefix(name, "kiln-")
		b.Images[name+":latest"], b.Images[name+":"+version[:12]] = "old-"+x, "new-"+x
	}
	return b
}

// promotions are the events of the promotion of every image of a version,
// in order.
func promotions(version string) []string {
	var events []string
	for _, name := range imageNames {
		events = append(events, "tag "+name+":"+version[:12]+" "+name, "rm "+name+":"+version[:12])
	}
	return events
}

// TestPromote starts a core whose version's images are tagged: it makes them
// the plain names only after its report, and before it keeps the proxy, which
// is then kept with the new image. A second start promotes nothing.
func TestPromote(t *testing.T) {
	k := newKilnBench(t, kilnModule)
	k.twoCards(t)
	e := newFakeEngine()
	e.book = promotable(k.a)
	beforeReport := make(chan []string, 1)
	atPoint = func(point string) {
		if point == "before-report" {
			select {
			case beforeReport <- e.events():
			default:
			}
		}
	}
	t.Cleanup(func() { atPoint = nil })
	h := startHanding(t, k.cfg, e, true)
	if line := h.line(t); line != "healthy" {
		t.Fatalf("the core wrote %q", line)
	}
	if got := <-beforeReport; len(got) != 0 {
		t.Errorf("before the report the engine's events are %q", got)
	}
	waitUntil(t, "the proxy is kept", func() bool { return e.kept() > 0 })
	want := append(promotions(k.a), "keep kiln-proxy new-proxy")
	if got := e.events(); !slices.Equal(got, want) {
		t.Errorf("the engine's events are %q, want %q", got, want)
	}
	h.ended(t)
	e.mu.Lock()
	images := maps.Clone(e.book.Images)
	e.mu.Unlock()
	for _, name := range imageNames {
		if got, want := images[name+":latest"], "new-"+strings.TrimPrefix(name, "kiln-"); got != want {
			t.Errorf("%s is %q, want %q", name, got, want)
		}
	}
	if len(images) != len(imageNames) {
		t.Errorf("the images are %v", images)
	}
	for _, name := range imageNames {
		if !strings.Contains(h.out.String(), "kiln core: the image "+name+":"+k.a[:12]+" is now "+name+"\n") {
			t.Errorf("the core did not say that %s is promoted:\n%s", name, h.out)
		}
	}

	h = startHanding(t, k.cfg, e, true)
	if line := h.line(t); line != "healthy" {
		t.Fatalf("the core wrote %q", line)
	}
	waitUntil(t, "the proxy is kept again", func() bool { return e.kept() > 1 })
	h.ended(t)
	if got := e.events()[len(want):]; !slices.Equal(got, []string{"keep kiln-proxy new-proxy"}) {
		t.Errorf("the second start's events are %q", got)
	}
}

// kept is the calls of KeepProxy so far.
func (e *fakeEngine) kept() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.keeps)
}

// readBook reads a noEngine's file of images.
func readBook(t *testing.T, path string) imageBook {
	t.Helper()
	var b imageBook
	if err := json.Unmarshal(readFile(t, path), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// writeBook writes a noEngine's file of images.
func writeBook(t *testing.T, path string, b imageBook) {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// keptUntil waits for a child's engine to have kept the proxy, and stops the
// child as Ctrl-C does.
func keptUntil(t *testing.T, l *launched, images string) {
	t.Helper()
	waitUntil(t, "the proxy is kept", func() bool {
		return slices.ContainsFunc(readBook(t, images).Events, func(e string) bool { return strings.HasPrefix(e, "keep ") })
	})
	l.cmd.Process.Signal(os.Interrupt)
	l.exited(t, 0)
}

// TestPromoteAfterFailedUpdate has the core of b fail before its report, as
// a failed update does: the plain names stay at the old images, and the
// previous core, started after the failed update, promotes nothing and keeps
// its proxy with the old image.
func TestPromoteAfterFailedUpdate(t *testing.T) {
	k := newKilnBench(t, kilnModule)
	k.twoCards(t)
	b := k.move(t, "", map[string]string{"cmd/proxy/x.go": "package main\n"})
	images := filepath.Join(t.TempDir(), "images.json")
	writeBook(t, images, promotable(b))
	newer := k.cfg
	newer.Version = b
	launch(t, child{Command: "core", Config: newer, NoEngine: true, Images: images, Hook: "tick:before-report:exit"}).exited(t, exitAtPoint)
	if got := readBook(t, images); !maps.Equal(got.Images, promotable(b).Images) || len(got.Events) != 0 {
		t.Errorf("after the failed update the engine holds %v, after %q", got.Images, got.Events)
	}

	l := launch(t, child{Command: "core", Config: k.cfg, NoEngine: true, Images: images}, k.failedEnv(b)...)
	l.until(t, "healthy")
	keptUntil(t, l, images)
	got := readBook(t, images)
	if !maps.Equal(got.Images, promotable(b).Images) || !slices.Equal(got.Events, []string{"keep kiln-proxy old-proxy"}) {
		t.Errorf("the previous core left the engine with %v, after %q\n%s%s", got.Images, got.Events, l.stdout, l.stderr)
	}
}

// TestPromoteKilled kills the core between the tag of the toolchain image's
// plain name and the removal of its tag: the next start removes the tag
// without tagging again, and promotes the other three.
func TestPromoteKilled(t *testing.T) {
	k := newKilnBench(t, kilnModule)
	k.twoCards(t)
	images := filepath.Join(t.TempDir(), "images.json")
	writeBook(t, images, promotable(k.a))
	ch := child{Command: "core", Config: k.cfg, NoEngine: true, Images: images, Hook: "tick:promote-tagged:exit"}
	launch(t, ch).exited(t, exitAtPoint)
	tag := k.a[:12]
	got := readBook(t, images)
	if got.Images["kiln-toolchain:latest"] != "new-toolchain" || got.Images["kiln-toolchain:"+tag] != "new-toolchain" ||
		!slices.Equal(got.Events, promotions(k.a)[:1]) {
		t.Fatalf("after the kill the engine holds %v, after %q", got.Images, got.Events)
	}

	ch.Hook = ""
	l := launch(t, ch)
	l.until(t, "healthy")
	keptUntil(t, l, images)
	got = readBook(t, images)
	want := append(append(promotions(k.a)[:1], promotions(k.a)[1:]...), "keep kiln-proxy new-proxy")
	if !slices.Equal(got.Events, want) {
		t.Errorf("the engine's events are %q, want %q", got.Events, want)
	}
	for _, name := range imageNames {
		if got.Images[name+":latest"] != "new-"+strings.TrimPrefix(name, "kiln-") {
			t.Errorf("%s is %q", name, got.Images[name+":latest"])
		}
	}
	if len(got.Images) != len(imageNames) {
		t.Errorf("the engine holds %v", got.Images)
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
	e.waitFirstTick(t, 0)
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
	withImages(&p.cfg)
	setFetchEvery(t, 20*time.Millisecond)
	gomod := imageTree()
	gomod["go.mod"] = "module " + kilnModule + "\n"
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
		!strings.Contains(h.out.String(), "kiln core: this core was built by hand, so it cannot tell whether the images' files changed; building the images as <name>:"+b[:12]+"\n") {
		t.Errorf("the core said:\n%s", h.out)
	}
	var want []string
	for _, name := range imageNames {
		want = append(want, "build "+name+":"+b[:12])
	}
	if want = append(want, "program kiln-toolchain:"+b[:12]); !slices.Equal(withoutKeeps(e.events()), want) {
		t.Errorf("the engine's events are %q, want %q", withoutKeeps(e.events()), want)
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

// TestUpdateRemovesByName fills the programs directory and the engine before
// an update: the programs and copies more than one step back go, and every
// other name, a directory, the running core's program and copy and the new
// program stay; of the images, only the tags of 12 hex digits of Kiln's
// images go that are neither the running version's nor the new one's.
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
	e.book.Images = map[string]string{}
	for _, ref := range []string{"kiln-proxy:" + c[:12], "kiln-proxy:latest", "kiln-proxy:mine", "other:" + c[:12], "kiln-run:" + c[:12]} {
		e.book.Images[ref] = "id-" + ref
	}
	h := startHanding(t, k.cfg, e, true)
	if line := h.line(t); line != "healthy" {
		t.Fatalf("the core wrote %q", line)
	}
	// The running version's tag, put there after the core's start, which
	// would have promoted it.
	waitUntil(t, "the proxy is kept", func() bool { return e.kept() > 0 })
	e.mu.Lock()
	e.book.Images["kiln-proxy:"+k.a[:12]] = "id-a"
	e.mu.Unlock()
	b := k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
	if line := h.line(t); !strings.HasPrefix(line, "replace ") {
		t.Fatalf("the core wrote %q", line)
	}
	h.returned(t)
	e.mu.Lock()
	refs := slices.Sorted(maps.Keys(e.book.Images))
	e.mu.Unlock()
	wantRefs := []string{"kiln-proxy:" + k.a[:12], "kiln-proxy:latest", "kiln-proxy:mine", "kiln-toolchain:" + b[:12], "other:" + c[:12]}
	if slices.Sort(wantRefs); !slices.Equal(refs, wantRefs) {
		t.Errorf("the engine holds %q, want %q", refs, wantRefs)
	}
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
// every card of the copy, turns automatic merging off with an entry, and does
// not update to the failed commit again. Its children see neither variable.
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
		"kiln core: automatic merging is off until you turn it on from the screen\n",
	} {
		if strings.Count(out, want) != 1 {
			t.Errorf("the core did not say %q once:\n%s%s", want, out, l.stderr)
		}
	}
	k.with(t, func(s *store.Store) {
		reason := "the update to " + b + " failed: the new core " + filepath.Join(k.programs(), "kiln-"+b) +
			" did not report healthy within 2m0s; the core went back to " + k.a
		if m, err := s.Merging(); err != nil || m != (store.Merging{Off: reason, Failed: b}) {
			t.Errorf("merging is %+v, %v, want off with %q", m, err, reason)
		}
		h, err := s.InstanceHistory()
		if err != nil || len(h) != 1 || h[0].Cause != store.CauseRule || h[0].MergingOff != reason || h[0].Failed != b {
			t.Errorf("the instance's history is %+v, %v, want one entry by the rule", h, err)
		}
	})
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

// buildCount is how often the core built a program on the engine.
func buildCount(e *fakeEngine) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.builds)
}

// besideCore opens the bench's store beside a running core, gives it to f,
// and closes it.
func (k *kilnBench) besideCore(t *testing.T, f func(s *store.Store)) {
	t.Helper()
	s, err := store.Open(filepath.Join(k.cfg.Data, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f(s)
}

// TestNoUpdateToFailed starts the previous core after a failed update to b,
// and then starts it again without the launcher's variables: automatic
// merging is still off, the core does not update to b, and it updates once
// main has moved past b.
func TestNoUpdateToFailed(t *testing.T) {
	k := newKilnBench(t, kilnModule)
	setFetchEvery(t, 20*time.Millisecond)
	k.twoCards(t)
	k.copyStore(t)
	b := k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
	l := launch(t, child{Command: "core", Config: k.cfg, NoEngine: true}, k.failedEnv(b)...)
	l.until(t, "healthy")
	l.cmd.Process.Signal(os.Interrupt)
	l.exited(t, 0)

	e := newFakeEngine()
	h := startHanding(t, k.cfg, e, true)
	if line := h.line(t); line != "healthy" {
		t.Fatalf("the core wrote %q\n%s", line, h.out)
	}
	waitRounds(t, e, 10)
	h.noLine(t, 50*time.Millisecond)
	if n := buildCount(e); n != 0 {
		t.Errorf("the restarted core built %d times", n)
	}
	if out := h.out.String(); strings.Count(out, "kiln core: "+b+" failed as an update; this core waits for main to move past it\n") != 1 {
		t.Errorf("the restarted core did not say once that %s failed:\n%s", b, out)
	}
	k.besideCore(t, func(s *store.Store) {
		if m, err := s.Merging(); err != nil || m.Off == "" || m.Failed != b {
			t.Errorf("merging is %+v, %v, want off for %s", m, err, b)
		}
	})

	c := k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved again.\n"}) // on top of b
	if line, want := h.line(t), "replace "+filepath.Join(k.programs(), "kiln-"+c); line != want {
		t.Fatalf("the core wrote %q, want %q\n%s", line, want, h.out)
	}
	h.returned(t)
}

// TestMergingOnRetries has automatic merging off for b with main at b: the
// core does not update, and once the developer has turned merging on it
// updates to b.
func TestMergingOnRetries(t *testing.T) {
	k := newKilnBench(t, kilnModule)
	setFetchEvery(t, 20*time.Millisecond)
	k.twoCards(t)
	b := k.move(t, "", map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
	k.with(t, func(s *store.Store) {
		if _, err := s.TurnMergingOff("the update to "+b+" failed: it exited; the core went back to "+k.a, b); err != nil {
			t.Fatal(err)
		}
	})
	e := newFakeEngine()
	h := startHanding(t, k.cfg, e, true)
	if line := h.line(t); line != "healthy" {
		t.Fatalf("the core wrote %q\n%s", line, h.out)
	}
	waitRounds(t, e, 10)
	h.noLine(t, 50*time.Millisecond)
	if n := buildCount(e); n != 0 {
		t.Errorf("the core built %d times while merging was off", n)
	}
	k.besideCore(t, func(s *store.Store) {
		if err := s.TurnMergingOn(); err != nil {
			t.Fatal(err)
		}
	})
	if line, want := h.line(t), "replace "+filepath.Join(k.programs(), "kiln-"+b); line != want {
		t.Fatalf("the core wrote %q, want %q\n%s", line, want, h.out)
	}
	h.returned(t)
}
