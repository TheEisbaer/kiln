package tick

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/seal"
	"github.com/TheEisbaer/kiln/internal/settings"
	"github.com/TheEisbaer/kiln/internal/store"
)

// TestCacheDue fills work's cache when what it is filled from changes, and
// only then: a commit of a file that is neither the toolchain's nor the
// cache's input fills nothing, and the run mounts the same volume; one of the
// cache's input fills a new volume and builds no run image; a new cache
// Dockerfile fills a new volume; a new toolchain Dockerfile builds both
// images and fills a new volume. Each run of work mounts the volume of that
// moment at the cache's path; Kiln's own run mounts none.
func TestCacheDue(t *testing.T) {
	b := newCacheBench(t)
	images := []string{"kiln-toolchain-work", "kiln-run-work", "kiln-cache-work"}
	for i, step := range []struct {
		name   string
		change func()
		builds []string // the images built
		fill   bool     // whether a new volume is filled
	}{
		{"the first run", func() {}, images, true},
		{"a file that is not an input", func() { commitMain(t, b.work.Copy, map[string]string{"NOTES.md": "changed\n"}) }, nil, false},
		{"the cache's input", func() { commitMain(t, b.work.Copy, map[string]string{"packages.lock": "two\n"}) }, images[2:], true},
		{"the cache's Dockerfile", func() {
			if err := os.WriteFile(b.work.Toolchain.Cache.Dockerfile, []byte(benchCacheDockerfile+"RUN true\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, images[2:], true},
		{"the toolchain's Dockerfile", func() {
			if err := os.WriteFile(b.dockerfile, []byte(benchDockerfile+"RUN true\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, images, true},
	} {
		fromBuilds, fromFills := len(b.builds()), len(b.fills())
		step.change()
		var k int64
		if i == 0 {
			k = b.card(t, store.KilnRepo)
		}
		w := b.card(t, "work")
		b.tick(t)
		if step.builds != nil {
			b.buildDone(t)
			b.tick(t)
		}
		b.waitImage(t, w, "kiln-run-work")
		if got := namesOf(b.builds()[fromBuilds:]); !slices.Equal(got, step.builds) && !(len(got) == 0 && step.builds == nil) {
			t.Errorf("%s: the builds are %q, want %q", step.name, got, step.builds)
		}
		fills := b.fills()
		if n := len(fills) - fromFills; n != map[bool]int{false: 0, true: 1}[step.fill] {
			t.Errorf("%s: %d fills, want a new one %v", step.name, n, step.fill)
		}
		last := fills[len(fills)-1]
		if last.Repo != "work" || last.Path != benchCachePath || last.Memory != "" || last.CPUs != 0 {
			t.Errorf("%s: the fill is %+v", step.name, last)
		}
		if step.fill && fromFills > 0 && last.Digest == fills[fromFills-1].Digest {
			t.Errorf("%s: the new fill has the digest of the one before, %s", step.name, last.Digest)
		}
		want := seal.RunCache{Volume: seal.CacheVolume("work", last.Digest), Path: benchCachePath}
		if got := b.cacheOf(w); got == nil || *got != want {
			t.Errorf("%s: the run mounts %+v, want %+v", step.name, got, want)
		}
		if i == 0 {
			b.e.waitStarted(t, group(k), group(w))
			b.waitImage(t, k, "kiln-run")
			if got := b.cacheOf(k); got != nil {
				t.Errorf("Kiln's own run mounts %+v, want none", got)
			}
			b.finish(t, k)
			cache := namesOf(b.builds())
			if tc := b.builds()[slices.Index(cache, "kiln-cache-work")]; tc.Dockerfile != cacheDockerfile ||
				tc.Toolchain != "kiln-toolchain-work" || tc.NoNetwork || tc.Labels[seal.DigestLabel] != last.Digest ||
				len(tc.entries) == 0 || tc.entries[0] != cacheDockerfile || tc.first != benchCacheDockerfile ||
				!slices.Contains(tc.entries, "packages.lock") {
				t.Errorf("the cache image's build is %+v, from %q", tc.Image, tc.entries)
			}
		}
		b.finish(t, w)
	}
	for _, line := range []string{"kiln core: repo work: filling kiln-cache-work-", ": there is none\n",
		": the cache's Dockerfile, its inputs or the toolchain changed\n", "kiln core: repo work: filled kiln-cache-work-"} {
		if !strings.Contains(b.out.String(), line) {
			t.Errorf("the core did not print %q:\n%s", line, b.out)
		}
	}
	if errOut := b.errOut.String(); errOut != "" {
		t.Errorf("the core printed errors:\n%s", errOut)
	}
}

// TestCacheFillFails holds only work's runs while its fill fails: Kiln's card
// runs, work's card waits for its run as it was, the failure is printed once
// per digest with the fill's reason, the same digests are tried again a
// minute after the failure and not before, and a new cache Dockerfile at
// once. Once a fill succeeds, work's card runs on the volume.
func TestCacheFillFails(t *testing.T) {
	b := newCacheBench(t)
	b.e.mu.Lock()
	b.e.fillFails = true
	b.e.mu.Unlock()
	k, w := b.card(t, store.KilnRepo), b.card(t, "work")
	b.tick(t)
	b.buildDone(t)
	b.e.waitStarted(t, group(k))
	for range 3 {
		b.tick(t)
		b.buildDone(t)
	}
	if n := len(b.fills()); n != 1 {
		t.Errorf("the fill was tried %d times within a minute, want once", n)
	}
	b.assertWaits(t, w)
	line := "kiln core: repo work: the cache kiln-cache-work-"
	reason := " was not filled: the fill exited with status 1, as the test has it; the repo's runs wait, and it is tried again at once " +
		"when the cache's Dockerfile, its inputs or the toolchain change, and otherwise a minute after this failure\n"
	if n := strings.Count(b.errOut.String(), line); n != 1 || !strings.Contains(b.errOut.String(), reason) {
		t.Errorf("the failure was printed %d times, want once with its reason:\n%s", n, b.errOut)
	}

	b.clock.advance(retryAfter + time.Second)
	b.tick(t)
	b.buildDone(t)
	b.tick(t)
	if n := len(b.fills()); n != 2 {
		t.Errorf("the fill was tried %d times after a minute, want twice", n)
	}
	if n := strings.Count(b.errOut.String(), line); n != 1 {
		t.Errorf("the failure of the same digest was printed %d times, want once:\n%s", n, b.errOut)
	}
	b.assertWaits(t, w)

	// A new cache Dockerfile is tried at once, and its failure printed.
	if err := os.WriteFile(b.work.Toolchain.Cache.Dockerfile, []byte(benchCacheDockerfile+"RUN false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.tick(t)
	b.buildDone(t)
	if n := len(b.fills()); n != 3 {
		t.Errorf("the fill of a new Dockerfile was tried %d times in all, want 3", n)
	}
	if n := strings.Count(b.errOut.String(), line); n != 2 {
		t.Errorf("the failures were printed %d times, want twice:\n%s", n, b.errOut)
	}
	if strings.Contains(b.errOut.String(), "was not built") {
		t.Errorf("the core printed a failed build:\n%s", b.errOut)
	}
	b.assertWaits(t, w)

	b.e.mu.Lock()
	b.e.fillFails = false
	b.e.mu.Unlock()
	b.clock.advance(retryAfter + time.Second)
	b.tick(t)
	b.buildDone(t)
	b.tick(t)
	b.waitImage(t, w, "kiln-run-work")
	fills := b.fills()
	if got, want := b.cacheOf(w), seal.CacheVolume("work", fills[len(fills)-1].Digest); got == nil || got.Volume != want {
		t.Errorf("the run mounts %+v, want %s", got, want)
	}
}

// TestCacheFillRepeat starts the fill again in a second core when the first
// core's context ended while its fill was held, and starts work's run only
// once that fill is done.
func TestCacheFillRepeat(t *testing.T) {
	b := newCacheBench(t)
	gate := make(chan struct{})
	b.e.mu.Lock()
	b.e.fillGate = gate
	b.e.mu.Unlock()
	w := b.card(t, "work")
	b.tick(t)
	waitUntil(t, "the fill is held", func() bool { return len(b.fills()) == 1 })
	b.tick(t)
	b.assertWaits(t, w)
	b.cancel()
	b.c.followers.Wait()
	b.assertWaits(t, w)

	b.newCore(t, b.c.store)
	b.tick(t)
	waitUntil(t, "the fill is held again", func() bool { return len(b.fills()) == 2 })
	b.tick(t)
	b.assertWaits(t, w)
	close(gate)
	b.buildDone(t)
	b.tick(t)
	b.waitImage(t, w, "kiln-run-work")
	fills := b.fills()
	if fills[0] != fills[1] {
		t.Errorf("the second fill is %+v, want the first's, %+v", fills[1], fills[0])
	}
	if got := b.cacheOf(w); got == nil || got.Volume != seal.CacheVolume("work", fills[1].Digest) {
		t.Errorf("the run mounts %+v", got)
	}
	if names := namesOf(b.builds()); !slices.Equal(names, []string{"kiln-toolchain-work", "kiln-run-work", "kiln-cache-work"}) {
		t.Errorf("the builds are %q; the second core should build nothing again", names)
	}
}

// removals are the calls of removal so far, by name.
func (b *toolchainBench) removals() []string {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	return slices.Clone(b.e.removals)
}

// hasVolume reports whether the engine has a volume of the name, made by a
// fill or by the test.
func (b *toolchainBench) hasVolume(name string) bool {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	_, made := b.e.volumes[name]
	_, foreign := b.e.foreign[name]
	return made || foreign
}

// fillTo commits a new content of work's cache input, writes a card of work,
// (none when content is empty), and ticks until its fill is done and its run is carried. It returns the
// card and the filled volume.
func (b *toolchainBench) fillTo(t *testing.T, content string) (int64, string) {
	t.Helper()
	if content != "" {
		commitMain(t, b.work.Copy, map[string]string{"packages.lock": content})
	}
	n := len(b.fills())
	w := b.card(t, "work")
	b.tick(t)
	waitUntil(t, "the fill is made", func() bool { return len(b.fills()) > n })
	b.buildDone(t)
	b.tick(t)
	b.waitImage(t, w, "kiln-run-work")
	fills := b.fills()
	return w, seal.CacheVolume("work", fills[len(fills)-1].Digest)
}

// restart makes a core afresh and runs its start pass to its end.
func (b *toolchainBench) restart(t *testing.T) {
	t.Helper()
	b.cancel()
	b.c.followers.Wait()
	b.newCore(t, b.c.store)
	b.startPass(t)
}

// startPass runs the core's start pass to its end.
func (b *toolchainBench) startPass(t *testing.T) {
	t.Helper()
	b.c.removeOldCachesAtStart(b.ctx)
	b.buildDone(t)
}

// TestOldCachesRemoved removes an old volume once no container uses it, at
// the next pass, never the current one, and never one without the label.
func TestOldCachesRemoved(t *testing.T) {
	b := newCacheBench(t)
	w1, v1 := b.fillTo(t, "")
	zeros := seal.CacheVolume("work", strings.Repeat("0", 64))
	ones := seal.CacheVolume("work", strings.Repeat("1", 64))
	b.e.mu.Lock()
	b.e.foreign[zeros], b.e.foreign[ones] = "", "other"
	b.e.mu.Unlock()

	w2, v2 := b.fillTo(t, "two\n")
	if got := b.removals(); len(got) != 0 {
		t.Errorf("after the fill of %s the removals are %q, want none: card %d's container uses %s", v2, got, w1, v1)
	}
	for _, v := range []string{v1, v2, zeros, ones} {
		if !b.hasVolume(v) {
			t.Errorf("%s is gone after the fill of the second volume", v)
		}
	}
	b.finish(t, w1)
	b.finish(t, w2)
	if !b.hasVolume(v1) || len(b.removals()) != 0 {
		t.Errorf("%s was removed without a pass: %q", v1, b.removals())
	}

	b.restart(t)
	if got := b.removals(); !slices.Equal(got, []string{v1}) {
		t.Errorf("the start pass removed %q, want %s", got, v1)
	}
	if want := "kiln core: repo work: removed the old cache volume " + v1 + "\n"; !strings.Contains(b.out.String(), want) {
		t.Errorf("the core did not print %q:\n%s", want, b.out)
	}
	if b.hasVolume(v1) || !b.hasVolume(v2) || !b.hasVolume(zeros) || !b.hasVolume(ones) {
		t.Errorf("after the start pass: first there %v, second there %v, unlabelled there %v, other's there %v",
			b.hasVolume(v1), b.hasVolume(v2), b.hasVolume(zeros), b.hasVolume(ones))
	}

	_, v3 := b.fillTo(t, "three\n")
	waitUntil(t, "the fill of the third volume removes the second", func() bool { return slices.Contains(b.removals(), v2) })
	if b.hasVolume(v2) || !b.hasVolume(v3) || !b.hasVolume(zeros) || !b.hasVolume(ones) {
		t.Errorf("after the third fill: second there %v, third there %v, unlabelled there %v, other's there %v",
			b.hasVolume(v2), b.hasVolume(v3), b.hasVolume(zeros), b.hasVolume(ones))
	}
	if got := b.errOut.String(); got != "" {
		t.Errorf("the core printed errors:\n%s", got)
	}
}

// TestOldCachesRemoveFails prints a failed removal once, tries it again at
// each pass, holds no run, and removes the volume at the first pass after
// the failure is gone.
func TestOldCachesRemoveFails(t *testing.T) {
	b := newCacheBench(t)
	w1, v1 := b.fillTo(t, "")
	b.finish(t, w1)
	b.e.mu.Lock()
	b.e.rmFails[v1] = "volume is busy, as the test has it"
	b.e.mu.Unlock()

	w2, v2 := b.fillTo(t, "two\n")
	waitUntil(t, "the fill's pass tries the volume", func() bool { return len(b.removals()) == 1 })
	b.startPass(t)
	b.startPass(t)
	if got := b.removals(); !slices.Equal(got, []string{v1, v1, v1}) {
		t.Errorf("the removals are %q, want three tries of %s", got, v1)
	}
	line := "kiln core: repo work: the old cache volume " + v1 + " was not removed: volume is busy, as the test has it; " +
		"it is tried again after the repo's next fill and at the core's next start\n"
	if n := strings.Count(b.errOut.String(), line); n != 1 || strings.Count(b.errOut.String(), "\n") != 1 {
		t.Errorf("the failure was printed %d times, want once:\n%s", n, b.errOut)
	}
	if got := b.cacheOf(w2); got == nil || got.Volume != v2 {
		t.Errorf("card %d's run mounts %+v, want %s", w2, got, v2)
	}
	if strings.Contains(b.errOut.String(), "was not filled") {
		t.Errorf("the failure held the fill:\n%s", b.errOut)
	}

	b.e.mu.Lock()
	b.e.rmFails = map[string]string{}
	b.e.mu.Unlock()
	b.finish(t, w2)
	b.startPass(t)
	if b.hasVolume(v1) {
		t.Errorf("%s stays after the failure is gone", v1)
	}
}

// TestOldCachesStartGuard holds the start pass: no fill of the repo starts
// beside it, and one starts once it is done.
func TestOldCachesStartGuard(t *testing.T) {
	b := newCacheBench(t)
	gate := make(chan struct{})
	b.e.mu.Lock()
	b.e.rmGate = gate
	b.e.mu.Unlock()
	b.c.removeOldCachesAtStart(b.ctx)
	// The guard is taken before the call returns, so the first tick of Run
	// already sees it, and not later in the pass's goroutine.
	if !b.building() {
		t.Fatal("the start pass returned before it took work's build guard")
	}
	w := b.card(t, "work")
	for range 3 {
		b.tick(t)
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(b.fills()); n != 0 {
		t.Errorf("%d fills ran while the start pass was held, want none", n)
	}
	b.assertWaits(t, w)
	close(gate)
	b.buildDone(t)
	b.tick(t)
	waitUntil(t, "the fill starts once the pass is done", func() bool { return len(b.fills()) == 1 })
	b.buildDone(t)
	b.tick(t)
	b.waitImage(t, w, "kiln-run-work")
}

// endedEngine is an engine whose RemoveOldCaches gives a set answer.
type endedEngine struct {
	engine
	removals []seal.CacheRemoval
	err      error
}

func (e endedEngine) RemoveOldCaches(context.Context, string, string) ([]seal.CacheRemoval, error) {
	return e.removals, e.err
}

// TestOldCachesEndedContext prints a removal that was done although the
// context has ended, and prints neither a failure nor a listing error then.
func TestOldCachesEndedContext(t *testing.T) {
	v1, v2 := "kiln-cache-work-"+strings.Repeat("1", 64), "kiln-cache-work-"+strings.Repeat("2", 64)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		name    string
		fake    endedEngine
		wantOut string
	}{
		{"a removal and a failure", endedEngine{removals: []seal.CacheRemoval{{Volume: v1}, {Volume: v2, Err: errors.New("signal: killed")}}},
			"kiln core: repo work: removed the old cache volume " + v1 + "\n"},
		{"a listing error", endedEngine{err: errors.New("signal: killed")}, ""},
	} {
		var out, errOut bytes.Buffer
		c2 := &core{engine: c.fake, out: &out, errOut: &errOut}
		c2.removeOldCaches(ctx, settings.Repo{Name: "work"}, strings.Repeat("c", 64))
		if out.String() != c.wantOut || errOut.Len() != 0 {
			t.Errorf("%s: out = %q, errOut = %q; want out %q and no errOut", c.name, out.String(), errOut.String(), c.wantOut)
		}
	}
}
