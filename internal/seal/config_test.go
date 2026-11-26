package seal

import (
	"archive/tar"
	"bytes"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestPlacingCardConfig checks what placing writes for a laid-over run: the
// card's configuration under kiln/card-config/, checked and owned as /work is,
// the directory also when its tar is empty, and nothing of it without one.
func TestPlacingCardConfig(t *testing.T) {
	names := func(t *testing.T, archive []byte) map[string]tar.Header {
		t.Helper()
		got := map[string]tar.Header{}
		tr := tar.NewReader(bytes.NewReader(archive))
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				return got
			}
			if err != nil {
				t.Fatal(err)
			}
			got[hdr.Name] = *hdr
		}
	}
	work := tarOf(t, file("CLAUDE.md", 0o644, "main's\n"))
	own := tarOf(t, file("CLAUDE.md", 0o644, "the card's\n"), file(".claude/hooks/check.sh", 0o755, "exit 0\n"))

	archive, err := placing(bytes.NewReader(work), bytes.NewReader(own), []byte("i\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := names(t, archive)
	for _, want := range []string{"work/", "work/CLAUDE.md", "kiln/card-config/", "kiln/card-config/CLAUDE.md",
		"kiln/card-config/.claude/", "kiln/card-config/.claude/hooks/", "kiln/card-config/.claude/hooks/check.sh", instructionPath} {
		h, ok := got[want]
		if !ok {
			t.Errorf("%s is not placed; placed: %v", want, slices.Sorted(maps.Keys(got)))
			continue
		}
		if h.Uid != runUID || h.Gid != runUID || h.Uname != "" || h.PAXRecords["SCHILY.xattr.com.apple.provenance"] != "" {
			t.Errorf("%s is owned by %d:%d %q, with %v", want, h.Uid, h.Gid, h.Uname, h.PAXRecords)
		}
	}
	if h := got["kiln/card-config/.claude/hooks/check.sh"]; h.Mode != 0o755 {
		t.Errorf("the hook has the mode %o", h.Mode)
	}

	// An empty tar still gives the directory, and none gives nothing.
	archive, err = placing(bytes.NewReader(work), bytes.NewReader(tarOf(t)), []byte("i\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := names(t, archive)["kiln/card-config/"]; !ok {
		t.Error("an empty card configuration gives no kiln/card-config/")
	}
	archive, err = placing(bytes.NewReader(work), nil, []byte("i\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for name := range names(t, archive) {
		if strings.HasPrefix(name, cardConfigDir) {
			t.Errorf("a run that is not laid over has %s", name)
		}
	}

	// Its names are judged as those of /work are.
	for _, bad := range []tarEntry{file("../escape", 0o644, "x"), file("/abs", 0o644, "x"),
		{hdr: tar.Header{Typeflag: tar.TypeFifo, Name: "fifo"}}} {
		if _, err := placing(bytes.NewReader(work), bytes.NewReader(tarOf(t, bad)), []byte("i\n"), nil); err == nil {
			t.Errorf("the card configuration's %q is placed", bad.hdr.Name)
		}
	}
	// A name given twice in the card's configuration is refused.
	if _, err := placing(bytes.NewReader(work), bytes.NewReader(tarOf(t, file("CLAUDE.md", 0o644, "x"), file("CLAUDE.md", 0o644, "y"))), []byte("i\n"), nil); err == nil {
		t.Error("a name given twice in the card configuration is placed")
	}
}

// configPathspecs are the pathspecs of internal/run's ConfigPathspecs, which
// this package does not import.
var configPathspecs = []string{
	":(top,glob)**/CLAUDE.md",
	":(top,glob)**/CLAUDE.local.md",
	":(top,glob)**/AGENTS.md",
	":(top,glob)**/.claude",
	":(top,glob)**/.claude/**",
	":(top,literal).mcp.json",
}

// configSaveRunScript is the command of the run that testConfigSave saves. It
// runs in /work, as uid 1000, with the card's configuration under
// /kiln/card-config.
const configSaveRunScript = `set -eu
echo edit >> CLAUDE.md
mkdir -p new/.claude
echo z > new/.claude/z
echo changed >> pkg/sub/code.go
cd /kiln/card-config
echo fix >> CLAUDE.md
mkdir -p .claude/skills/new
echo skill > .claude/skills/new/SKILL.md
rm .claude/hooks/gone.sh
echo nc > notconfig.go
mkdir -p .claude/nested/.git
echo junk > .claude/nested/.git/HEAD
echo n > .claude/nested/n.txt
`

// testConfigSave saves a laid-over run (ADR 0025): the configuration comes
// from /kiln/card-config, everything else from /work.
func testConfigSave(t *testing.T, f fixture) {
	card := map[string]string{
		"CLAUDE.md":              "card\n",
		".claude/hooks/check.sh": "card\n",
		".claude/hooks/gone.sh":  "gone\n",
		"pkg/AGENTS.md":          "card\n",
		"pkg/sub/code.go":        "code\n",
	}
	repo := t.TempDir()
	gitIn(t, repo, nil, "init", "--quiet")
	var own []tarEntry
	for p, data := range card {
		writeFile(t, repo, p, data)
		if p != "pkg/sub/code.go" {
			own = append(own, file(p, 0o644, data))
		}
	}
	gitIn(t, repo, nil, "add", "--force", ".")
	tree := gitIn(t, repo, nil, "write-tree")
	objects := gitIn(t, repo, nil, "rev-list", "--objects", tree)
	base := gitIn(t, repo, strings.NewReader(objects+"\n"), "pack-objects", "--stdout")
	// /work as a laid-over run has it: main's CLAUDE.md, the card's code.
	work := []tarEntry{file("CLAUDE.md", 0o644, "main\n"), file(".claude/hooks/check.sh", 0o644, "main\n"),
		file("pkg/AGENTS.md", 0o644, "main\n"), file("pkg/sub/code.go", 0o644, "code\n")}

	id := f.runID("cfgsave")
	createRun(t, Run{ID: id, Image: f.runImage, Proxy: f.proxy.Name, Command: []string{"sh", "-c", configSaveRunScript}})
	if err := PlaceRun(t.Context(), id, bytes.NewReader(tarOf(t, work...)), bytes.NewReader(tarOf(t, own...)), []byte("save\n"), nil); err != nil {
		t.Fatal(err)
	}
	startAndWait(t, id)

	newTree, pack, err := SaveRun(t.Context(), id, f.runImage, tree, []byte(base), configPathspecs)
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, bytes.NewReader(pack), "index-pack", "--stdin", "--strict")
	got := treeOf(t, repo, newTree)
	want := map[string]string{
		"CLAUDE.md":                   "100644 card\nfix\n",
		".claude/hooks/check.sh":      "100644 card\n",
		".claude/skills/new/SKILL.md": "100644 skill\n",
		".claude/nested/n.txt":        "100644 n\n",
		"pkg/AGENTS.md":               "100644 card\n",
		"pkg/sub/code.go":             "100644 code\nchanged\n",
	}
	if !maps.Equal(got, want) {
		t.Errorf("the save's tree is\n%v\nwant\n%v", got, want)
	}

	// Without the pathspecs, the save is of /work alone, as before.
	whole, pack, err := SaveRun(t.Context(), id, f.runImage, tree, []byte(base), nil)
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, bytes.NewReader(pack), "index-pack", "--stdin", "--strict")
	if got := treeOf(t, repo, whole); got["CLAUDE.md"] != "100644 main\nedit\n" || got["new/.claude/z"] != "100644 z\n" {
		t.Errorf("the save of /work alone is %v", got)
	}
	if _, _, err := SaveRun(t.Context(), id, f.runImage, tree, []byte(base), []string{"**/CLAUDE.md"}); err == nil {
		t.Error("a pathspec without :( is taken")
	}
	assertNoSave(t, id)
}
