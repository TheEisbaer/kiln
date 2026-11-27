package tick

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/TheEisbaer/kiln/images"
	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/seal"
	"github.com/TheEisbaer/kiln/internal/settings"
)

// A repo's own toolchain (ADR 0039). A repo of the settings file may name a
// Dockerfile kept in the instance, settings.Toolchain. For such a repo the
// core builds two images under their plain names: kiln-toolchain-<repo> from
// that Dockerfile, with `git archive` of the repo's main's head as the
// context and the Dockerfile added first as Dockerfile.kiln-toolchain, and
// kiln-run-<repo>, Kiln's layer, the one embedded in the running program
// (images.RunLayer), on top of it. Every run of the repo, its save included,
// uses kiln-run-<repo>.
//
// Each image carries the label kiln.digest, which says what it was built
// from: the toolchain's digest is a SHA-256 over the Dockerfile's bytes and
// the object IDs of the inputs at main's head; the run image's over the
// toolchain's digest and Kiln's layer's files. The labels are the facts
// (ADR 0035): what the core keeps in memory, which repo's build runs and
// which try failed last, is only a guard.
//
// When a card of the repo could start its run, the core works the digests out
// afresh. If kiln-run-<repo> carries the run image's digest, the run starts on
// it. Otherwise the core builds what differs beside the tick, counted among
// the followers, one build per repo at a time, and the card waits for a run
// as it is. After a failed build the next try comes at once when a digest has
// changed since, and otherwise a minute after the failure at the earliest.
// Other repos' runs go on throughout.
//
// A repo's toolchain may name a cache (ADR 0038, ADR 0039): a Dockerfile in
// the instance, built from main as the toolchain is, as Dockerfile.kiln-cache,
// with TOOLCHAIN set to kiln-toolchain-<repo>, into kiln-cache-<repo>, whose
// command fills the volume kiln-cache-<repo>-<digest> (seal.FillCache). The
// cache's digest is a SHA-256 over its Dockerfile's bytes, its path, the
// object IDs of its inputs at main's head and the toolchain's digest, not
// over Kiln's layer. The image and the fill come last in the repo's build,
// under its guard, and a run of the repo starts only once the volume of this
// moment's digest is filled (seal.CacheFilled); it mounts that volume
// read-only. A failed cache image build or fill holds the repo's runs, and is
// tried again as a failed toolchain build is. After each fill and once at the
// start, under the repo's guard, the core removes the repo's other cache
// volumes that no container uses (seal.RemoveOldCaches); a failure is printed
// once per volume and text and tried again on the next pass.

// toolchainDockerfile and cacheDockerfile are the names of the instance's
// Dockerfiles in the toolchain's and the cache's image's context, paths main
// may not hold.
const (
	toolchainDockerfile = "Dockerfile.kiln-toolchain"
	cacheDockerfile     = "Dockerfile.kiln-cache"
)

// maxToolchainDockerfile is the largest Dockerfile of a toolchain the core
// reads.
const maxToolchainDockerfile = 1 << 20

// retryAfter is the least time between two tries of a repo's build of the
// same digests.
const retryAfter = time.Minute

// The preambles of the three digests, which a change of what they cover
// changes.
const (
	toolchainPreamble = "kiln toolchain image digest 1\n"
	runImagePreamble  = "kiln run image digest 1\n"
	cachePreamble     = "kiln cache digest 1\n"
)

// toolchainImageOf and runImageOf are the names of a repo's own images.
func toolchainImageOf(repo string) string { return "kiln-toolchain-" + repo }
func runImageOf(repo string) string       { return "kiln-run-" + repo }

// imageOf is the run image of a repo's runs: kiln-run-<repo> for a repo with
// a toolchain, and Config.Image otherwise.
func (c *core) imageOf(repo settings.Repo) string {
	if repo.Toolchain != nil {
		return runImageOf(repo.Name)
	}
	return c.Image
}

// toolchainState is a repo's build as the core keeps it in memory, guarded
// by mu.
type toolchainState struct {
	building bool
	failed   string    // the run image's and the cache's digests of the last try, if it failed
	failedAt time.Time // when it failed
}

// repoImages is what a repo's images are built from at one moment.
type repoImages struct {
	head       string // main's head, the context's commit
	dockerfile []byte
	layer      []layerFile
	toolchain  string // the toolchain image's digest
	run        string // the run image's digest
	// The cache's Dockerfile and digest, nil and "" for a repo without a
	// cache.
	cacheDockerfile []byte
	cache           string
}

// layerFile is a file of Kiln's layer.
type layerFile struct {
	name string
	data []byte
}

// runImageReady reports whether a run of the repo may start now, and the
// cache it mounts: always, with none, for a repo without a toolchain, and for
// one with a toolchain once kiln-run-<repo> carries the digest of this moment
// and, for a repo with a cache, the cache's volume of this moment's digest is
// filled. Otherwise it starts the repo's build, unless one runs or the last
// try of the same digests failed less than retryAfter ago, and the run waits.
// A reason that keeps the build from starting, such as a Dockerfile that
// cannot be read or an input that main does not have, is printed once per
// repo and reason.
func (c *core) runImageReady(ctx context.Context, repo settings.Repo) (*seal.RunCache, bool) {
	if repo.Toolchain == nil {
		return nil, true
	}
	im, reason := c.repoImages(ctx, repo)
	if ctx.Err() != nil {
		return nil, false
	}
	have := ""
	if reason == "" {
		var err error
		if have, err = c.engine.ImageDigest(ctx, runImageOf(repo.Name)); err != nil {
			reason = err.Error()
		}
	}
	filled := false
	if reason == "" && have == im.run && im.cache != "" {
		var err error
		if filled, err = c.engine.CacheFilled(ctx, repo.Name, im.cache); err != nil {
			reason = err.Error()
		}
	}
	if ctx.Err() != nil {
		return nil, false
	}
	if reason != "" {
		reason = run.Printable(reason)
		c.sayOnceAs(c.errOut, "toolchain\x00"+repo.Name+"\x00"+reason,
			fmt.Sprintf("kiln core: repo %s: %s is not built: %s; the repo's runs wait", repo.Name, runImageOf(repo.Name), reason))
		return nil, false
	}
	if have == im.run && im.cache == "" {
		return nil, true
	}
	if have == im.run && filled {
		return &seal.RunCache{Volume: seal.CacheVolume(repo.Name, im.cache), Path: repo.Toolchain.Cache.Path}, true
	}
	key := im.run + "\x00" + im.cache
	c.mu.Lock()
	if c.toolchains == nil {
		c.toolchains = map[string]*toolchainState{}
	}
	st := c.toolchains[repo.Name]
	if st == nil {
		st = &toolchainState{}
		c.toolchains[repo.Name] = st
	}
	if st.building || (st.failed == key && c.now().Sub(st.failedAt) < retryAfter) {
		c.mu.Unlock()
		return nil, false
	}
	st.building = true
	c.mu.Unlock()
	c.followers.Add(1)
	go func() {
		defer c.followers.Done()
		image, err := c.buildRepoImages(ctx, repo, im, have)
		c.mu.Lock()
		st.building = false
		st.failed = ""
		if err != nil && ctx.Err() == nil {
			st.failed, st.failedAt = key, c.now()
		}
		c.mu.Unlock()
		if err == nil || ctx.Err() != nil {
			return
		}
		// BuildKit's output carries timings, so the line is said once per
		// digest, not per text.
		if volume := seal.CacheVolume(repo.Name, im.cache); im.cache != "" && image == volume {
			c.sayOnceAs(c.errOut, "cache failed\x00"+repo.Name+"\x00"+im.cache,
				fmt.Sprintf("kiln core: repo %s: the cache %s was not filled: %s; the repo's runs wait, and it is tried again at once "+
					"when the cache's Dockerfile, its inputs or the toolchain change, and otherwise a minute after this failure",
					repo.Name, volume, run.Printable(err.Error())))
			return
		}
		c.sayOnceAs(c.errOut, "toolchain failed\x00"+repo.Name+"\x00"+im.run,
			fmt.Sprintf("kiln core: repo %s: %s was not built: %s; the repo's runs wait, and it is tried again at once "+
				"when its Dockerfile, its inputs or Kiln's layer change, and otherwise a minute after this failure",
				repo.Name, image, run.Printable(err.Error())))
	}()
	return nil, false
}

// now is the core's clock's time, or the time for a core without a clock.
func (c *core) now() time.Time {
	if c.clock == nil {
		return time.Now()
	}
	return c.clock.Now()
}

// buildRepoImages builds a repo's images, with the network, from what they
// are built from at one moment: unless kiln-run-<repo> carries the run
// image's digest already, kiln-toolchain-<repo> unless it carries the
// toolchain's digest already, and then kiln-run-<repo>; and then, for a repo
// with a cache, the cache (fillCache). had is the run image's digest as the
// tick found it. It returns the image whose build failed, or the cache's
// volume when the cache's image or its fill failed, with the error.
func (c *core) buildRepoImages(ctx context.Context, repo settings.Repo, im repoImages, had string) (string, error) {
	if had != im.run {
		if image, err := c.buildRunImage(ctx, repo, im, had); err != nil {
			return image, err
		}
	}
	if im.cache == "" {
		return "", nil
	}
	return c.fillCache(ctx, repo, im)
}

// buildRunImage builds kiln-toolchain-<repo> unless it carries the
// toolchain's digest already, and then kiln-run-<repo>. It returns the image
// whose build failed, with the error.
func (c *core) buildRunImage(ctx context.Context, repo settings.Repo, im repoImages, had string) (string, error) {
	toolchain, runImage := toolchainImageOf(repo.Name), runImageOf(repo.Name)
	got, err := c.engine.ImageDigest(ctx, toolchain)
	if err != nil {
		return toolchain, err
	}
	building, why := runImage, "Kiln's layer changed"
	if got != im.toolchain {
		building, why = toolchain+" and "+runImage, "its Dockerfile or inputs changed"
	}
	if had == "" {
		why = "there is none"
	}
	fmt.Fprintf(c.out, "kiln core: repo %s: building %s from main at %s: %s\n", repo.Name, building, im.head, why)
	if got != im.toolchain {
		spec := seal.Image{Name: toolchain, Dockerfile: toolchainDockerfile, Labels: map[string]string{seal.DigestLabel: im.toolchain}}
		if err := c.buildFromMain(ctx, repo, im.head, im.dockerfile, spec); err != nil {
			return toolchain, err
		}
	}
	layer, err := layerTar(im.layer)
	if err != nil {
		return runImage, err
	}
	spec := seal.Image{Name: runImage, Dockerfile: "Dockerfile", Toolchain: toolchain, Labels: map[string]string{seal.DigestLabel: im.run}}
	if err := c.engine.BuildImage(ctx, spec, bytes.NewReader(layer)); err != nil {
		return runImage, err
	}
	fmt.Fprintf(c.out, "kiln core: repo %s: built %s\n", repo.Name, runImage)
	return "", nil
}

// fillCache fills the repo's cache volume of the cache's digest, unless it is
// filled: it builds kiln-cache-<repo>, unless it carries the cache's digest
// already, and fills the volume from it in kiln-fill-<repo>, with the network
// and the repo's memory and CPUs (seal.FillCache). It returns the volume when
// a step failed, with the error.
func (c *core) fillCache(ctx context.Context, repo settings.Repo, im repoImages) (string, error) {
	volume, image := seal.CacheVolume(repo.Name, im.cache), seal.CacheImage(repo.Name)
	filled, err := c.engine.CacheFilled(ctx, repo.Name, im.cache)
	if err != nil || filled {
		return volume, err
	}
	got, err := c.engine.ImageDigest(ctx, image)
	if err != nil {
		return volume, err
	}
	why := "it is not filled"
	switch {
	case got == "":
		why = "there is none"
	case got != im.cache:
		why = "the cache's Dockerfile, its inputs or the toolchain changed"
	}
	fmt.Fprintf(c.out, "kiln core: repo %s: filling %s from main at %s: %s\n", repo.Name, volume, im.head, why)
	if got != im.cache {
		spec := seal.Image{Name: image, Dockerfile: cacheDockerfile, Toolchain: toolchainImageOf(repo.Name),
			Labels: map[string]string{seal.DigestLabel: im.cache}}
		if err := c.buildFromMain(ctx, repo, im.head, im.cacheDockerfile, spec); err != nil {
			return volume, err
		}
	}
	err = c.engine.FillCache(ctx, seal.Fill{Repo: repo.Name, Digest: im.cache, Path: repo.Toolchain.Cache.Path,
		Memory: repo.Memory, CPUs: repo.CPUs})
	if err != nil {
		return volume, err
	}
	fmt.Fprintf(c.out, "kiln core: repo %s: filled %s\n", repo.Name, volume)
	c.removeOldCaches(ctx, repo, im.cache)
	return "", nil
}

// removeOldCaches removes the repo's cache volumes other than the one of
// digest that no container uses, and prints what it did: a removal on the
// output, a failure once per volume and text. A removal that was done is
// always printed; a failure or a listing error that comes with an ended
// context is not, since a cancelled call's error says nothing about the
// volume. It never changes what the caller does next. The caller holds the
// repo's build guard.
func (c *core) removeOldCaches(ctx context.Context, repo settings.Repo, digest string) {
	const ending = "; it is tried again after the repo's next fill and at the core's next start"
	removals, err := c.engine.RemoveOldCaches(ctx, repo.Name, digest)
	ended := ctx.Err() != nil
	if err != nil {
		if ended {
			return
		}
		text := run.Printable(err.Error())
		c.sayOnceAs(c.errOut, "old caches\x00"+repo.Name+"\x00"+text,
			fmt.Sprintf("kiln core: repo %s: its old cache volumes were not looked at: %s%s", repo.Name, text, ending))
		return
	}
	for _, r := range removals {
		if r.Err == nil {
			fmt.Fprintf(c.out, "kiln core: repo %s: removed the old cache volume %s\n", repo.Name, r.Volume)
			continue
		}
		if ended {
			continue
		}
		text := run.Printable(r.Err.Error())
		c.sayOnceAs(c.errOut, "old cache\x00"+r.Volume+"\x00"+text,
			fmt.Sprintf("kiln core: repo %s: the old cache volume %s was not removed: %s%s", repo.Name, r.Volume, text, ending))
	}
}

// removeOldCachesAtStart runs the pass once for each repo with a cache, at
// the core's start. Each repo's build guard is taken here, before the first
// tick, so that no fill of the repo starts beside its pass; the pass itself
// runs in a follower, with the digest of this moment. A repo whose digests
// cannot be worked out has no pass: the tick says why.
func (c *core) removeOldCachesAtStart(ctx context.Context) {
	for _, repo := range c.repos() {
		if repo.Toolchain == nil || repo.Toolchain.Cache == nil {
			continue
		}
		c.mu.Lock()
		if c.toolchains == nil {
			c.toolchains = map[string]*toolchainState{}
		}
		st := c.toolchains[repo.Name]
		if st == nil {
			st = &toolchainState{}
			c.toolchains[repo.Name] = st
		}
		st.building = true
		c.mu.Unlock()
		c.followers.Add(1)
		go func() {
			defer c.followers.Done()
			defer func() {
				c.mu.Lock()
				st.building = false
				c.mu.Unlock()
			}()
			if im, reason := c.repoImages(ctx, repo); reason == "" && ctx.Err() == nil {
				c.removeOldCaches(ctx, repo, im.cache)
			}
		}()
	}
}

// buildFromMain builds an image with `git archive` of main's head as the
// context, never a card's branch, and the instance's Dockerfile added first
// under the image's Dockerfile name.
func (c *core) buildFromMain(ctx context.Context, repo settings.Repo, head string, dockerfile []byte, spec seal.Image) error {
	// The Dockerfile comes first, as one entry with the tar's end left off,
	// and then `git archive`'s stream as it is.
	var entry bytes.Buffer
	tw := tar.NewWriter(&entry)
	if err := tw.WriteHeader(&tar.Header{Name: spec.Dockerfile, Mode: 0o644, Size: int64(len(dockerfile)),
		ModTime: time.Unix(0, 0)}); err != nil {
		return err
	}
	if _, err := tw.Write(dockerfile); err != nil {
		return err
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return c.fromArchive(ctx, repo.Copy, []string{"archive", "--format=tar", head}, func(ctx context.Context, src io.Reader) error {
		return c.engine.BuildImage(ctx, spec, io.MultiReader(&entry, src))
	})
}

// repoImages reads afresh what a repo's images and its cache are built
// from: main's head, the Dockerfiles, the inputs at that head, and Kiln's
// layer, and works out the digests. It returns why they cannot be, if they
// cannot.
func (c *core) repoImages(ctx context.Context, repo settings.Repo) (repoImages, string) {
	tc := repo.Toolchain
	dockerfile, err := readDockerfile("toolchain", tc.Dockerfile)
	if err != nil {
		return repoImages{}, err.Error()
	}
	var cacheFile []byte
	if tc.Cache != nil {
		if cacheFile, err = readDockerfile("cache", tc.Cache.Dockerfile); err != nil {
			return repoImages{}, err.Error()
		}
	}
	layer, err := c.layerFiles()
	if err != nil {
		return repoImages{}, "Kiln's layer: " + err.Error()
	}
	commit, err := head(ctx, repo.Copy)
	if err != nil {
		return repoImages{}, err.Error()
	}
	paths := append([]string{toolchainDockerfile}, tc.Inputs...)
	if tc.Cache != nil {
		paths = append(append(paths, cacheDockerfile), tc.Cache.Inputs...)
	}
	for _, in := range paths {
		if strings.ContainsAny(in, "\n\r") {
			return repoImages{}, fmt.Sprintf("the input %q holds a line break", in)
		}
	}
	objects, err := objectsAt(ctx, repo.Copy, commit, paths)
	if err != nil {
		return repoImages{}, err.Error()
	}
	if objects[0] != "" {
		return repoImages{}, fmt.Sprintf("main at %s holds %s, the name the toolchain's Dockerfile takes in the build's context", commit, toolchainDockerfile)
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s%x\n", toolchainPreamble, sha256.Sum256(dockerfile))
	for i, in := range tc.Inputs {
		if objects[i+1] == "" {
			return repoImages{}, fmt.Sprintf("the toolchain's input %q is not at main's head %s", in, commit)
		}
		fmt.Fprintf(h, "%s\x00%s\n", in, objects[i+1])
	}
	toolchain := hex.EncodeToString(h.Sum(nil))
	h = sha256.New()
	fmt.Fprintf(h, "%s%s\n", runImagePreamble, toolchain)
	for _, f := range layer {
		fmt.Fprintf(h, "%s\x00%d\x00", f.name, len(f.data))
		h.Write(f.data)
	}
	im := repoImages{head: commit, dockerfile: dockerfile, layer: layer, toolchain: toolchain, run: hex.EncodeToString(h.Sum(nil))}
	if tc.Cache == nil {
		return im, ""
	}
	objects = objects[1+len(tc.Inputs):]
	if objects[0] != "" {
		return repoImages{}, fmt.Sprintf("main at %s holds %s, the name the cache's Dockerfile takes in the build's context", commit, cacheDockerfile)
	}
	h = sha256.New()
	fmt.Fprintf(h, "%s%x\n%s\n", cachePreamble, sha256.Sum256(cacheFile), tc.Cache.Path)
	for i, in := range tc.Cache.Inputs {
		if objects[i+1] == "" {
			return repoImages{}, fmt.Sprintf("the cache's input %q is not at main's head %s", in, commit)
		}
		fmt.Fprintf(h, "%s\n", objects[i+1])
	}
	fmt.Fprintf(h, "%s\n", toolchain)
	im.cacheDockerfile, im.cache = cacheFile, hex.EncodeToString(h.Sum(nil))
	return im, ""
}

// readDockerfile reads a toolchain's or a cache's Dockerfile, what, of at
// most maxToolchainDockerfile bytes.
func readDockerfile(what, name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, fmt.Errorf("the %s's Dockerfile: %w", what, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxToolchainDockerfile+1))
	if err != nil {
		return nil, fmt.Errorf("the %s's Dockerfile: %w", what, err)
	}
	if len(data) > maxToolchainDockerfile {
		return nil, fmt.Errorf("the %s's Dockerfile %s is larger than 1 MiB", what, name)
	}
	return data, nil
}

// objectLine is a line of `git cat-file --batch-check` in the format
// objectsAt asks for.
var objectLine = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64}) [a-z]+$`)

// objectsAt are the "<object id> <type>" of paths at a commit of a bare repo,
// in order, "" for a path the commit does not have. `git ls-tree` would drop
// a path that another of them is inside.
func objectsAt(ctx context.Context, repo, commit string, paths []string) ([]string, error) {
	cmd := gitCommand(ctx, repo, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	var in strings.Builder
	for _, p := range paths {
		in.WriteString(commit + ":" + p + "\n")
	}
	cmd.Stdin = strings.NewReader(in.String())
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file on %s: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(paths) {
		return nil, fmt.Errorf("git cat-file on %s gave %d lines for %d paths", repo, len(lines), len(paths))
	}
	objects := make([]string, len(paths))
	for i, line := range lines {
		switch {
		case line == commit+":"+paths[i]+" missing":
		case objectLine.MatchString(line):
			objects[i] = line
		default:
			return nil, fmt.Errorf("git cat-file on %s gave %q for %s", repo, line, paths[i])
		}
	}
	return objects, nil
}

// layer is Kiln's layer: the directory Config.Layer, or the embedded one.
func (c *core) layer() (fs.FS, error) {
	if c.Layer != "" {
		return os.DirFS(c.Layer), nil
	}
	return fs.Sub(images.RunLayer, "run")
}

// layerFiles are the files of Kiln's layer, in the order of their names.
// Anything but a directory or a regular file is refused.
func (c *core) layerFiles() ([]layerFile, error) {
	fsys, err := c.layer()
	if err != nil {
		return nil, err
	}
	var files []layerFile
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		files = append(files, layerFile{name: p, data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(files, func(f layerFile) bool { return f.name == "Dockerfile" }) {
		return nil, errors.New("it has no Dockerfile")
	}
	slices.SortFunc(files, func(a, b layerFile) int { return strings.Compare(a.name, b.name) })
	return files, nil
}

// layerTar is a tar of Kiln's layer's files, as a build's context.
func layerTar(files []layerFile) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.data)),
			ModTime: time.Unix(0, 0)}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
