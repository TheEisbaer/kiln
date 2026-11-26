package run

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// configRepo makes a bare copy with main at a commit of mainFiles and the
// card's branch at a commit on top of it, where cardFiles are written, and
// removed where their data is "". It returns the copy and both commits.
func configRepo(t *testing.T, mainFiles, cardFiles map[string]string) (repo, main, card string) {
	t.Helper()
	return linkRepo(t, mainFiles, cardFiles, nil)
}

// linkRepo is configRepo with the symlinks cardLinks, by path and target,
// added in the card's commit.
func linkRepo(t *testing.T, mainFiles, cardFiles, cardLinks map[string]string) (repo, main, card string) {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	gitIn(t, dir, "init", "--quiet", "--initial-branch=main", work)
	for name, data := range mainFiles {
		writeIn(t, work, name, data, 0o644)
	}
	gitIn(t, work, "add", "--all", "--force")
	gitIn(t, work, "commit", "--quiet", "--message", "main")
	gitIn(t, work, "checkout", "--quiet", "-b", "kiln/card-1")
	for name, data := range cardFiles {
		if data == "" {
			gitIn(t, work, "rm", "--quiet", "-r", name)
			continue
		}
		if err := os.RemoveAll(filepath.Join(work, name)); err != nil {
			t.Fatal(err)
		}
		writeIn(t, work, name, data, 0o644)
	}
	for name, target := range cardLinks {
		link := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, work, "add", "--all", "--force")
	gitIn(t, work, "commit", "--quiet", "--allow-empty", "--message", "card")
	repo = filepath.Join(dir, "copy.git")
	gitIn(t, dir, "clone", "--quiet", "--bare", work, repo)
	return repo, gitIn(t, repo, "rev-parse", "main"), gitIn(t, repo, "rev-parse", "kiln/card-1")
}

// configMain is main of the tests: configuration at the top, nested, and
// files that only look like it.
var configMain = map[string]string{
	"CLAUDE.md":                        "main\n",
	"pkg/AGENTS.md":                    "main\n",
	".claude/settings.json":            "{}\n",
	".claude/hooks/check.sh":           "main\n",
	".claude/hooks/gone.sh":            "main\n",
	"nested/.claude/skills/n/SKILL.md": "main\n",
	".mcp.json":                        "{}\n",
	"docs/claude.md":                   "main\n",
	"pkg/.mcp.json":                    "main\n",
	"code.go":                          "main\n",
}

// configCard is the card's change of it: it changes, adds and removes
// configuration, and changes files outside it.
var configCard = map[string]string{
	"CLAUDE.md":                        "card\n",
	"pkg/AGENTS.md":                    "card\n",
	".claude/hooks/check.sh":           "card\n",
	".claude/hooks/gone.sh":            "",
	".claude/skills/new/SKILL.md":      "card\n",
	"nested/.claude/skills/n/SKILL.md": "card\n",
	"sub/CLAUDE.local.md":              "card\n",
	"docs/claude.md":                   "card\n",
	"pkg/.mcp.json":                    "card\n",
	"code.go":                          "card\n",
}

func TestConfigChanges(t *testing.T) {
	repo, main, card := configRepo(t, configMain, configCard)
	got, err := ConfigChanges(t.Context(), repo, main, card)
	if err != nil {
		t.Fatal(err)
	}
	want := []ConfigChange{
		{"M", ".claude/hooks/check.sh"},
		{"D", ".claude/hooks/gone.sh"},
		{"A", ".claude/skills/new/SKILL.md"},
		{"M", "CLAUDE.md"},
		{"M", "nested/.claude/skills/n/SKILL.md"},
		{"M", "pkg/AGENTS.md"},
		{"A", "sub/CLAUDE.local.md"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("the changes are\n%v\nwant\n%v", got, want)
	}
	if got, err := ConfigChanges(t.Context(), repo, main, main); err != nil || len(got) != 0 {
		t.Errorf("main against itself: %v, %v", got, err)
	}

	// A card that changes no configuration has none.
	repo, main, card = configRepo(t, configMain, map[string]string{"code.go": "card\n", "docs/claude.md": "card\n"})
	if got, err := ConfigChanges(t.Context(), repo, main, card); err != nil || len(got) != 0 {
		t.Errorf("a card without configuration changes: %v, %v", got, err)
	}
}

// laidOver is a run of the card's commit with main as Main, checked and laid
// over as carry does it for a run that starts.
func laidOver(t *testing.T, repo, main, card, parent string) *run {
	t.Helper()
	instruction := filepath.Join(t.TempDir(), "i.md")
	writeIn(t, filepath.Dir(instruction), "i.md", "Do it.\n", 0o644)
	in := Inputs{Repo: repo, Commit: card, Parent: parent, Main: main, Branch: "kiln/card-1", ID: "run1", Instruction: instruction,
		Runs: t.TempDir(), Image: "kiln-run", Proxy: "kiln-proxy", Model: "claude-haiku-5-5", Effort: "low"}
	r, err := check(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	mainTree, err := r.mainTreeOf(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if mainTree != "" {
		if err := r.layOver(t.Context(), mainTree); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// TestLayOver checks the work tree a laid-over run's /work shows: main's
// configuration, a path the card added absent and one it removed there, and
// the card's other files, and that building it leaves the copy's refs and
// index alone.
func TestLayOver(t *testing.T) {
	ctx := t.Context()
	repo, main, card := configRepo(t, configMain, configCard)
	refs := gitIn(t, repo, "for-each-ref")
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)

	r := laidOver(t, repo, main, card, main)
	if r.mainTree != gitIn(t, repo, "rev-parse", main+"^{tree}") {
		t.Fatalf("the main tree is %q", r.mainTree)
	}
	// The configuration is main's, everything else the card's.
	if d := gitIn(t, repo, append([]string{"diff", "--name-status", main, r.workTree, "--"}, ConfigPathspecs...)...); d != "" {
		t.Errorf("the work tree's configuration differs from main's:\n%s", d)
	}
	if d := gitIn(t, repo, append([]string{"diff", "--name-status", card, r.workTree, "--", "."}, excludes(ConfigPathspecs)...)...); d != "" {
		t.Errorf("the work tree differs from the card outside the configuration:\n%s", d)
	}
	paths := map[string]string{}
	for _, e := range r.workEntries {
		paths[e.path] = gitIn(t, repo, "cat-file", "blob", e.object)
	}
	for path, want := range map[string]string{
		".claude/hooks/gone.sh":            "main", // the card removed it
		"CLAUDE.md":                        "main",
		"pkg/AGENTS.md":                    "main",
		"nested/.claude/skills/n/SKILL.md": "main",
		".mcp.json":                        "{}",
		"docs/claude.md":                   "card",
		"pkg/.mcp.json":                    "card",
		"code.go":                          "card",
	} {
		if paths[path] != want {
			t.Errorf("%s in the work tree is %q, want %q", path, paths[path], want)
		}
	}
	for _, path := range []string{".claude/skills/new/SKILL.md", "sub/CLAUDE.local.md"} {
		if _, ok := paths[path]; ok {
			t.Errorf("%s, which the card added, is in the work tree", path)
		}
	}
	// The parent tree is main's here, so the work parent tree is main's tree.
	if r.workParentTree != r.mainTree {
		t.Errorf("the work parent tree is %s, main's tree %s", r.workParentTree, r.mainTree)
	}
	if tree, parent := r.baseTrees(); tree != r.workTree || parent != r.workParentTree {
		t.Errorf("the base trees are %s and %s", tree, parent)
	}

	// The card's whole configuration goes to /kiln/card-config.
	own := map[string]string{}
	for _, e := range r.cardConfig {
		own[e.path] = gitIn(t, repo, "cat-file", "blob", e.object)
	}
	wantOwn := map[string]string{
		"CLAUDE.md":                        "card",
		"pkg/AGENTS.md":                    "card",
		".claude/settings.json":            "{}",
		".claude/hooks/check.sh":           "card",
		".claude/skills/new/SKILL.md":      "card",
		"nested/.claude/skills/n/SKILL.md": "card",
		"sub/CLAUDE.local.md":              "card",
		".mcp.json":                        "{}",
	}
	if !maps.Equal(own, wantOwn) {
		t.Errorf("the card's configuration is\n%v\nwant\n%v", own, wantOwn)
	}
	archive, err := placingTar(ctx, repo, r.cardConfig)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(archive))
	n := 0
	for {
		if _, err := tr.Next(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != len(wantOwn) {
		t.Errorf("the card configuration's tar has %d entries, want %d", n, len(wantOwn))
	}

	// The same run built again gives the same trees.
	again := laidOver(t, repo, main, card, main)
	if again.workTree != r.workTree || again.workParentTree != r.workParentTree {
		t.Errorf("built again: %s %s, was %s %s", again.workTree, again.workParentTree, r.workTree, r.workParentTree)
	}

	// Nothing but objects is written: the refs are as they were, and the
	// copy has no index.
	if got := gitIn(t, repo, "for-each-ref"); got != refs {
		t.Errorf("the refs are\n%s\nwere\n%s", got, refs)
	}
	if _, err := os.Stat(filepath.Join(repo, "index")); !os.IsNotExist(err) {
		t.Errorf("the copy has an index: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(temp, "kiln-overlay-*")); len(left) > 0 {
		t.Errorf("temporary indexes are left: %v", left)
	}
}

// TestNotLaidOver checks that a run without Main, and one whose commit has no
// configuration change, is not laid over: /work is the commit's, as before.
func TestNotLaidOver(t *testing.T) {
	repo, main, card := configRepo(t, configMain, configCard)
	if r := laidOver(t, repo, "", card, main); r.mainTree != "" || r.workTree != "" {
		t.Errorf("a run without Main is laid over on %s", r.mainTree)
	} else if tree, parent := r.baseTrees(); tree != r.tree || parent != r.parentTree {
		t.Errorf("its base trees are %s and %s", tree, parent)
	}
	repo, main, card = configRepo(t, configMain, map[string]string{"code.go": "card\n"})
	if r := laidOver(t, repo, main, card, main); r.mainTree != "" {
		t.Errorf("a run without configuration changes is laid over on %s", r.mainTree)
	}
	got := strings.Join((&run{Inputs: Inputs{Model: "m", Effort: "low"}, tree: "aaa", parentTree: "bbb"}).command()[3:7], " ")
	if got != "sh /work /kiln/base.pack aaa" {
		t.Errorf("the command of a run not laid over names %q", got)
	}
}

// TestLayOverCommand checks that a laid-over run's command names the work
// trees in the place of the commit's.
func TestLayOverCommand(t *testing.T) {
	r := &run{Inputs: Inputs{Model: "claude-haiku-5-5", Effort: "low"}, tree: "aaa", parentTree: "bbb",
		mainTree: "mmm", workTree: "www", workParentTree: "ppp"}
	if got := strings.Join(r.command()[3:8], " "); got != "sh /work /kiln/base.pack www ppp" {
		t.Errorf("the command names %q", got)
	}
}

// TestLayOverGuard checks the refusal of a card whose file stands where main
// has configuration under it, which the work tree would otherwise drop.
func TestLayOverGuard(t *testing.T) {
	repo, main, card := configRepo(t, map[string]string{"foo/.claude/x": "main\n", "CLAUDE.md": "main\n"},
		map[string]string{"foo": "a file of the card's\n", "CLAUDE.md": "card\n"})
	instruction := filepath.Join(t.TempDir(), "i.md")
	writeIn(t, filepath.Dir(instruction), "i.md", "Do it.\n", 0o644)
	r, err := check(t.Context(), Inputs{Repo: repo, Commit: card, Main: main, Branch: "kiln/card-1", ID: "run1",
		Instruction: instruction, Runs: t.TempDir(), Image: "kiln-run", Proxy: "kiln-proxy", Model: "claude-haiku-5-5", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	mainTree, err := r.mainTreeOf(t.Context())
	if err != nil || mainTree == "" {
		t.Fatalf("the main tree: %q, %v", mainTree, err)
	}
	err = r.layOver(t.Context(), mainTree)
	want := "the card's foo stands where main's Claude Code configuration is; the run is not started"
	if err == nil || err.Error() != want {
		t.Errorf("want %q, got %v", want, err)
	}
	if r.mainTree != "" {
		t.Errorf("the refused run is laid over on %s", r.mainTree)
	}
}

// TestReachesKiln checks the walk of a link of the work tree against what
// the run's kernel would reach: at or under /kiln, or ending at /, is refused.
func TestReachesKiln(t *testing.T) {
	for _, c := range []struct {
		name  string
		links map[string]string
		walk  string
		want  bool
		err   bool
	}{
		// The card's cases.
		{"absolute into the card's configuration", map[string]string{"y": "/kiln/card-config/evil"}, "y", true, false},
		{"relative up to /kiln", map[string]string{"y": "../kiln/card-config"}, "y", true, false},
		{"nested, relative up to /kiln", map[string]string{"a/b": "../../kiln"}, "a/b", true, false},
		{"a chain through a link into /kiln", map[string]string{"z": "y", "y": "/kiln/card-config/evil"}, "z", true, false},
		// What a lexical rule, link by link, would let through.
		{"to /", map[string]string{"root": "/"}, "root", true, false},
		{"up to / at the top", map[string]string{"up": ".."}, "up", true, false},
		{".. after a link", map[string]string{"c": "/usr", "x": "c/../kiln"}, "x", true, false},
		{"through a link to /", map[string]string{"root": "/", "d": "root/kiln/out"}, "d", true, false},
		{"through /kiln and out", map[string]string{"k": "/kiln/../usr"}, "k", true, false},
		{"far up", map[string]string{"a/far": "../../../.."}, "a/far", true, false},
		{"a loop", map[string]string{"l1": "l2", "l2": "l1"}, "l1", false, true},
		{"a link to itself", map[string]string{"self": "self"}, "self", false, true},
		// Harmless links.
		{"a program", map[string]string{"env": "/usr/bin/env"}, "env", false, false},
		{"a file inside /work", map[string]string{"sub/r": "../README.md"}, "sub/r", false, false},
		{"a name that starts like /kiln", map[string]string{"o": "/kiln-other"}, "o", false, false},
		{"under a name that starts like /kiln", map[string]string{"o": "/kilnx/y"}, "o", false, false},
		{"to /work", map[string]string{"w": "/work"}, "w", false, false},
		{"to its own directory", map[string]string{"dot": "."}, "dot", false, false},
		{"a chain that stays harmless", map[string]string{"z": "c/bin", "c": "/usr"}, "z", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := reachesKiln(c.links, "/work/"+c.walk)
			if c.err {
				if !errors.Is(err, errTooManyLinks) {
					t.Errorf("want too many symlinks, got %v, %v", got, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Errorf("got %v, %v, want %v", got, err, c.want)
			}
		})
	}
}

// linkCard is a run of a laid-over card with the symlink y to target.
func linkCard(t *testing.T, target string) *run {
	t.Helper()
	repo, main, card := linkRepo(t, map[string]string{"README.md": "main\n"},
		map[string]string{"evil/CLAUDE.md": "card rules\n"}, map[string]string{"y": target})
	r := laidOver(t, repo, main, card, main)
	if r.mainTree == "" {
		t.Fatal("the card is not laid over")
	}
	return r
}

// TestLayOverLinkGuard checks, through Git, that a laid-over run whose link
// leads into /kiln is refused with the link named, and one with a harmless
// link is not.
func TestLayOverLinkGuard(t *testing.T) {
	err := linkCard(t, "/kiln/card-config/evil").refuseLinksIntoKiln(t.Context())
	want := "the symlink y -> /kiln/card-config/evil leads into /kiln, where a laid-over run has the card's own Claude Code configuration; the run is not started"
	if err == nil || err.Error() != want {
		t.Errorf("want %q, got %v", want, err)
	}
	if err := linkCard(t, "/usr/bin/env").refuseLinksIntoKiln(t.Context()); err != nil {
		t.Errorf("a harmless link: %v", err)
	}

	// The first link in the tree's order is named, and a loop is refused.
	repo, main, card := linkRepo(t, map[string]string{"README.md": "main\n"}, map[string]string{"CLAUDE.md": "card\n"},
		map[string]string{"a": "b", "b": "a", "c": "/"})
	err = laidOver(t, repo, main, card, main).refuseLinksIntoKiln(t.Context())
	want = "the symlink a -> b is not followed to its end within 40 symlinks; the run is not started"
	if err == nil || err.Error() != want {
		t.Errorf("want %q, got %v", want, err)
	}
}

// fakeDocker puts first on PATH a docker that logs its arguments, one call a
// line, prints nothing and succeeds, and returns the log's path.
func fakeDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	writeIn(t, dir, "docker", "#!/bin/sh\necho \"$*\" >> '"+log+"'\n", 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// TestLinkGuardCreatesNothing checks, through Carry, that both guards of a
// laid-over run refuse it before anything is created, and that a run that is
// not laid over is not checked for links.
func TestLinkGuardCreatesNothing(t *testing.T) {
	inputs := func(repo, main, card string) Inputs {
		instruction := filepath.Join(t.TempDir(), "i.md")
		writeIn(t, filepath.Dir(instruction), "i.md", "Do it.\n", 0o644)
		return Inputs{Repo: repo, Commit: card, Parent: main, Main: main, Branch: "kiln/card-1", ID: "run1", Instruction: instruction,
			Runs: t.TempDir(), Image: "kiln-run", Proxy: "kiln-proxy", Model: "claude-haiku-5-5", Effort: "low"}
	}
	calls := func(log string) []string {
		data, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	refused := func(t *testing.T, in Inputs, want string) {
		log := fakeDocker(t)
		_, err := Carry(t.Context(), in, func() error { t.Error("starting is called"); return nil }, io.Discard)
		if err == nil || err.Error() != want {
			t.Errorf("want %q, got %v", want, err)
		}
		if got := calls(log); len(got) != 1 || !strings.HasPrefix(got[0], "ps --all ") {
			t.Errorf("docker is called with %q, want only the ps of InspectRun", got)
		}
	}

	t.Run("a link into /kiln", func(t *testing.T) {
		repo, main, card := linkRepo(t, map[string]string{"README.md": "main\n"},
			map[string]string{"evil/CLAUDE.md": "card rules\n"}, map[string]string{"y": "/kiln/card-config/evil"})
		refused(t, inputs(repo, main, card),
			"the symlink y -> /kiln/card-config/evil leads into /kiln, where a laid-over run has the card's own Claude Code configuration; the run is not started")
	})
	t.Run("a card file where main has configuration", func(t *testing.T) {
		repo, main, card := configRepo(t, map[string]string{"foo/.claude/x": "main\n", "CLAUDE.md": "main\n"},
			map[string]string{"foo": "a file of the card's\n", "CLAUDE.md": "card\n"})
		refused(t, inputs(repo, main, card), "the card's foo stands where main's Claude Code configuration is; the run is not started")
	})
	t.Run("not laid over", func(t *testing.T) {
		repo, main, card := linkRepo(t, map[string]string{"README.md": "main\n"},
			map[string]string{"code.go": "card\n"}, map[string]string{"y": "/kiln/card-config/evil"})
		log := fakeDocker(t)
		_, err := Carry(t.Context(), inputs(repo, main, card), func() error { return errors.New("not started") }, io.Discard)
		if err == nil || strings.Contains(err.Error(), "symlink") {
			t.Errorf("want an error of the fake docker, got %v", err)
		}
		if got := calls(log); len(got) < 2 {
			t.Errorf("docker is called with %q only: the run stopped in step 0", got)
		}
	})
}
