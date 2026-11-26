package run

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
