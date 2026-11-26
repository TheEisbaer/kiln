package run

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// commitOn makes a commit on top of parent in a bare repo, with files written
// over the parent's tree, by a temporary index of its own, and returns it. It
// moves no ref.
func commitOn(t *testing.T, repo, parent string, files map[string]string) string {
	t.Helper()
	env := append(gitEnv(), "GIT_INDEX_FILE="+filepath.Join(t.TempDir(), "index"),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@localhost", "GIT_AUTHOR_DATE=2026-11-17T10:00:00Z",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@localhost", "GIT_COMMITTER_DATE=2026-11-17T10:00:00Z")
	git := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"--git-dir=" + repo}, args...)...)
		cmd.Env, cmd.Stdin = env, strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("", "read-tree", parent)
	for name, data := range files {
		blob := git(data, "hash-object", "-w", "--stdin")
		git("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+name)
	}
	return git("message\n", "commit-tree", "-p", parent, git("", "write-tree"))
}

// testLaidOver is a laid-over run (ADR 0025) killed after its container is
// created, with main moved to another CLAUDE.md before it is started again:
// the repeat places the work tree of the label's main, not today's, the
// card's CLAUDE.md lies at /kiln/card-config, and the save keeps the card's.
// Every CLAUDE.md keeps the marker, which the upstream checks.
func testLaidOver(t *testing.T, f *fixture) {
	marker := "The marker is " + f.marker + "."
	card := commitOn(t, f.repo, f.commit, map[string]string{"CLAUDE.md": "# Card\n\n" + marker + " The card's version.\n"})
	moved := commitOn(t, f.repo, f.commit, map[string]string{"CLAUDE.md": "# Moved\n\n" + marker + " Main moved.\n"})

	c := f.newCase(t, "config")
	c.in.Commit, c.in.Main = card, f.commit
	if code, out, errOut := f.kiln(t, c, "create:exit", true); code != exitAfterStep {
		t.Fatalf("exit status %d, want %d\n%s\n%s", code, exitAfterStep, out, errOut)
	}
	if got := must(t, "inspect", "--format", `{{index .Config.Labels "kiln.main-tree"}}`, "kiln-run-"+c.id); got != gitIn(t, f.repo, "rev-parse", f.commit+"^{tree}") {
		t.Errorf("the label kiln.main-tree is %q", got)
	}
	c.in.Main = moved
	if code, out, errOut := f.kiln(t, c, "", true); code != 0 {
		t.Fatalf("started again: exit status %d\n%s\n%s", code, out, errOut)
	}

	got := toolResultsIn(t, filepath.Join(f.runs, c.id, "output.jsonl"))["toolu_kiln_1"]
	if want := "# Try\n\n" + marker + "\n# Card\n\n" + marker + " The card's version.\n"; !strings.Contains(got, want) {
		t.Errorf("the run saw\n%q\nwant main's first CLAUDE.md in /work and the card's in /kiln/card-config:\n%q", got, want)
	}
	if strings.Contains(got, "Main moved.") {
		t.Errorf("the repeat placed today's main: %q", got)
	}

	// The save keeps the card's CLAUDE.md and the run's line.
	save := gitIn(t, f.repo, "rev-parse", "refs/heads/"+c.in.Branch)
	if got := gitIn(t, f.repo, "diff", "--numstat", card, save); got != "1\t0\tNOTES.md" {
		t.Errorf("the save's change is %q, want one line added to NOTES.md", got)
	}
	if got := gitIn(t, f.repo, "log", "-1", "--format=%P", save); got != card {
		t.Errorf("the save's parent is %s, want the card's commit %s", got, card)
	}
	if _, err := os.Stat(filepath.Join(f.runs, c.id, "result.json")); err != nil {
		t.Error(err)
	}
}
