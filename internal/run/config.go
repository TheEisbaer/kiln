package run

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
)

// ConfigPathspecs are the paths of the repo's Claude Code configuration, as
// Git pathspecs (ADR 0025, rule 2): every CLAUDE.md, CLAUDE.local.md and
// AGENTS.md, every .claude directory and what is in it, at any depth, and
// .mcp.json at the top. A run whose commit differs from main in any of them
// is laid over: its /work shows main's version of them, and the card's
// version is placed at /kiln/card-config.
var ConfigPathspecs = []string{
	":(top,glob)**/CLAUDE.md",
	":(top,glob)**/CLAUDE.local.md",
	":(top,glob)**/AGENTS.md",
	":(top,glob)**/.claude",
	":(top,glob)**/.claude/**",
	":(top,literal).mcp.json",
}

// cardConfigPath is where a laid-over run finds the card's configuration.
const cardConfigPath = "/kiln/card-config"

// workPath is where a run finds its work tree.
const workPath = "/work"

// kilnPath is the directory of the card's configuration in a laid-over run,
// and of everything else Kiln places there.
var kilnPath = path.Dir(cardConfigPath)

// maxLinks is how many symlinks the kernel follows in one lookup (Linux's
// limit), after which the lookup fails.
const maxLinks = 40

// errTooManyLinks is the walk's answer to a lookup that takes more than
// maxLinks symlinks.
var errTooManyLinks = errors.New("too many symlinks")

// excludes are the pathspecs as excludes: everything outside them.
func excludes(pathspecs []string) []string {
	var ex []string
	for _, p := range pathspecs {
		ex = append(ex, ":(exclude,"+strings.TrimPrefix(p, ":("))
	}
	return ex
}

// ConfigChange is one configuration path in which a commit differs from main.
type ConfigChange struct {
	// Status is A when only the commit has it, D when only main has it, and
	// M or T when both have it, differently.
	Status string
	Path   string
}

// ConfigChanges returns the configuration paths in which commit differs from
// main, both commits or trees of the repo; none when they do not differ. It
// only reads.
func ConfigChanges(ctx context.Context, repo, main, commit string) ([]ConfigChange, error) {
	out, err := git(ctx, repo, nil, nil, append([]string{"diff", "--name-status", "--no-renames", "-z",
		"--end-of-options", main, commit, "--"}, ConfigPathspecs...)...)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	var changes []ConfigChange
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("git diff --name-status printed %q", out)
	}
	for i := 0; i < len(fields); i += 2 {
		changes = append(changes, ConfigChange{Status: fields[i], Path: fields[i+1]})
	}
	return changes, nil
}

// withIndex runs f with the name of a temporary index file of its own, which
// is removed on return, so that nothing writes the copy's own index.
func withIndex(f func(env []string) error) error {
	file, err := os.CreateTemp("", "kiln-overlay-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Close(); err != nil {
		return err
	}
	return f([]string{"GIT_INDEX_FILE=" + name})
}

// configEntries are the configuration entries of a tree, as `git ls-files -s
// -z` prints them, which `update-index --index-info` takes.
func configEntries(ctx context.Context, repo, tree string) ([]byte, error) {
	var out []byte
	err := withIndex(func(env []string) error {
		if _, err := git(ctx, repo, nil, env, "read-tree", tree); err != nil {
			return err
		}
		var err error
		out, err = git(ctx, repo, nil, env, append([]string{"ls-files", "-s", "-z", "--"}, ConfigPathspecs...)...)
		return err
	})
	return out, err
}

// parseStaged reads the output of `git ls-files -s -z` as tree entries.
func parseStaged(out []byte) ([]treeEntry, error) {
	var entries []treeEntry
	for _, line := range strings.Split(string(out), "\x00") {
		if line == "" {
			continue
		}
		meta, path, ok := strings.Cut(line, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || f[2] != "0" {
			return nil, fmt.Errorf("git ls-files -s printed %q", line)
		}
		typ := "blob"
		if f[0] == "160000" {
			typ = "commit"
		}
		entries = append(entries, treeEntry{mode: f[0], typ: typ, object: f[1], path: path})
	}
	return entries, nil
}

// overlayTree writes the tree that is base with every configuration path
// removed and main's configuration entries, given as configEntries prints
// them, added, and returns its ID. It writes tree objects into the repo and
// nothing else: no ref, and no index of the repo's own.
func overlayTree(ctx context.Context, repo, base string, mainEntries []byte) (string, error) {
	var tree string
	err := withIndex(func(env []string) error {
		if _, err := git(ctx, repo, nil, env, "read-tree", base); err != nil {
			return err
		}
		paths, err := git(ctx, repo, nil, env, append([]string{"ls-files", "-z", "--"}, ConfigPathspecs...)...)
		if err != nil {
			return err
		}
		// update-index --force-remove needs a work tree; mode 0 removes the
		// entry in a bare repo as well.
		null := strings.Repeat("0", len(base))
		var removals bytes.Buffer
		for _, p := range strings.Split(string(paths), "\x00") {
			if p != "" {
				fmt.Fprintf(&removals, "0 %s\t%s\x00", null, p)
			}
		}
		if _, err := git(ctx, repo, &removals, env, "update-index", "-z", "--index-info"); err != nil {
			return err
		}
		if _, err := git(ctx, repo, bytes.NewReader(mainEntries), env, "update-index", "-z", "--index-info"); err != nil {
			return err
		}
		out, err := git(ctx, repo, nil, env, "write-tree")
		tree = strings.TrimSpace(string(out))
		return err
	})
	return tree, err
}

// layOver makes the run a laid-over one on main's tree: it builds the work
// tree, which /work shows and the baseline's HEAD holds, and the work parent
// tree, HEAD~1, when the run has a parent, and the card's configuration
// entries, which go to /kiln/card-config. It refuses a run in which a file
// of the card's stands where main has configuration under it, which the
// overlay would drop, before anything is created.
func (r *run) layOver(ctx context.Context, mainTree string) error {
	mainEntries, err := configEntries(ctx, r.Repo, mainTree)
	if err != nil {
		return err
	}
	if r.workTree, err = overlayTree(ctx, r.Repo, r.tree, mainEntries); err != nil {
		return err
	}
	lost, err := gitLine(ctx, r.Repo, append([]string{"diff", "--name-only", "-z", r.tree, r.workTree, "--", "."},
		excludes(ConfigPathspecs)...)...)
	if err != nil {
		return err
	}
	if lost != "" {
		path, _, _ := strings.Cut(lost, "\x00")
		return fmt.Errorf("the card's %s stands where main's Claude Code configuration is; the run is not started", path)
	}
	if r.parentTree != "" {
		if r.workParentTree, err = overlayTree(ctx, r.Repo, r.parentTree, mainEntries); err != nil {
			return err
		}
	}
	if r.workEntries, err = listTree(ctx, r.Repo, r.workTree); err != nil {
		return err
	}
	if err := refuseSubmodules(r.workEntries); err != nil {
		return err
	}
	own, err := configEntries(ctx, r.Repo, r.tree)
	if err != nil {
		return err
	}
	if r.cardConfig, err = parseStaged(own); err != nil {
		return err
	}
	r.mainTree = mainTree
	return nil
}

// mainTreeOf is the tree of main for a run that starts: main's tree when the
// commit differs from main in a configuration path, else "".
func (r *run) mainTreeOf(ctx context.Context) (string, error) {
	if r.Main == "" {
		return "", nil
	}
	main, err := gitLine(ctx, r.Repo, "rev-parse", "--verify", "--end-of-options", r.Main+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("the main commit %q: %w", r.Main, err)
	}
	changes, err := ConfigChanges(ctx, r.Repo, main, r.commit)
	if err != nil || len(changes) == 0 {
		return "", err
	}
	return gitLine(ctx, r.Repo, "rev-parse", "--verify", main+"^{tree}")
}

// reachesKiln tells whether the absolute path p of a run, followed the way
// the run's kernel follows it, reaches /kiln. links maps each symlink of the
// work tree, by its path under /work, to its target as Git stores it; every
// other path counts as a plain directory. The walk goes component by
// component from /, follows a link before what comes after it, and so takes
// .. after the link is resolved. It reports true as soon as it stands at or
// under /kiln, and when it ends at /, from where /kiln is one step away. After
// maxLinks symlinks it returns errTooManyLinks.
func reachesKiln(links map[string]string, p string) (bool, error) {
	rest := strings.Split(p, "/")
	at, hops := "/", 0
	for len(rest) > 0 {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			at = path.Dir(at) // the parent of / is /
		default:
			next := path.Join(at, c)
			if rel, ok := strings.CutPrefix(next, workPath+"/"); ok {
				if target, ok := links[rel]; ok {
					if hops++; hops > maxLinks {
						return false, errTooManyLinks
					}
					// An absolute target starts again from /; a relative one
					// goes on from the link's directory, which at is.
					if strings.HasPrefix(target, "/") {
						at = "/"
					}
					rest = append(strings.Split(target, "/"), rest...)
					continue
				}
			}
			at = next
		}
		if at == kilnPath || strings.HasPrefix(at, kilnPath+"/") {
			return true, nil
		}
	}
	return at == "/", nil
}

// linkTargets reads the targets of the symlinks among entries, by their path,
// in one `git cat-file --batch`. It only reads.
func linkTargets(ctx context.Context, repo string, entries []treeEntry) (map[string]string, error) {
	var objects bytes.Buffer
	var links []treeEntry
	for _, e := range entries {
		if e.mode == "120000" {
			links = append(links, e)
			fmt.Fprintln(&objects, e.object)
		}
	}
	targets := map[string]string{}
	if len(links) == 0 {
		return targets, nil
	}
	out, err := git(ctx, repo, &objects, nil, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(bytes.NewReader(out))
	for _, e := range links {
		data, err := readObject(r, e.object)
		if err != nil {
			return nil, err
		}
		targets[e.path] = string(data)
	}
	return targets, nil
}

// refuseLinksIntoKiln refuses a laid-over run in which a symlink of the work
// tree, followed as the run's kernel follows it, reaches /kiln, where the
// card's own configuration is (ADR 0025, rule 2): Claude Code would load the
// card's CLAUDE.md and skills through it. It names the first such link in
// the tree's order. A run that is not laid over is not checked: it has no
// card configuration under /kiln. It only reads.
func (r *run) refuseLinksIntoKiln(ctx context.Context) error {
	if r.mainTree == "" {
		return nil
	}
	links, err := linkTargets(ctx, r.Repo, r.workEntries)
	if err != nil {
		return err
	}
	for _, e := range r.workEntries {
		target, ok := links[e.path]
		if !ok {
			continue
		}
		reaches, err := reachesKiln(links, path.Join(workPath, e.path))
		if errors.Is(err, errTooManyLinks) {
			return fmt.Errorf("the symlink %s -> %s is not followed to its end within %d symlinks; the run is not started", e.path, target, maxLinks)
		}
		if err != nil {
			return err
		}
		if reaches {
			return fmt.Errorf("the symlink %s -> %s leads into %s, where a laid-over run has the card's own Claude Code configuration; the run is not started", e.path, target, kilnPath)
		}
	}
	return nil
}
