package tick

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/seal"
	"github.com/TheEisbaer/kiln/internal/store"
)

// The update of the core (ADR 0019, ADR 0036, ADR 0037). Once main of Kiln's
// own repo has moved past the running version, a core under a launcher builds
// the images of main's head under its tag, <name>:<first 12 hex digits>, the
// toolchain always and the others when their files changed, and the program
// of main's head in that toolchain into <data>/programs/; it stops acting,
// copies the store beside its own version, asks the launcher to replace it
// and exits; the runs go on in their containers. The new core makes its tagged
// images the plain names once it has reported. After a failed update the launcher
// starts the previous program with KILN_UPDATE_FAILED, and that core puts its
// copy back before it opens the store.
//
// The directory <data>/programs/ holds only these names, each written as
// <name>.new and renamed once whole: kiln-<commit>, a program the core built,
// and kiln-<version>.db, or kiln-by-hand.db, the store's copy that the core
// of that version made just before it asked to be replaced. Nothing else in
// it is written or removed.

// The variables by which the launcher tells a falling-back core of the failed
// update: the reason, and the program that failed. They are the launcher's
// own (internal/launcher, whose constants are unexported); the test through
// the launcher keeps the two in step.
const (
	envUpdateFailed        = "KILN_UPDATE_FAILED"
	envUpdateFailedProgram = "KILN_UPDATE_FAILED_PROGRAM"
)

// byHand names the store's copy of a core built by hand, which has no version.
const byHand = "by-hand"

// imageFiles are the files the four images are built from: their Dockerfiles,
// and the contexts of the toolchain, proxy and way-out images.
var imageFiles = []string{"images", "cmd/proxy", "cmd/wayout", "go.mod", "go.sum"}

// oldName is a name in <data>/programs/ that the removal may remove: a program
// or a copy, whole or being written.
var oldName = regexp.MustCompile(`^kiln-(?:(?:[0-9a-f]{40}|[0-9a-f]{64})(?:\.new|\.db|\.db\.new)?|by-hand\.db(?:\.new)?)$`)

// failedUpdate is what a falling-back core is told by the launcher: the
// reason, empty for a core that does not fall back, and the program that
// failed.
type failedUpdate struct {
	reason, program string
}

// takeFailedUpdate reads the launcher's variables of a failed update and
// unsets both, so that no child of the core sees them.
func takeFailedUpdate() (failedUpdate, error) {
	f := failedUpdate{reason: os.Getenv(envUpdateFailed), program: os.Getenv(envUpdateFailedProgram)}
	for _, name := range []string{envUpdateFailed, envUpdateFailedProgram} {
		if err := os.Unsetenv(name); err != nil {
			return failedUpdate{}, err
		}
	}
	return f, nil
}

// commit is the commit of the program that failed, from its base name
// kiln-<commit>, or "".
func (f failedUpdate) commit() string {
	c, ok := strings.CutPrefix(filepath.Base(f.program), "kiln-")
	if !ok || !objectID.MatchString(c) {
		return ""
	}
	return c
}

// turnMergingOff turns automatic merging off after a failed update, with a
// reason that names the failed update, why it failed and the version the core
// of version went back to, and the failed commit, which the core does not
// update to while merging is off.
func turnMergingOff(s *store.Store, f failedUpdate, version string) error {
	what := f.commit()
	if what == "" {
		what = f.program
	}
	back := versionOf(version)
	if back == "" {
		back = "a core built by hand"
	}
	_, err := s.TurnMergingOff(run.Printable("the update to "+what+" failed: "+f.reason+"; the core went back to "+back), f.commit())
	return err
}

// objectID is a commit's full hash, SHA-1 or SHA-256.
var objectID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// updateState is the update as the core keeps it, in memory only.
type updateState struct {
	launched bool        // the core has a launcher
	merged   atomic.Bool // a card of this instance was closed as merged since the core started
	seen     string      // main's head at the last look, read by the tick only

	mu       sync.Mutex
	building bool            // a build runs
	broken   map[string]bool // the commits whose build failed in this core's life
	said     map[string]bool // the lines printed once per core
	built    chan string     // the program of a finished build, for the main loop
}

// sayOnce prints a line once in the core's life.
func (c *core) sayOnce(w io.Writer, line string) { c.sayOnceAs(w, line, line) }

// sayOnceAs prints a line once in the core's life for each key.
func (c *core) sayOnceAs(w io.Writer, key, line string) {
	c.update.mu.Lock()
	defer c.update.mu.Unlock()
	if c.update.said[key] {
		return
	}
	if c.update.said == nil {
		c.update.said = map[string]bool{}
	}
	c.update.said[key] = true
	fmt.Fprintln(w, line)
}

// programs is the directory of the programs and the store's copies.
func (c *core) programs() string { return filepath.Join(c.Data, "programs") }

// versionName is the name a core's copy of the store goes by: its version,
// or by-hand.
func versionName(version string) string {
	if v := versionOf(version); v != "" {
		return v
	}
	return byHand
}

// checkUpdate is the update's look at main: after a fetch round of main
// (moved), and, in a core without a launcher, which fetches main only for
// its cards, whenever main's head has moved since the last look. When an
// update is due it starts the build beside the tick, unless one runs. An
// error is printed once per text.
func (c *core) checkUpdate(ctx context.Context, moved bool) {
	if !moved && c.update.launched {
		return
	}
	target, err := head(ctx, c.own().Copy)
	if err == nil && !moved && target == c.update.seen {
		return
	}
	if err == nil {
		c.update.seen = target
		target, err = c.updateDue(ctx, target)
	}
	if err != nil {
		if ctx.Err() == nil {
			c.sayOnce(c.errOut, "kiln core: the update: "+run.Printable(err.Error()))
		}
		return
	}
	if target != "" {
		c.startBuild(ctx, target)
	}
}

// updateDue is the commit the core updates to, or "" when no update is due:
// the repo is Kiln's own; main's head is past the running version, or, for a
// core built by hand, one of the instance's cards merged since it started;
// the core has a launcher; and the head is neither the commit of a failed
// update while automatic merging is off, which the store keeps across
// restarts, nor one whose build failed in this core's life.
func (c *core) updateDue(ctx context.Context, target string) (string, error) {
	own, err := kilnsOwn(ctx, c.own().Copy, target)
	if err != nil || !own {
		return "", err
	}
	if v := versionOf(c.Version); v != "" {
		if target == v {
			return "", nil
		}
		past, err := isAncestor(ctx, c.own().Copy, v, target)
		if err != nil {
			return "", err
		}
		if !past {
			return "", nil
		}
	} else if !c.update.merged.Load() {
		return "", nil
	}
	if !c.update.launched {
		c.sayOnceAs(c.out, "no launcher", "kiln core: main is at "+target+", past this core's version; a core without a launcher does not update itself")
		return "", nil
	}
	merging, err := c.store.Merging()
	if err != nil {
		return "", err
	}
	if merging.Failed != "" && merging.Failed == target {
		c.sayOnce(c.out, "kiln core: "+target+" failed as an update; this core waits for main to move past it")
		return "", nil
	}
	c.update.mu.Lock()
	defer c.update.mu.Unlock()
	if c.update.broken[target] {
		return "", nil
	}
	return target, nil
}

// kilnsOwn reports whether a commit's go.mod names the module of the core's
// own build information: the repo is Kiln's own. A commit without go.mod, or
// one whose go.mod has no module line, is not.
func kilnsOwn(ctx context.Context, repo, commit string) (bool, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Path == "" {
		return false, nil
	}
	cmd := gitCommand(ctx, repo, "cat-file", "blob", commit+":go.mod")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil // no go.mod at the commit
	}
	if err != nil {
		return false, err
	}
	return modulePath(string(out)) == info.Main.Path, nil
}

// modulePath is the module path a go.mod names: the first line whose first
// word is module, without a // comment and without quotes; "" without one.
func modulePath(gomod string) string {
	sc := bufio.NewScanner(strings.NewReader(gomod))
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "//")
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "module" {
			continue
		}
		if len(f) != 2 {
			return ""
		}
		if p, err := strconv.Unquote(f[1]); err == nil {
			return p
		}
		return f[1]
	}
	return ""
}

// isAncestor reports whether a commit is an ancestor of another. A commit that
// is not in the repo is an error.
func isAncestor(ctx context.Context, repo, ancestor, commit string) (bool, error) {
	cmd := gitCommand(ctx, repo, "merge-base", "--is-ancestor", ancestor, commit)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	case errors.As(err, &exit) && exit.ExitCode() == 128:
		return false, fmt.Errorf("the running version %s is not in the copy %s, so the core cannot tell whether main is past it", ancestor, repo)
	}
	return false, fmt.Errorf("git merge-base on %s: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
}

// startBuild starts the build of a commit's images and program beside the
// tick, counted among the followers, unless a build runs or a program waits
// for the main loop. A finished build hands its program to the main loop; a failed one is
// printed, and the commit is not built again in this core's life.
func (c *core) startBuild(ctx context.Context, target string) {
	c.update.mu.Lock()
	if c.update.building || len(c.update.built) > 0 {
		c.update.mu.Unlock()
		return
	}
	c.update.building = true
	c.update.mu.Unlock()
	from := "this core, built by hand"
	if v := versionOf(c.Version); v != "" {
		from = v
	}
	fmt.Fprintf(c.out, "kiln core: main is at %s, past %s; building the core of %s\n", target, from, target)
	c.followers.Add(1)
	go func() {
		defer c.followers.Done()
		all, _ := c.imagesDue(ctx, target)
		toolchain, err := c.buildImages(ctx, target, all)
		var program string
		if err == nil {
			program, err = c.build(ctx, target, toolchain)
		}
		c.update.mu.Lock()
		defer c.update.mu.Unlock()
		c.update.building = false
		if err != nil {
			if ctx.Err() == nil {
				if c.update.broken == nil {
					c.update.broken = map[string]bool{}
				}
				c.update.broken[target] = true
				fmt.Fprintf(c.errOut, "kiln core: the core of %s was not built: %s; this core goes on as %s\n",
					target, run.Printable(err.Error()), from)
			}
			return
		}
		c.update.built <- program
	}()
}

// tagOf is the tag of a version's images: the first 12 hex digits of its
// commit.
func tagOf(commit string) string { return commit[:12] }

// versionTag is a tag of the images of a version.
var versionTag = regexp.MustCompile(`^[0-9a-f]{12}$`)

// kilnImage is one of the images the update builds, as the README builds it:
// its context is `git archive` of the commit, or of a directory of it, with
// only some paths, or all of them; its Dockerfile is a path in that context.
type kilnImage struct {
	name        string
	dir         string   // the directory of the commit that is the context, "" for its root
	paths       []string // the paths in the context, none for all of them
	dockerfile  string
	noNetwork   bool // the image only compiles, from the toolchain image's module cache
	onToolchain bool // the image is built FROM the new toolchain image
}

// images are the images the update builds, in the order it builds them: the
// toolchain first, which the others are built on.
func (c *core) images() []kilnImage {
	return []kilnImage{
		{name: c.ToolchainImage, paths: []string{"go.mod", "go.sum", "images/toolchain/Dockerfile"}, dockerfile: "images/toolchain/Dockerfile"},
		{name: c.ProxyImage, dockerfile: "images/proxy/Dockerfile", noNetwork: true, onToolchain: true},
		{name: c.WayOutImage, dockerfile: "images/wayout/Dockerfile", noNetwork: true, onToolchain: true},
		{name: c.RunImage, dir: "images/run", dockerfile: "Dockerfile", onToolchain: true},
	}
}

// imageNames are the names of the images, in that order, without an empty
// one.
func (c *core) imageNames() []string {
	var names []string
	for _, im := range c.images() {
		if im.name != "" {
			names = append(names, im.name)
		}
	}
	return names
}

// imagesDue reports whether the update builds every image, and not only the
// toolchain image, and prints why: when the files of the images changed
// between the running version and the commit, which it returns, and always
// for a core built by hand, which cannot tell, or when the diff fails.
func (c *core) imagesDue(ctx context.Context, target string) (bool, string) {
	as := "building the images as <name>:" + tagOf(target)
	v := versionOf(c.Version)
	if v == "" {
		fmt.Fprintln(c.out, "kiln core: this core was built by hand, so it cannot tell whether the images' files changed; "+as)
		return true, ""
	}
	files, err := gitLine(ctx, c.own().Copy, append([]string{"diff", "--name-only", "--no-renames", v, target, "--"}, imageFiles...)...)
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(c.errOut, "kiln core: the images' files: %s; %s\n", run.Printable(err.Error()), as)
		}
		return true, ""
	}
	if files == "" {
		return false, ""
	}
	fmt.Fprintf(c.out, "kiln core: the images' files changed between %s and %s (%s); %s\n",
		v, target, run.Printable(fileNames(strings.Split(files, "\n"))), as)
	return true, files
}

// buildImages builds the images of a commit, each under the commit's tag and
// one after the other: the toolchain image always, and the others when all is
// set. It returns the toolchain image the program is built in. A failed build
// is returned, and the images already built keep their tags.
func (c *core) buildImages(ctx context.Context, target string, all bool) (string, error) {
	tag := tagOf(target)
	toolchain := c.ToolchainImage + ":" + tag
	for i, im := range c.images() {
		if i > 0 && !all {
			break
		}
		spec := seal.Image{Name: im.name, Tag: tag, Dockerfile: im.dockerfile, NoNetwork: im.noNetwork}
		if im.onToolchain {
			spec.Toolchain = toolchain
		}
		treeish := target
		if im.dir != "" {
			treeish += ":" + im.dir
		}
		err := c.fromArchive(ctx, c.own().Copy, append([]string{"archive", "--format=tar", treeish}, im.paths...),
			func(ctx context.Context, src io.Reader) error { return c.engine.BuildImage(ctx, spec, src) })
		if err != nil {
			return "", fmt.Errorf("the image %s:%s: %w", im.name, tag, err)
		}
	}
	return toolchain, nil
}

// build builds a commit's program to <data>/programs/kiln-<commit>, unless it
// is there: from `git archive` of the commit, through the engine's build in
// the toolchain image, into kiln-<commit>.new, which is synced and renamed
// once it is whole.
func (c *core) build(ctx context.Context, target, toolchain string) (string, error) {
	dir := c.programs()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	program := filepath.Join(dir, "kiln-"+target)
	there, err := exists(program)
	if err != nil || there {
		return program, err
	}
	partial := program + ".new"
	if err := removeIfThere(partial); err != nil {
		return "", err
	}
	if err := c.buildInto(ctx, target, toolchain, partial); err != nil {
		os.Remove(partial)
		return "", err
	}
	at("update-built-new")
	if err := os.Rename(partial, program); err != nil {
		os.Remove(partial)
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	at("update-built")
	return program, nil
}

// fromArchive streams `git archive` with the arguments, on a repo's copy, into
// consume, on a context that ends with it. A consumer that did not read the whole archive
// lets git go; its error carries git's, if git said one.
func (c *core) fromArchive(ctx context.Context, repo string, args []string, consume func(context.Context, io.Reader) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	archive := gitCommand(ctx, repo, args...)
	var stderr strings.Builder
	archive.Stderr = &stderr
	src, err := archive.StdoutPipe()
	if err != nil {
		return err
	}
	if err := archive.Start(); err != nil {
		return err
	}
	consumeErr := consume(ctx, src)
	src.Close()
	if consumeErr != nil {
		cancel()
		archive.Wait()
		if said := strings.TrimSpace(stderr.String()); said != "" {
			return fmt.Errorf("%w; git archive: %s", consumeErr, said)
		}
		return consumeErr
	}
	if err := archive.Wait(); err != nil {
		return fmt.Errorf("git archive on %s: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// buildInto streams `git archive` of a commit into the engine's build in the
// toolchain image, and the program into a new file at path, synced and
// closed. An empty program is refused.
func (c *core) buildInto(ctx context.Context, target, toolchain, path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o700); err != nil {
		return err
	}
	err = c.fromArchive(ctx, c.own().Copy, []string{"archive", "--format=tar", target}, func(ctx context.Context, src io.Reader) error {
		return c.engine.Build(ctx, seal.Program{Instance: c.instance, Image: toolchain, Version: target,
			GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}, src, f)
	})
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		return errors.New("the build wrote an empty program")
	}
	return f.Close()
}

// promote makes the images of the running version's tag the plain names, in
// the new core after its report and before it keeps the proxy, so that after
// a failed update the previous core finds the plain names as they were. For
// each image whose tag is there: the plain name is tagged to the tag's image
// unless it is that image already, and the tag is removed. A kill between
// the two is finished by the next start. A core built by hand promotes
// nothing. A step that fails is returned.
func (c *core) promote(ctx context.Context) error {
	v := versionOf(c.Version)
	if v == "" {
		return nil
	}
	for _, name := range c.imageNames() {
		tagged := name + ":" + tagOf(v)
		id, err := c.engine.ImageID(ctx, tagged)
		if err != nil {
			return fmt.Errorf("the image %s is not made %s: %w", tagged, name, err)
		}
		if id == "" {
			continue
		}
		plain, err := c.engine.ImageID(ctx, name)
		if err != nil {
			return fmt.Errorf("the image %s is not made %s: %w", tagged, name, err)
		}
		if plain != id {
			if err := c.engine.TagImage(ctx, tagged, name); err != nil {
				return fmt.Errorf("the image %s is not made %s: %w", tagged, name, err)
			}
		}
		at("promote-tagged")
		if err := c.engine.RemoveImageTag(ctx, tagged); err != nil {
			return fmt.Errorf("the tag %s is not removed: %w", tagged, err)
		}
		fmt.Fprintf(c.out, "kiln core: the image %s is now %s\n", tagged, name)
	}
	return nil
}

// removeOldTags removes, by exact reference, every tag of 12 hex digits of
// the images but the running version's and the new version's. Every other
// tag and name, and every untagged image, is left alone. A removal that fails
// is printed.
func (c *core) removeOldTags(ctx context.Context, target string) {
	keep := []string{tagOf(target)}
	if v := versionOf(c.Version); v != "" {
		keep = append(keep, tagOf(v))
	}
	for _, name := range c.imageNames() {
		tags, err := c.engine.ImageTags(ctx, name)
		if err != nil {
			fmt.Fprintf(c.errOut, "kiln core: the old tags of %s are not removed: %s\n", name, run.Printable(err.Error()))
			continue
		}
		for _, tag := range tags {
			if !versionTag.MatchString(tag) || slices.Contains(keep, tag) {
				continue
			}
			if err := c.engine.RemoveImageTag(ctx, name+":"+tag); err != nil {
				fmt.Fprintln(c.errOut, "kiln core: an old image's tag is not removed:", run.Printable(err.Error()))
			}
		}
	}
}

// handOver is the hand-over to a built program, in the main loop: the core
// stops acting and waits for every follower, as on Ctrl-C, and stops the
// screen; then it copies the store beside its own version and closes it, so
// that nothing is written after the copy; it removes the programs and copies
// beyond one step back, and the images' tags beyond one step back under ctx,
// the core's context, asks the launcher to replace it by the program, and
// returns. A failed copy or request is returned, and the launcher starts the
// core again, which repeats the update.
func (c *core) handOver(ctx context.Context, stopActing, stopScreen func(), l reporter, program string) error {
	c.acting.Store(false)
	stopActing()
	c.followers.Wait()
	at("update-stopped")
	if stopScreen != nil {
		stopScreen()
	}
	dir := c.programs()
	copied := filepath.Join(dir, "kiln-"+versionName(c.Version)+".db")
	if err := removeIfThere(copied + ".new"); err != nil {
		c.store.Close()
		return err
	}
	if err := c.store.CopyAndClose(copied + ".new"); err != nil {
		return fmt.Errorf("the store's copy: %w", err)
	}
	at("update-copy-new")
	if err := os.Rename(copied+".new", copied); err != nil {
		return fmt.Errorf("the store's copy: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("the store's copy: %w", err)
	}
	at("update-copied")
	c.removeOld(dir, filepath.Base(program), filepath.Base(copied))
	c.removeOldTags(ctx, strings.TrimPrefix(filepath.Base(program), "kiln-"))
	at("update-removed")
	if err := l.AskReplace(program); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "kiln core: asked to be replaced by %s; the core ends, and the runs go on\n", program)
	return nil
}

// removeOld removes, by exact name, every program and copy in the directory
// but the running core's program, its copy and the new program. Directories
// and every other name are left alone. A removal that fails is printed.
func (c *core) removeOld(dir string, keep ...string) {
	if v := versionOf(c.Version); v != "" {
		keep = append(keep, "kiln-"+v)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintln(c.errOut, "kiln core: the old programs are not removed:", err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !oldName.MatchString(name) || slices.Contains(keep, name) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(c.errOut, "kiln core: an old program is not removed:", err)
		}
	}
}

// restore puts the store's copy of this core's version back after a failed
// update, under the data directory's lock and before the store is opened:
// the copy is copied to store.db.restore and synced, the store's journal is
// removed, so that a hot journal of the newer core's is not rolled back onto
// the restored file, and the restore is renamed over store.db. The copy stays,
// so every step can be repeated. Without a copy the store is opened as it is.
func restore(out io.Writer, data, version string) error {
	copied := filepath.Join(data, "programs", "kiln-"+version+".db")
	src, err := os.Open(copied)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(out, "kiln core: there is no copy of the store at %s; the store is opened as it is\n", copied)
		return nil
	}
	if err != nil {
		return err
	}
	defer src.Close()
	restored := filepath.Join(data, "store.db.restore")
	if err := removeIfThere(restored); err != nil {
		return err
	}
	dst, err := os.OpenFile(restored, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if err == nil {
		err = dst.Sync()
	}
	if err = errors.Join(err, dst.Close()); err != nil {
		return fmt.Errorf("the store's copy %s: %w", copied, err)
	}
	at("restore-new")
	if err := removeIfThere(filepath.Join(data, "store.db-journal")); err != nil {
		return err
	}
	at("restore-journal")
	if err := os.Rename(restored, filepath.Join(data, "store.db")); err != nil {
		return err
	}
	if err := syncDir(data); err != nil {
		return err
	}
	fmt.Fprintf(out, "kiln core: the store is put back from %s\n", copied)
	at("restore-done")
	return nil
}

// removeIfThere removes a file, and is content when it is not there.
func removeIfThere(name string) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// syncDir syncs a directory, so that a rename in it is on the disk. A system
// that cannot sync a directory is let be.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
