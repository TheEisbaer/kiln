package run

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gitIn runs git in a directory for a test, with a fixed author and dates, and
// returns its output, trimmed.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@localhost", "GIT_AUTHOR_DATE=2026-11-17T10:00:00Z",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@localhost", "GIT_COMMITTER_DATE=2026-11-17T10:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeIn writes a file under a directory, with its parents.
func writeIn(t *testing.T, dir, name, data string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // past the umask
		t.Fatal(err)
	}
}

// bareRepo makes a commit of the given files in a fresh repo, and returns a
// bare copy of it and the commit's ID. A file whose data starts with "->" is a
// symlink to the rest.
func bareRepo(t *testing.T, files map[string]string, executable ...string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	gitIn(t, dir, "init", "--quiet", "--initial-branch=main", work)
	for name, data := range files {
		if target, ok := strings.CutPrefix(data, "->"); ok {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(work, name)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		writeIn(t, work, name, data, 0o644)
	}
	for _, name := range executable {
		if err := os.Chmod(filepath.Join(work, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, work, "add", "--all", "--force")
	gitIn(t, work, "commit", "--quiet", "--message", "start")
	bare := filepath.Join(dir, "copy.git")
	gitIn(t, dir, "clone", "--quiet", "--bare", work, bare)
	return bare, gitIn(t, work, "rev-parse", "HEAD")
}

func TestPlacingTar(t *testing.T) {
	repo, commit := bareRepo(t, map[string]string{
		"README.md":        "read me\n",
		"bin/run.sh":       "echo hi\n",
		"link":             "->README.md",
		"export-ignore.md": "kept\n",
		".gitattributes":   "export-ignore.md export-ignore\n",
		"a/b/c/deep.txt":   "deep\n",
	}, "bin/run.sh")
	entries, err := listTree(t.Context(), repo, commit)
	if err != nil {
		t.Fatal(err)
	}
	data, err := placingTar(t.Context(), repo, entries)
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		typ  byte
		mode int64
		link string
		data string
	}
	got := map[string]entry{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = entry{h.Typeflag, h.Mode, h.Linkname, string(b)}
	}
	want := map[string]entry{
		".gitattributes":   {tar.TypeReg, 0o644, "", "export-ignore.md export-ignore\n"},
		"README.md":        {tar.TypeReg, 0o644, "", "read me\n"},
		"a/b/c/deep.txt":   {tar.TypeReg, 0o644, "", "deep\n"},
		"bin/run.sh":       {tar.TypeReg, 0o755, "", "echo hi\n"},
		"export-ignore.md": {tar.TypeReg, 0o644, "", "kept\n"}, // git archive would leave it out
		"link":             {tar.TypeSymlink, 0o777, "README.md", ""},
	}
	if len(got) != len(want) {
		t.Errorf("the tar has %d entries, want %d: %v", len(got), len(want), got)
	}
	for name, w := range want {
		if g, ok := got[name]; !ok || g != w {
			t.Errorf("%s: got %+v, want %+v", name, g, w)
		}
	}

	t.Run("a submodule is refused", func(t *testing.T) {
		dir := t.TempDir()
		gitIn(t, dir, "init", "--quiet", "--bare", "copy.git")
		bare := filepath.Join(dir, "copy.git")
		gitIn(t, bare, "update-index", "--add", "--cacheinfo", "160000,"+commit+",sub")
		tree := gitIn(t, bare, "write-tree")
		withSub := gitIn(t, bare, "commit-tree", "-m", "sub", tree)
		entries, err := listTree(t.Context(), bare, withSub)
		if err != nil {
			t.Fatal(err)
		}
		if err := refuseSubmodules(entries); err == nil || !strings.Contains(err.Error(), "submodule") {
			t.Errorf("refuseSubmodules: want a refusal of the submodule, got %v", err)
		}
		if _, err := placingTar(t.Context(), bare, entries); err == nil {
			t.Error("placingTar: no error")
		}
	})
}

// output is a run's output in Claude Code 2.1.326's stream-json form, as the
// scripted upstream makes it.
const output = `{"type":"system","subtype":"init","cwd":"/work","model":"claude-haiku-5-5","permissionMode":"bypassPermissions","claude_code_version":"2.1.326"}
{"type":"assistant","message":{"content":[{"id":"toolu_1","input":{"command":"sh run.sh","description":"Run the script"},"name":"Bash","type":"tool_use"}]}}
not an event
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"waiting"}]}}
{"type":"assistant","message":{"content":[{"text":"\nDone.\nAnd more.","type":"text"}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":2,"total_cost_usd":0.00003}
`

func TestReadEvents(t *testing.T) {
	var printed bytes.Buffer
	var mismatches []string
	f := &follower{out: &printed, model: "claude-haiku-5-5", mismatch: func(seen string) { mismatches = append(mismatches, seen) }}
	s, err := readEvents(strings.NewReader(output), f.see)
	if err != nil {
		t.Fatal(err)
	}
	if s.model != "claude-haiku-5-5" {
		t.Errorf("the model seen is %q", s.model)
	}
	if want := (Result{Subtype: "success", NumTurns: 2, TotalCostUSD: 0.00003}); s.result == nil || *s.result != want {
		t.Errorf("the result is %+v, want %+v", s.result, want)
	}
	if len(mismatches) != 0 {
		t.Errorf("a mismatch was seen: %q", mismatches)
	}
	want := "model claude-haiku-5-5, Claude Code 2.1.326\n" +
		"tool Bash: Run the script\n" +
		"text: Done.\n" +
		"result success, 2 turns, $0.0000\n"
	if printed.String() != want {
		t.Errorf("following printed\n%s\nwant\n%s", printed.String(), want)
	}

	t.Run("a model mismatch", func(t *testing.T) {
		var mismatches []string
		f := &follower{out: io.Discard, model: "claude-opus-5-5", mismatch: func(seen string) { mismatches = append(mismatches, seen) }}
		// Twice, as when the run is followed again from the first event.
		if _, err := readEvents(strings.NewReader(output+output), f.see); err != nil {
			t.Fatal(err)
		}
		if len(mismatches) != 1 || mismatches[0] != "claude-haiku-5-5" {
			t.Errorf("the mismatches seen are %q, want claude-haiku-5-5 once", mismatches)
		}
		rec := &record{Inputs: RecordInputs{Model: "claude-opus-5-5"}, ModelSeen: "claude-haiku-5-5",
			Result: &Result{Subtype: "success"}, Saved: noChange}
		if rec.status() == 0 {
			t.Error("a run on another model has the exit status 0")
		}
	})

	t.Run("a run with no result", func(t *testing.T) {
		lines := strings.SplitAfter(output, "\n")
		s, err := readEvents(strings.NewReader(strings.Join(lines[:4], "")), nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.result != nil {
			t.Errorf("the result is %+v, want none", s.result)
		}
		rec := &record{Inputs: RecordInputs{Model: s.model}, ModelSeen: s.model, Saved: noChange}
		if rec.status() == 0 {
			t.Error("a run with no result has the exit status 0")
		}
		rec.Result = &Result{Subtype: "success"}
		if rec.status() != 0 {
			t.Error("a run with a result, the model and exit 0 does not have the exit status 0")
		}
	})
}

func TestHostSave(t *testing.T) {
	repo, commit := bareRepo(t, map[string]string{"NOTES.md": "notes\n"})
	ctx := t.Context()
	// A tree that differs from the commit's.
	blob := gitIn(t, repo, "hash-object", "-w", "/dev/null")
	tree := strings.TrimSpace(func() string {
		cmd := exec.Command("git", "--git-dir="+repo, "mktree")
		cmd.Stdin = strings.NewReader("100644 blob " + blob + "\tNEW.md\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}())
	end := time.Date(2026, 11, 17, 12, 0, 0, 123456789, time.UTC)

	first, err := commitSave(ctx, repo, tree, commit, "run1", end)
	if err != nil {
		t.Fatal(err)
	}
	second, err := commitSave(ctx, repo, tree, commit, "run1", end)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("the same save made twice gave %s and %s", first, second)
	}

	raw := gitIn(t, repo, "cat-file", "commit", first)
	date := strconv.FormatInt(end.Unix(), 10) + " +0000"
	for _, want := range []string{
		"tree " + tree + "\n",
		"parent " + commit + "\n",
		"author Kiln <kiln@localhost> " + date + "\n",
		"committer Kiln <kiln@localhost> " + date + "\n",
		"\n\nchore: save run run1\n\nKiln-Run: run1",
	} {
		if !strings.Contains(raw+"\n", want) {
			t.Errorf("the commit lacks %q:\n%s", want, raw)
		}
	}
	for _, c := range []struct {
		parent, id string
		want       bool
	}{{commit, "run1", true}, {commit, "run2", false}, {first, "run1", false}} {
		if got, err := isSaveOf(ctx, repo, first, c.parent, c.id); err != nil || got != c.want {
			t.Errorf("isSaveOf(%s, %s) = %v, %v; want %v", c.parent, c.id, got, err, c.want)
		}
	}

	// The guard: a branch that must be absent, and one that must be at the commit.
	ref := "refs/heads/card"
	if err := moveBranch(ctx, repo, ref, first, ""); err != nil {
		t.Fatal(err)
	}
	if err := moveBranch(ctx, repo, ref, first, ""); err == nil {
		t.Error("a branch that exists was moved with the guard \"\"")
	}
	if err := moveBranch(ctx, repo, ref, commit, commit); err == nil {
		t.Error("a branch at the save was moved with the guard of the commit")
	}
	if at, err := branchAt(ctx, repo, ref); err != nil || at != first {
		t.Errorf("the branch is at %q (%v), want %s", at, err, first)
	}
	if at, err := branchAt(ctx, repo, "refs/heads/none"); err != nil || at != "" {
		t.Errorf("a missing branch is at %q (%v), want \"\"", at, err)
	}
}

func TestCheckFlags(t *testing.T) {
	good := Inputs{Repo: "r", Commit: "c", Branch: "b", ID: "run1", Instruction: "i", Runs: "d",
		Image: "kiln-run", Proxy: "kiln-proxy", Model: "claude-haiku-5-5", Effort: "low"}
	if err := checkFlags(good); err != nil {
		t.Fatalf("good inputs: %v", err)
	}
	for _, c := range []struct {
		what string
		edit func(*Inputs)
	}{
		{"an alias", func(in *Inputs) { in.Model = "haiku" }},
		{"the alias opusplan", func(in *Inputs) { in.Model = "opusplan" }},
		{"a model with [", func(in *Inputs) { in.Model = "claude-opus-5-5[1m]" }},
		{"only the prefix", func(in *Inputs) { in.Model = "claude-" }},
		{"an effort above high", func(in *Inputs) { in.Effort = "xhigh" }},
		{"the effort max", func(in *Inputs) { in.Effort = "max" }},
		{"no effort", func(in *Inputs) { in.Effort = "" }},
		{"a bad ID", func(in *Inputs) { in.ID = "../x" }},
		{"an ID with capitals", func(in *Inputs) { in.ID = "Run1" }},
		{"no repo", func(in *Inputs) { in.Repo = "" }},
	} {
		in := good
		c.edit(&in)
		if err := checkFlags(in); err == nil {
			t.Errorf("%s: no error", c.what)
		}
	}
}

func TestCommand(t *testing.T) {
	r := &run{Inputs: Inputs{Model: "claude-haiku-5-5", Effort: "low"}}
	got := strings.Join(r.command(), " ")
	want := `sh -c exec claude "$@" < /kiln/instruction.md sh -p --output-format stream-json --verbose --permission-mode bypassPermissions --model claude-haiku-5-5 --effort low`
	if got != want {
		t.Errorf("the command is\n%s\nwant\n%s", got, want)
	}
}
