package tick

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/seal"
	"github.com/TheEisbaer/kiln/internal/settings"
	"github.com/TheEisbaer/kiln/internal/store"
	"github.com/TheEisbaer/kiln/internal/web"
)

// toolchainBench is a core on the fake engine with two repos: Kiln's own,
// without a toolchain, and work, with one, whose Dockerfile is in a directory
// outside both repos. The test ticks the core itself.
type toolchainBench struct {
	c           *core
	e           *fakeEngine
	ctx         context.Context
	out, errOut *syncBuilder
	kiln, work  settings.Repo
	dockerfile  string
	clock       *fakeClock
}

// The toolchain's Dockerfile of the bench, and the files of work's main.
const benchDockerfile = "FROM kiln-toolchain\nCOPY lock.json /opt/cache/\n"

var benchFiles = map[string]string{"NOTES.md": "# Notes\n", "lock.json": "{}\n", "deps/a.txt": "a\n", "other.txt": "other\n"}

func newToolchainBench(t *testing.T, inputs ...string) *toolchainBench {
	t.Helper()
	data := t.TempDir()
	s, err := store.Open(filepath.Join(data, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	kilnCopy, _ := bareRepo(t, map[string]string{"NOTES.md": "# Notes of kiln\n"})
	workCopy, _ := bareRepo(t, benchFiles)
	b := &toolchainBench{e: newFakeEngine(), out: &syncBuilder{}, errOut: &syncBuilder{}, clock: newFakeClock()}
	b.dockerfile = filepath.Join(t.TempDir(), "work.Dockerfile")
	if err := os.WriteFile(b.dockerfile, []byte(benchDockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if inputs == nil {
		inputs = []string{"lock.json", "deps"}
	}
	b.kiln = settings.Repo{Name: store.KilnRepo, Copy: kilnCopy, Own: true}
	b.work = settings.Repo{Name: "work", Copy: workCopy, Toolchain: &settings.Toolchain{Dockerfile: b.dockerfile, Inputs: inputs}}
	b.c = &core{Config: Config{Data: data, Repos: []settings.Repo{b.kiln, b.work}, Image: "kiln-run", Proxy: "kiln-proxy",
		Model: "claude-opus-5-5", Runs: 3}, store: s, instance: "ab12cd34", out: b.out, errOut: b.errOut,
		engine: b.e, followed: map[int64]string{}, refused: map[int64]bool{}, stuck: map[string]stuckNote{},
		publishing: map[int64]bool{}, checking: map[int64]*checkState{}, tested: map[int64]hostTest{},
		activity: web.NewActivity(), clock: b.clock}
	ctx, cancel := context.WithCancel(t.Context())
	b.ctx = ctx
	t.Cleanup(func() { cancel(); b.c.followers.Wait() })
	return b
}

// card writes a card of the repo waiting for a spec run.
func (b *toolchainBench) card(t *testing.T, repo string) int64 {
	t.Helper()
	c, err := b.c.store.CreateIn(repo, "A card of "+repo+".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.store.Move(c.ID, waiting, store.CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func (b *toolchainBench) tick(t *testing.T) {
	t.Helper()
	if err := b.c.tick(b.ctx); err != nil {
		t.Fatal(err)
	}
}

// building reports whether work's build runs.
func (b *toolchainBench) building() bool {
	b.c.mu.Lock()
	defer b.c.mu.Unlock()
	st := b.c.toolchains["work"]
	return st != nil && st.building
}

// buildDone waits until work's build has ended.
func (b *toolchainBench) buildDone(t *testing.T) {
	t.Helper()
	waitUntil(t, "work's build ends", func() bool { return !b.building() })
}

// builds are the calls of BuildImage so far.
func (b *toolchainBench) builds() []imageBuild {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	return slices.Clone(b.e.imageBuilds)
}

func namesOf(builds []imageBuild) []string {
	names := []string{}
	for _, im := range builds {
		names = append(names, im.Name)
	}
	return names
}

// started reports whether a card's run was given to Carry, and its inputs.
func (b *toolchainBench) started(card int64) (bool, string) {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	in, ok := b.e.inputs[card]
	return ok, in.Image
}

// waitImage waits until a card's run is carried, and asserts its image.
func (b *toolchainBench) waitImage(t *testing.T, card int64, image string) {
	t.Helper()
	waitUntil(t, "the card's run is carried", func() bool { ok, _ := b.started(card); return ok })
	if _, got := b.started(card); got != image {
		t.Errorf("card %d's run is on %q, want %q", card, got, image)
	}
}

// finish lets a card's run end and waits until it has.
func (b *toolchainBench) finish(t *testing.T, card int64) {
	t.Helper()
	b.e.open(card, "exit")
	b.e.waitGone(t, card)
	waitUntil(t, "the card waits no longer for a run", func() bool {
		c, err := b.c.store.Card(card)
		return err == nil && c.WaitsFor != store.Run && !b.c.follows(card)
	})
}

// assertWaits asserts that a card waits for its spec run as it did, with no
// run and no move.
func (b *toolchainBench) assertWaits(t *testing.T, card int64) {
	t.Helper()
	c, err := b.c.store.Card(card)
	if err != nil {
		t.Fatal(err)
	}
	n, err := b.c.store.RunCount(card)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := b.c.store.History(card)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.started(card); ok || c.State != waiting || n != 0 || len(entries) != 2 {
		t.Errorf("card %d is in %v with %d runs, %d entries and carried %v; want it waiting for its spec run as it was",
			card, c.State, n, len(entries), ok)
	}
}

// commitMain commits files to the main of a bare repo made by bareRepo, in
// its origin, which the tick fetches from, and in the copy.
func commitMain(t *testing.T, bare string, files map[string]string) {
	t.Helper()
	work := filepath.Join(filepath.Dir(bare), "work")
	write(t, work, files)
	gitIn(t, work, "add", "--all")
	gitIn(t, work, "commit", "--quiet", "--message", "a change")
	gitIn(t, work, "push", "--quiet", bare, "HEAD:refs/heads/main")
}

// TestToolchainImage builds work's two images beside the tick, from the
// instance's Dockerfile and work's main, and gives work's runs
// kiln-run-work, while Kiln's card runs on Kiln's run image, also while
// work's build is held.
func TestToolchainImage(t *testing.T) {
	b := newToolchainBench(t)
	k, w := b.card(t, store.KilnRepo), b.card(t, "work")
	gate := make(chan struct{})
	b.e.mu.Lock()
	b.e.imageGate = gate
	b.e.mu.Unlock()

	b.tick(t)
	b.e.waitStarted(t, group(k))
	b.waitImage(t, k, "kiln-run")
	if !b.building() {
		t.Fatal("work's build does not run")
	}
	b.tick(t)
	b.assertWaits(t, w)
	close(gate)
	b.buildDone(t)
	b.tick(t)
	b.e.waitStarted(t, group(k), group(w))
	b.waitImage(t, w, "kiln-run-work")

	builds := b.builds()
	if names := namesOf(builds); !slices.Equal(names, []string{"kiln-toolchain-work", "kiln-run-work"}) {
		t.Fatalf("the builds are %q, want kiln-toolchain-work and kiln-run-work", names)
	}
	tc, ri := builds[0], builds[1]
	if tc.Tag != "" || tc.NoNetwork || tc.Toolchain != "" || tc.Dockerfile != toolchainDockerfile || tc.Labels[seal.DigestLabel] == "" {
		t.Errorf("the toolchain's build is %+v", tc.Image)
	}
	if len(tc.entries) == 0 || tc.entries[0] != toolchainDockerfile || tc.first != benchDockerfile {
		t.Errorf("the toolchain's context begins with %q, %q; want %s with the instance's Dockerfile", tc.entries, tc.first, toolchainDockerfile)
	}
	for name := range benchFiles {
		if !slices.Contains(tc.entries, name) {
			t.Errorf("the toolchain's context %q has no %s of work's main", tc.entries, name)
		}
	}
	if ri.Tag != "" || ri.NoNetwork || ri.Toolchain != "kiln-toolchain-work" || ri.Dockerfile != "Dockerfile" ||
		!slices.Equal(ri.entries, []string{"Dockerfile"}) || ri.Labels[seal.DigestLabel] == "" ||
		ri.Labels[seal.DigestLabel] == tc.Labels[seal.DigestLabel] {
		t.Errorf("the run image's build is %+v, from %q", ri.Image, ri.entries)
	}
	for _, ev := range b.e.events() {
		if strings.HasPrefix(ev, "tag ") || strings.HasPrefix(ev, "rm ") {
			t.Errorf("the engine was asked to %s", ev)
		}
	}
	want := "kiln core: repo work: building kiln-toolchain-work and kiln-run-work from main at "
	if out := b.out.String(); !strings.Contains(out, want) || !strings.Contains(out, ": there is none\n") ||
		!strings.Contains(out, "kiln core: repo work: built kiln-run-work\n") {
		t.Errorf("the core printed\n%s", out)
	}
	if errOut := b.errOut.String(); errOut != "" {
		t.Errorf("the core printed errors:\n%s", errOut)
	}
}

// TestToolchainDue builds work's images again when what they are built from
// changes, and only then: a commit of a file that is not an input builds
// nothing; one of an input file, or of a file inside an input directory, and
// a new Dockerfile build both images; a new layer, or a run image that is
// gone, builds the run image only.
func TestToolchainDue(t *testing.T) {
	b := newToolchainBench(t)
	layer := t.TempDir()
	writeLayer := func(text string) {
		if err := os.WriteFile(filepath.Join(layer, "Dockerfile"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeLayer("ARG TOOLCHAIN\nFROM ${TOOLCHAIN}\n")
	b.c.Layer = layer
	both, runOnly := []string{"kiln-toolchain-work", "kiln-run-work"}, []string{"kiln-run-work"}
	for _, step := range []struct {
		name   string
		change func()
		want   []string
	}{
		{"the first run", func() {}, both},
		{"a file that is not an input", func() { commitMain(t, b.work.Copy, map[string]string{"other.txt": "changed\n"}) }, nil},
		{"an input file", func() { commitMain(t, b.work.Copy, map[string]string{"lock.json": `{"a":1}` + "\n"}) }, both},
		{"a file in an input directory", func() { commitMain(t, b.work.Copy, map[string]string{"deps/b.txt": "b\n"}) }, both},
		{"the Dockerfile", func() {
			if err := os.WriteFile(b.dockerfile, []byte(benchDockerfile+"RUN true\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, both},
		{"Kiln's layer", func() { writeLayer("ARG TOOLCHAIN\nFROM ${TOOLCHAIN}\nUSER 1000\n") }, runOnly},
		{"the run image gone", func() {
			b.e.mu.Lock()
			delete(b.e.book.Images, "kiln-run-work:latest")
			b.e.mu.Unlock()
		}, runOnly},
	} {
		from := len(b.builds())
		step.change()
		w := b.card(t, "work")
		b.tick(t)
		if step.want != nil {
			b.buildDone(t)
			b.tick(t)
		}
		b.waitImage(t, w, "kiln-run-work")
		if got := namesOf(b.builds()[from:]); !slices.Equal(got, step.want) && !(len(got) == 0 && step.want == nil) {
			t.Errorf("%s: the builds are %q, want %q", step.name, got, step.want)
		}
		b.finish(t, w)
	}
}

// TestToolchainBuildFails holds only work's runs while its build fails: the
// card waits for its run as it was, Kiln's card runs, the same digests are
// tried again a minute after the failure and not before, and changed ones at
// once; the failure is printed once per digest. Once the build succeeds,
// work's card runs on kiln-run-work. A failed run image is built again
// without its toolchain, which carries its digest already.
func TestToolchainBuildFails(t *testing.T) {
	t.Run("the toolchain", func(t *testing.T) {
		b := newToolchainBench(t)
		b.e.imageFails["kiln-toolchain-work"] = true
		k, w := b.card(t, store.KilnRepo), b.card(t, "work")
		b.tick(t)
		b.buildDone(t)
		b.e.waitStarted(t, group(k))
		for range 3 {
			b.tick(t)
		}
		if n := len(b.builds()); n != 1 {
			t.Errorf("the build was tried %d times within a minute, want once", n)
		}
		b.clock.advance(retryAfter + time.Second)
		b.tick(t)
		b.buildDone(t)
		b.tick(t)
		if n := len(b.builds()); n != 2 {
			t.Errorf("the build was tried %d times after a minute, want twice", n)
		}
		line := "kiln core: repo work: kiln-toolchain-work was not built: "
		if n := strings.Count(b.errOut.String(), line); n != 1 {
			t.Errorf("the failure was printed %d times, want once:\n%s", n, b.errOut)
		}
		if !strings.Contains(b.errOut.String(), "the image's build failed, as the test has it; the repo's runs wait") {
			t.Errorf("the failure does not say why:\n%s", b.errOut)
		}
		b.assertWaits(t, w)

		// A new Dockerfile is tried at once, and its failure printed.
		if err := os.WriteFile(b.dockerfile, []byte(benchDockerfile+"RUN false\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		b.tick(t)
		b.buildDone(t)
		if n := len(b.builds()); n != 3 {
			t.Errorf("the build of a new Dockerfile was tried %d times in all, want 3", n)
		}
		if n := strings.Count(b.errOut.String(), line); n != 2 {
			t.Errorf("the failures were printed %d times, want twice:\n%s", n, b.errOut)
		}
		b.assertWaits(t, w)

		b.e.mu.Lock()
		b.e.imageFails["kiln-toolchain-work"] = false
		b.e.mu.Unlock()
		b.clock.advance(retryAfter + time.Second)
		b.tick(t)
		b.buildDone(t)
		b.tick(t)
		b.waitImage(t, w, "kiln-run-work")
		if names := namesOf(b.builds()[3:]); !slices.Equal(names, []string{"kiln-toolchain-work", "kiln-run-work"}) {
			t.Errorf("the builds after the fix are %q", names)
		}
	})
	t.Run("the run image", func(t *testing.T) {
		b := newToolchainBench(t)
		b.e.imageFails["kiln-run-work"] = true
		k, w := b.card(t, store.KilnRepo), b.card(t, "work")
		b.tick(t)
		b.buildDone(t)
		b.e.waitStarted(t, group(k))
		b.tick(t)
		b.assertWaits(t, w)
		if names := namesOf(b.builds()); !slices.Equal(names, []string{"kiln-toolchain-work", "kiln-run-work"}) {
			t.Errorf("the builds are %q", names)
		}
		if !strings.Contains(b.errOut.String(), "kiln core: repo work: kiln-run-work was not built: ") {
			t.Errorf("the core printed\n%s", b.errOut)
		}
		b.e.mu.Lock()
		b.e.imageFails["kiln-run-work"] = false
		b.e.mu.Unlock()
		b.clock.advance(retryAfter + time.Second)
		b.tick(t)
		b.buildDone(t)
		b.tick(t)
		b.waitImage(t, w, "kiln-run-work")
		if names := namesOf(b.builds()[2:]); !slices.Equal(names, []string{"kiln-run-work"}) {
			t.Errorf("the builds after the fix are %q, want kiln-run-work only", names)
		}
		if !strings.Contains(b.out.String(), "building kiln-run-work from main at ") {
			t.Errorf("the core printed\n%s", b.out)
		}
	})
}

// TestToolchainNotBuilt starts no build and no run of work's card when its
// Dockerfile cannot be read, an input is not at main, or main holds the name
// the Dockerfile takes in the context; it prints the reason once, and Kiln's
// card runs.
func TestToolchainNotBuilt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inputs []string
		setup  func(b *toolchainBench)
		want   string
	}{
		{"no Dockerfile", nil, func(b *toolchainBench) {
			if err := os.Remove(b.dockerfile); err != nil {
				t.Fatal(err)
			}
		}, "kiln core: repo work: kiln-run-work is not built: the toolchain's Dockerfile: open "},
		{"an input missing at main", []string{"lock.json", "missing.txt"}, func(*toolchainBench) {},
			`kiln core: repo work: kiln-run-work is not built: the toolchain's input "missing.txt" is not at main's head `},
		{"the reserved name at main", nil, func(b *toolchainBench) {
			commitMain(t, b.work.Copy, map[string]string{toolchainDockerfile: "FROM scratch\n"})
		}, "holds Dockerfile.kiln-toolchain, the name the toolchain's Dockerfile takes in the build's context; the repo's runs wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newToolchainBench(t, tc.inputs...)
			tc.setup(b)
			k, w := b.card(t, store.KilnRepo), b.card(t, "work")
			for range 3 {
				b.tick(t)
			}
			b.e.waitStarted(t, group(k))
			b.assertWaits(t, w)
			if n := len(b.builds()); n != 0 || b.building() {
				t.Errorf("%d builds", n)
			}
			if n := strings.Count(b.errOut.String(), tc.want); n != 1 {
				t.Errorf("the reason was printed %d times, want once:\n%s", n, b.errOut)
			}
		})
	}
}
