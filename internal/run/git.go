package run

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// git runs the git command on a repo with the given standard input, which may
// be nil, and extra environment, and returns its standard output. Variables of
// the caller's that start with GIT_ are left out, so that none of them points
// git at another repo or index. A failure carries what git printed to standard
// error.
func git(ctx context.Context, repo string, stdin io.Reader, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + repo}, args...)...)
	cmd.Env = append(gitEnv(), env...)
	cmd.Stdin = stdin
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// gitLine is git with its output trimmed of the final newline.
func gitLine(ctx context.Context, repo string, args ...string) (string, error) {
	out, err := git(ctx, repo, nil, nil, args...)
	return strings.TrimRight(string(out), "\n"), err
}

// gitEnv is the caller's environment without the variables that start with GIT_.
func gitEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			env = append(env, e)
		}
	}
	return env
}

// treeEntry is one line of `git ls-tree -r`.
type treeEntry struct {
	mode, typ, object, path string
}

// listTree lists every file, symlink and submodule of a commit.
func listTree(ctx context.Context, repo, commit string) ([]treeEntry, error) {
	out, err := git(ctx, repo, nil, nil, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	var entries []treeEntry
	for _, line := range strings.Split(string(out), "\x00") {
		if line == "" {
			continue
		}
		meta, path, ok := strings.Cut(line, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("git ls-tree printed %q", line)
		}
		entries = append(entries, treeEntry{mode: f[0], typ: f[1], object: f[2], path: path})
	}
	return entries, nil
}

// refuseSubmodules refuses a commit that contains a submodule.
func refuseSubmodules(entries []treeEntry) error {
	for _, e := range entries {
		if e.mode == "160000" {
			return fmt.Errorf("the commit contains the submodule %s; submodules are not placed in a run", e.path)
		}
	}
	return nil
}

// placingTar writes a tar of a commit's files with relative paths, from `git
// ls-tree` and `git cat-file --batch`, never from `git archive`, which would
// apply export-ignore and other attributes. Mode 100755 becomes 0755, 100644
// becomes 0644, and 120000 becomes a symlink; anything else is refused. The
// seal decides the owner and where the files go (seal.PlaceRun).
func placingTar(ctx context.Context, repo string, entries []treeEntry) ([]byte, error) {
	if err := refuseSubmodules(entries); err != nil {
		return nil, err
	}
	modes := map[string]int64{"100644": 0o644, "100755": 0o755, "120000": 0o777}
	for _, e := range entries {
		if _, ok := modes[e.mode]; !ok || e.typ != "blob" {
			return nil, fmt.Errorf("the commit has %s with the mode %s, which is not placed", e.path, e.mode)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+repo, "cat-file", "--batch")
	cmd.Env = gitEnv()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		// Written while the objects are read, so that neither pipe fills up.
		w := bufio.NewWriter(stdin)
		for _, e := range entries {
			fmt.Fprintln(w, e.object)
		}
		w.Flush()
		stdin.Close()
	}()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	r := bufio.NewReader(stdout)
	readErr := func() error {
		for _, e := range entries {
			data, err := readObject(r, e.object)
			if err != nil {
				return err
			}
			h := &tar.Header{Name: e.path, Mode: modes[e.mode], ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
			if e.mode == "120000" {
				h.Typeflag, h.Linkname, data = tar.TypeSymlink, string(data), nil
			} else {
				h.Typeflag, h.Size = tar.TypeReg, int64(len(data))
			}
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if _, err := tw.Write(data); err != nil {
				return err
			}
		}
		return tw.Close()
	}()
	if readErr != nil {
		cancel()
	}
	if err := cmd.Wait(); err != nil && readErr == nil {
		readErr = fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if readErr != nil {
		return nil, readErr
	}
	return buf.Bytes(), nil
}

// readObject reads one object of `git cat-file --batch`.
func readObject(r *bufio.Reader, object string) ([]byte, error) {
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("git cat-file for %s: %w", object, err)
	}
	f := strings.Fields(header)
	if len(f) != 3 || f[0] != object || f[1] != "blob" {
		return nil, fmt.Errorf("git cat-file for %s printed %q", object, strings.TrimSpace(header))
	}
	size, err := strconv.Atoi(f[2])
	if err != nil {
		return nil, fmt.Errorf("git cat-file for %s printed %q", object, strings.TrimSpace(header))
	}
	data := make([]byte, size+1) // and the newline after it
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("git cat-file for %s: %w", object, err)
	}
	return data[:size], nil
}

// basePack is a pack of a tree's objects, as `git rev-list --objects <tree> |
// git pack-objects --stdout` makes it: what the save container starts from.
func basePack(ctx context.Context, repo, tree string) ([]byte, error) {
	list, err := git(ctx, repo, nil, nil, "rev-list", "--objects", tree)
	if err != nil {
		return nil, err
	}
	return git(ctx, repo, bytes.NewReader(list), nil, "pack-objects", "--quiet", "--stdout")
}

// takeIn adds the objects of a pack that SaveRun returned to the repo, checked
// with --strict, and makes sure that the new ID is a tree there.
func takeIn(ctx context.Context, repo, tree string, pack []byte) error {
	if _, err := git(ctx, repo, bytes.NewReader(pack), nil, "index-pack", "--stdin", "--strict"); err != nil {
		return err
	}
	typ, err := gitLine(ctx, repo, "cat-file", "-t", tree)
	if err != nil {
		return err
	}
	if typ != "tree" {
		return fmt.Errorf("the save gave %s, which is a %s, not a tree", tree, typ)
	}
	return nil
}

// The save's commit, by Kiln.
const (
	saveName  = "Kiln"
	saveEmail = "kiln@localhost"
	trailer   = "Kiln-Run"
)

// saveMessage is the message of a run's save.
func saveMessage(id string) string {
	return "chore: save run " + id + "\n\n" + trailer + ": " + id + "\n"
}

// commitSave makes the commit of a run's save: the tree on top of the parent,
// by Kiln, with both dates the given time, so that the same save made again is
// the same commit.
func commitSave(ctx context.Context, repo, tree, parent, id string, date time.Time) (string, error) {
	when := strconv.FormatInt(date.Unix(), 10) + " +0000"
	out, err := git(ctx, repo, strings.NewReader(saveMessage(id)), []string{
		"GIT_AUTHOR_NAME=" + saveName, "GIT_AUTHOR_EMAIL=" + saveEmail, "GIT_AUTHOR_DATE=" + when,
		"GIT_COMMITTER_NAME=" + saveName, "GIT_COMMITTER_EMAIL=" + saveEmail, "GIT_COMMITTER_DATE=" + when,
	}, "commit-tree", "--no-gpg-sign", "-p", parent, tree)
	return strings.TrimSpace(string(out)), err
}

// branchAt returns the commit a branch is at, or "" when it does not exist.
func branchAt(ctx context.Context, repo, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+repo, "rev-parse", "--verify", "--quiet", ref)
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0 {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// moveBranch sets a branch to a commit, guarded by the value it must have now:
// "" for a branch that must not exist. Git refuses the update when the branch
// has another value.
func moveBranch(ctx context.Context, repo, ref, commit, old string) error {
	_, err := git(ctx, repo, nil, nil, "update-ref", ref, commit, old)
	return err
}

// isSaveOf reports whether a commit is the save of a run on top of a parent:
// that parent alone, and the trailer Kiln-Run: <id>.
func isSaveOf(ctx context.Context, repo, commit, parent, id string) (bool, error) {
	out, err := gitLine(ctx, repo, "cat-file", "commit", commit)
	if err != nil {
		return false, err
	}
	headers, message, _ := strings.Cut(out, "\n\n")
	var parents []string
	for _, line := range strings.Split(headers, "\n") {
		if p, ok := strings.CutPrefix(line, "parent "); ok {
			parents = append(parents, p)
		}
	}
	if len(parents) != 1 || parents[0] != parent {
		return false, nil
	}
	for _, line := range strings.Split(message, "\n") {
		if line == trailer+": "+id {
			return true, nil
		}
	}
	return false, nil
}
