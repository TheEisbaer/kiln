package tick

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/seal"
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
