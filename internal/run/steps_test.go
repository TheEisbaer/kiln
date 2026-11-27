package run

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		if rec.status(false) == 0 {
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
		if rec.status(false) == 0 {
			t.Error("a run with no result has the exit status 0")
		}
		rec.Result = &Result{Subtype: "success"}
		if rec.status(false) != 0 {
			t.Error("a run with a result, the model and exit 0 does not have the exit status 0")
		}
	})

	// A run is failed when any one fact of its record is, exit 0 included.
	t.Run("failed with exit 0", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			oom    bool
			result *Result
			reason string
		}{
			{"killed for memory", true, &Result{Subtype: "success", NumTurns: 3, TotalCostUSD: 0.25},
				"exit 0, killed for memory, result success, 3 turns, $0.2500, no change"},
			{"a result that is an error", false, &Result{Subtype: "success", IsError: true, NumTurns: 3, TotalCostUSD: 0.25},
				"exit 0, result is_error, 3 turns, $0.2500, no change"},
			{"no result", false, nil, "exit 0, no result, no change"},
		} {
			rec := &record{Inputs: RecordInputs{Model: "claude-haiku-5-5"}, ModelSeen: "claude-haiku-5-5",
				Ending: Ending{OOMKilled: c.oom}, Result: c.result, Saved: noChange}
			if rec.status(false) == 0 {
				t.Errorf("%s: the run has the exit status 0", c.name)
			}
			if got := rec.reason(false); got != c.reason {
				t.Errorf("%s: the reason is %q, want %q", c.name, got, c.reason)
			}
			if rec.refused() {
				t.Errorf("%s: the run is told refused", c.name)
			}
		}
	})
}

// refusedOutput is the output of a run whose credential the upstream refused,
// with the events Claude Code wrote in a real run on 2026-11-23, cut short: a
// retry after a 401, one after a 500, and the result that ended the run.
const refusedOutput = `{"type":"system","subtype":"init","cwd":"/work","model":"claude-opus-5-5","claude_code_version":"2.1.326"}
{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":509,"error_status":401,"error":"authentication_failed"}
{"type":"system","subtype":"api_retry","attempt":2,"max_retries":10,"retry_delay_ms":1022,"error_status":401,"error":"authentication_failed"}
{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":511,"error_status":500,"error":"server_error"}
{"type":"system","subtype":"status","error_status":401}
{"type":"result","subtype":"success","is_error":true,"num_turns":23,"total_cost_usd":1.5,"api_error_status":401,"terminal_reason":"api_error"}
`

// TestRefused checks that following calls refused for each retry after a 401
// and for nothing else, and that a run that the 401 ended says so in its
// reason.
func TestRefused(t *testing.T) {
	refused := 0
	f := &follower{out: io.Discard, model: "claude-opus-5-5", mismatch: func(string) {}, refused: func() { refused++ }}
	s, err := readEvents(strings.NewReader(refusedOutput), f.see)
	if err != nil {
		t.Fatal(err)
	}
	if refused != 2 {
		t.Errorf("refused was called %d times, want 2", refused)
	}
	if want := (Result{Subtype: "success", IsError: true, NumTurns: 23, TotalCostUSD: 1.5, APIErrorStatus: 401}); s.result == nil || *s.result != want {
		t.Errorf("the result is %+v, want %+v", s.result, want)
	}
	rec := &record{Inputs: RecordInputs{Model: "claude-opus-5-5"}, ModelSeen: s.model, Result: s.result,
		Ending: Ending{ExitCode: 1}, Saved: notSaved}
	if rec.status(false) == 0 {
		t.Error("a run that the 401 ended has the exit status 0")
	}
	if got, want := rec.reason(false), "the upstream refused the Claude credential (401), 23 turns, $1.5000, not saved"; got != want {
		t.Errorf("the reason is %q, want %q", got, want)
	}
	if !rec.refused() {
		t.Error("a run that the 401 ended is not told refused")
	}
	// Following without refused set, as `kiln run` by hand does.
	f.refused = nil
	if _, err := readEvents(strings.NewReader(refusedOutput), f.see); err != nil {
		t.Fatal(err)
	}
	// Another error of the API keeps the reason as it was.
	rec.Result.APIErrorStatus = 500
	if got, want := rec.reason(false), "exit 1, result is_error, 23 turns, $1.5000, not saved"; got != want {
		t.Errorf("with a 500 the reason is %q, want %q", got, want)
	}
	if rec.refused() {
		t.Error("a run that a 500 ended is told refused")
	}
}

// TestActivities checks what following hands to the screen: each text, cut at
// 500 characters, and each tool use with one line of its input.
func TestActivities(t *testing.T) {
	var got []Activity
	f := &follower{out: io.Discard, model: "claude-haiku-5-5", mismatch: func(string) {},
		activity: func(a Activity) { got = append(got, a) }}
	long := strings.Repeat("é", 501)
	events := output + `{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"Read","input":{"file_path":"/work/a.go\nsecond"}},` +
		`{"type":"tool_use","name":"Odd","input":{"n":1}},` +
		`{"type":"tool_use","name":"Empty","input":{}},` +
		`{"type":"text","text":"` + long + `"},{"type":"text","text":"  "}]}}` + "\n"
	if _, err := readEvents(strings.NewReader(events), f.see); err != nil {
		t.Fatal(err)
	}
	want := []Activity{
		{Kind: "tool", Tool: "Bash", Text: "Run the script"},
		{Kind: "text", Text: "Done.\nAnd more."},
		{Kind: "tool", Tool: "Read", Text: "/work/a.go"},
		{Kind: "tool", Tool: "Odd", Text: `{"n":1}`},
		{Kind: "tool", Tool: "Empty", Text: ""},
		{Kind: "text", Text: strings.Repeat("é", 500) + "…"},
	}
	if len(got) != len(want) {
		t.Fatalf("activities %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].At.IsZero() {
			t.Errorf("activity %d has no time", i)
		}
		got[i].At = time.Time{}
		if got[i] != want[i] {
			t.Errorf("activity %d is %+v, want %+v", i, got[i], want[i])
		}
	}
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

	first, err := commitSave(ctx, repo, tree, commit, "run1", "", end)
	if err != nil {
		t.Fatal(err)
	}
	second, err := commitSave(ctx, repo, tree, commit, "run1", "", end)
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

	// With a title, the title is the subject and the trailer the last paragraph.
	titled, err := commitSave(ctx, repo, tree, commit, "run1", "feat(run): x", end)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := commitSave(ctx, repo, tree, commit, "run1", "feat(run): x", end); err != nil || again != titled {
		t.Errorf("the titled save made twice gave %s and %s (%v)", titled, again, err)
	}
	if raw := gitIn(t, repo, "cat-file", "commit", titled); !strings.HasSuffix(raw+"\n", "\n\nfeat(run): x\n\nKiln-Run: run1\n") {
		t.Errorf("the titled commit's message is wrong:\n%s", raw)
	}
	if ok, err := isSaveOf(ctx, repo, titled, commit, "run1"); err != nil || !ok {
		t.Errorf("isSaveOf of the titled save = %v, %v", ok, err)
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

func TestSavedAs(t *testing.T) {
	repo, commit := bareRepo(t, map[string]string{"NOTES.md": "notes\n"})
	ctx := t.Context()
	mktree := func(name string) string {
		blob := gitIn(t, repo, "hash-object", "-w", "/dev/null")
		cmd := exec.Command("git", "--git-dir="+repo, "mktree")
		cmd.Stdin = strings.NewReader("100644 blob " + blob + "\t" + name + "\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	tree, other := mktree("NEW.md"), mktree("OTHER.md")
	end := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	save := func(tree, parent, id, title string) string {
		t.Helper()
		c, err := commitSave(ctx, repo, tree, parent, id, title, end)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	// The save this core would make now, with the title.
	if now := save(tree, commit, "run1", "feat: x"); now == "" {
		t.Fatal("no save")
	}
	elsewhere := save(other, commit, "run1", "")
	for _, c := range []struct {
		what, at string
		want     bool
	}{
		{"the same save with another message", save(tree, commit, "run1", ""), true},
		{"another tree", elsewhere, false},
		{"a save of run2", save(tree, commit, "run2", ""), false},
		{"another parent", save(tree, elsewhere, "run1", ""), false},
	} {
		if got, err := savedAs(ctx, repo, c.at, commit, "run1", tree); err != nil || got != c.want {
			t.Errorf("%s: savedAs = %v, %v; want %v", c.what, got, err, c.want)
		}
	}
}

func TestCheckFlags(t *testing.T) {
	good := Inputs{Repo: "r", Commit: "c", Branch: "b", ID: "run1", Instruction: "i", Runs: "d",
		Image: "kiln-run", Proxy: "kiln-proxy", Model: "claude-haiku-5-5", Effort: "low"}
	if err := checkFlags(good); err != nil {
		t.Fatalf("good inputs: %v", err)
	}
	titled := good
	titled.Title = "feat: x"
	if err := checkFlags(titled); err != nil {
		t.Errorf("the title %q: %v", titled.Title, err)
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
		{"a title with a newline", func(in *Inputs) { in.Title = "feat: x\nKiln-Run: other" }},
		{"a title of 257 bytes", func(in *Inputs) { in.Title = strings.Repeat("a", 257) }},
	} {
		in := good
		c.edit(&in)
		if err := checkFlags(in); err == nil {
			t.Errorf("%s: no error", c.what)
		}
	}
}

func TestCommand(t *testing.T) {
	r := &run{Inputs: Inputs{Model: "claude-haiku-5-5", Effort: "low"}, tree: "aaa", parentTree: "bbb"}
	got := r.command()
	if len(got) < 3 || got[0] != "sh" || got[1] != "-c" || got[2] != baselineScript+"exec claude \"$@\" < /kiln/instruction.md\n" {
		t.Fatalf("the command is %q", got)
	}
	want := `sh /work /kiln/base.pack aaa bbb -p --output-format stream-json --verbose --permission-mode bypassPermissions --model claude-haiku-5-5 --effort low`
	if g := strings.Join(got[3:], " "); g != want {
		t.Errorf("the command's arguments are\n%s\nwant\n%s", g, want)
	}
	r.parentTree = ""
	if g := strings.Join(r.command()[3:], " "); !strings.HasPrefix(g, "sh /work /kiln/base.pack aaa  -p ") {
		t.Errorf("without a parent the fourth argument is empty: %q", g)
	}
}

// TestBaseline runs the baseline script on a placed tree, with a global
// configuration whose core.hooksPath holds hooks that leave a marker, to see
// that none runs.
func TestBaseline(t *testing.T) {
	repo, commit := bareRepo(t, map[string]string{
		".gitignore":        "build/\n",
		"build/tracked.txt": "tracked under an ignored path\n",
		".gitattributes":    "kept.md export-ignore\n",
		"kept.md":           "kept\n",
		"link":              "->NOTES.md",
		"bin/tool.sh":       "echo hi\n",
		"NOTES.md":          "# Notes\n",
	}, "bin/tool.sh")
	// A parent: main as it was, with the start commit on top of it.
	tree := gitIn(t, repo, "rev-parse", commit+"^{tree}")
	entries, err := listTree(t.Context(), repo, commit)
	if err != nil {
		t.Fatal(err)
	}
	tarData, err := placingTar(t.Context(), repo, entries)
	if err != nil {
		t.Fatal(err)
	}
	// The other tree of the parent: the start's with one file less.
	idx := filepath.Join(t.TempDir(), "index")
	gitEnvIdx := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"--git-dir=" + repo}, args...)...)
		cmd.Env = append(gitEnv(), "GIT_INDEX_FILE="+idx, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitEnvIdx("read-tree", tree)
	gitEnvIdx("rm", "--cached", "--quiet", "kept.md")
	parentTree := gitEnvIdx("write-tree")

	hooks := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")
	for _, h := range []string{"reference-transaction", "post-index-change"} {
		writeIn(t, hooks, h, "#!/bin/sh\ntouch "+marker+"\n", 0o755)
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	writeIn(t, filepath.Dir(global), filepath.Base(global), "[core]\n\thooksPath = "+hooks+"\n", 0o644)

	// baseline places the tree in a fresh directory and runs the script there.
	baseline := func(t *testing.T, parent string, pre func(dir string)) (dir string, stdout string, err error) {
		t.Helper()
		dir = t.TempDir()
		untar := exec.Command("tar", "-x", "-C", dir)
		untar.Stdin = bytes.NewReader(tarData)
		if out, err := untar.CombinedOutput(); err != nil {
			t.Fatalf("tar: %v\n%s", err, out)
		}
		trees := []string{tree}
		if parent != "" {
			trees = append(trees, parent)
		}
		pack, err := basePack(t.Context(), repo, trees...)
		if err != nil {
			t.Fatal(err)
		}
		packFile := filepath.Join(t.TempDir(), "base.pack")
		if err := os.WriteFile(packFile, pack, 0o644); err != nil {
			t.Fatal(err)
		}
		if pre != nil {
			pre(dir)
		}
		cmd := exec.Command("sh", "-c", baselineScript, "sh", dir, packFile, tree, parent)
		cmd.Env = append(gitEnv(), "GIT_CONFIG_GLOBAL="+global, "GIT_CONFIG_NOSYSTEM=1")
		var out bytes.Buffer
		cmd.Stdout = &out
		var errOut bytes.Buffer
		cmd.Stderr = &errOut
		err = cmd.Run()
		if err != nil {
			err = fmt.Errorf("%w: %s", err, errOut.String())
		}
		return dir, out.String(), err
	}
	// in runs git in the placed directory, with the same global configuration.
	in := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(gitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimRight(string(out), "\n")
	}
	// The commit that commit-tree gives on the host, with the same identity.
	hostCommit := func(t *testing.T, tree, message string, parent string) string {
		t.Helper()
		args := []string{"commit-tree"}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		cmd := gitCommand(t.Context(), repo, append(args, "-m", message, tree)...)
		cmd.Env = append(gitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=kiln", "GIT_AUTHOR_EMAIL=kiln@invalid", "GIT_AUTHOR_DATE=@0 +0000",
			"GIT_COMMITTER_NAME=kiln", "GIT_COMMITTER_EMAIL=kiln@invalid", "GIT_COMMITTER_DATE=@0 +0000")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}

	t.Run("one commit holding exactly the tree given", func(t *testing.T) {
		dir, stdout, err := baseline(t, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if stdout != "" {
			t.Errorf("the script wrote to standard output: %q", stdout)
		}
		if got := in(t, dir, "rev-parse", "HEAD^{tree}"); got != tree {
			t.Errorf("the tree is %s, want %s", got, tree)
		}
		want := hostCommit(t, tree, "Where the work stood when this run began", "")
		if got := in(t, dir, "rev-parse", "HEAD"); got != want {
			t.Errorf("HEAD is %s, want %s", got, want)
		}
		if got := in(t, dir, "symbolic-ref", "HEAD"); got != "refs/heads/start" {
			t.Errorf("HEAD names %s", got)
		}
		if got := in(t, dir, "rev-list", "--count", "HEAD"); got != "1" {
			t.Errorf("%s commits, want 1", got)
		}
		if got := in(t, dir, "remote"); got != "" {
			t.Errorf("remotes: %q", got)
		}
		if _, err := os.Stat(filepath.Join(dir, ".git", "hooks")); err == nil {
			t.Error(".git/hooks exists")
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("a hook ran")
		}
		if got := in(t, dir, "status", "--porcelain"); got != "" {
			t.Errorf("git status after the baseline: %q", got)
		}

		writeIn(t, dir, "NOTES.md", "# Notes\nmore\n", 0o644)
		writeIn(t, dir, "build/new.txt", "ignored\n", 0o644)
		if got := in(t, dir, "status", "--porcelain"); got != " M NOTES.md" {
			t.Errorf("git status after a change: %q", got)
		}
		if got := in(t, dir, "diff", "--numstat"); got != "1\t0\tNOTES.md" {
			t.Errorf("git diff --numstat: %q", got)
		}
		if got := in(t, dir, "show", "HEAD:NOTES.md"); got != "# Notes" {
			t.Errorf("git show HEAD:NOTES.md: %q", got)
		}

		dir2, _, err := baseline(t, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if a, b := in(t, dir, "rev-parse", "HEAD"), in(t, dir2, "rev-parse", "HEAD"); a != b {
			t.Errorf("two baselines of the same tree: %s and %s", a, b)
		}
	})

	t.Run("a parent where the card's branch left main", func(t *testing.T) {
		dir, stdout, err := baseline(t, parentTree, nil)
		if err != nil {
			t.Fatal(err)
		}
		if stdout != "" {
			t.Errorf("the script wrote to standard output: %q", stdout)
		}
		parent := hostCommit(t, parentTree, "Where the card branch left main", "")
		want := hostCommit(t, tree, "Where the work stood when this run began", parent)
		if got := in(t, dir, "rev-parse", "HEAD"); got != want {
			t.Errorf("HEAD is %s, want %s", got, want)
		}
		if got := in(t, dir, "rev-parse", "HEAD~1"); got != parent {
			t.Errorf("HEAD~1 is %s, want %s", got, parent)
		}
		if got := in(t, dir, "rev-list", "--count", "HEAD"); got != "2" {
			t.Errorf("%s commits, want 2", got)
		}
		if got := in(t, dir, "status", "--porcelain"); got != "" {
			t.Errorf("git status after the baseline: %q", got)
		}
		if got := in(t, dir, "diff", "--name-status", "HEAD~1"); got != "A\tkept.md" {
			t.Errorf("git diff HEAD~1 of an untouched tree: %q", got)
		}
		if got := in(t, dir, "show", "HEAD~1:NOTES.md"); got != "# Notes" {
			t.Errorf("git show HEAD~1:NOTES.md: %q", got)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("a hook ran")
		}
	})

	t.Run("a directory that has a .git fails", func(t *testing.T) {
		_, _, err := baseline(t, "", func(dir string) {
			in(t, dir, "init", "--quiet")
			in(t, dir, "-c", "user.name=x", "-c", "user.email=x@invalid", "commit", "--quiet", "--allow-empty", "--message", "x", "--no-gpg-sign")
		})
		if err == nil {
			t.Error("the script succeeded")
		}
	})
}

func TestPrintable(t *testing.T) {
	in := "a\x1b[2Kb\tc\nd\x07e\u009bf\x7fg\x00h"
	want := "a�[2Kb\tc\nd�e�f�g�h"
	if got := printable(in); got != want {
		t.Errorf("printable(%q) = %q, want %q", in, got, want)
	}

	// As the run's standard error comes, with a C1 character split across two
	// writes, and a byte that is not UTF-8.
	var out bytes.Buffer
	w := &printableWriter{w: &out}
	for _, part := range []string{"x\x1b]0;title\x07 \xc2", "\x9by \x9b z\t\n"} {
		if n, err := w.Write([]byte(part)); err != nil || n != len(part) {
			t.Fatalf("Write(%q) = %d, %v", part, n, err)
		}
	}
	if got, want := out.String(), "x�]0;title� �y � z\t\n"; got != want {
		t.Errorf("the writer passed on %q, want %q", got, want)
	}

	// An event's text, a tool's description and the model, as following prints
	// them.
	var printed bytes.Buffer
	f := &follower{out: &printed, model: "claude-haiku-5-5\u001b[2K", mismatch: func(string) {}}
	events := `{"type":"system","subtype":"init","model":"claude-haiku-5-5\u001b[2K","claude_code_version":"2.1.326\u0007"}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash\u001b]52;c;eA==\u0007","input":{"description":"Run\u001b[1A it"}},{"type":"text","text":"Done.\u001b[2K\u009b"}]}}
`
	if _, err := readEvents(strings.NewReader(events), f.see); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsFunc(printed.String(), func(r rune) bool { return isControl(r) && r != '\n' }) {
		t.Errorf("following printed a control character: %q", printed.String())
	}
	if !strings.Contains(printed.String(), "text: Done.�[2K�") {
		t.Errorf("following printed %q", printed.String())
	}
}

// A repo given by a relative path, as `kiln run --repo` allows, is found from
// the directory the caller is in.
// TestParentTreeOf: a repeat takes the parent tree from the container's
// label, which must be empty or the full ID of a tree of the repo.
func TestParentTreeOf(t *testing.T) {
	repo, commit := bareRepo(t, map[string]string{"NOTES.md": "notes\n"})
	tree, err := gitLine(t.Context(), repo, "rev-parse", commit+"^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := parentTreeOf(t.Context(), repo, ""); got != "" || err != nil {
		t.Errorf("no label: want \"\", got %q, %v", got, err)
	}
	if got, err := parentTreeOf(t.Context(), repo, tree); got != tree || err != nil {
		t.Errorf("the tree: want %s, got %q, %v", tree, got, err)
	}
	for _, label := range []string{commit, tree[:12], "HEAD", "--all", strings.Repeat("0", 40)} {
		if got, err := parentTreeOf(t.Context(), repo, label); err == nil {
			t.Errorf("the label %q: want an error, got %q", label, got)
		}
	}
}

func TestRelativeRepo(t *testing.T) {
	repo, _ := bareRepo(t, map[string]string{"NOTES.md": "notes\n"})
	t.Chdir(filepath.Dir(repo))
	got, err := gitLine(t.Context(), filepath.Base(repo), "rev-parse", "--is-bare-repository")
	if err != nil || got != "true" {
		t.Errorf("git on a relative repo: want true, got %q, %v", got, err)
	}
}

func TestSymbolicBranch(t *testing.T) {
	repo, commit := bareRepo(t, map[string]string{"NOTES.md": "notes\n"})
	gitIn(t, repo, "update-ref", "refs/heads/main", commit)
	gitIn(t, repo, "symbolic-ref", "refs/heads/alias", "refs/heads/main")
	instruction := filepath.Join(t.TempDir(), "i.md")
	writeIn(t, filepath.Dir(instruction), "i.md", "Do it.\n", 0o644)
	in := Inputs{Repo: repo, Commit: commit, Branch: "alias", ID: "run1", Instruction: instruction,
		Runs: t.TempDir(), Image: "kiln-run", Proxy: "kiln-proxy", Model: "claude-haiku-5-5", Effort: "low"}
	if _, err := check(t.Context(), in); err == nil || !strings.Contains(err.Error(), "symbolic ref") {
		t.Errorf("check with a symbolic branch: want a refusal, got %v", err)
	}
	in.Branch = "main"
	if _, err := check(t.Context(), in); err != nil {
		t.Errorf("check with a plain branch: %v", err)
	}

	// update-ref never moves main through the symbolic ref.
	other := gitIn(t, repo, "commit-tree", "-p", commit, "-m", "other", gitIn(t, repo, "rev-parse", commit+"^{tree}"))
	moveBranch(t.Context(), repo, "refs/heads/alias", other, commit)
	if at := gitIn(t, repo, "rev-parse", "refs/heads/main"); at != commit {
		t.Errorf("main is at %s, was %s", at, commit)
	}
}

// repeated reads one byte over and over.
type repeated byte

func (r repeated) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func TestLongLine(t *testing.T) {
	const limit = 1 << 10
	const long = 64 << 20 // far over the limit
	first := `{"type":"system","subtype":"init","model":"claude-haiku-5-5"}` + "\n"
	last := `{"type":"result","subtype":"success","num_turns":2}` + "\n"
	output := func(line int64) io.Reader {
		return io.MultiReader(strings.NewReader(first), io.LimitReader(repeated('x'), line),
			strings.NewReader("\n"+last))
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	s, err := readEventsUpTo(output(long), nil, limit)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if s.model != "claude-haiku-5-5" || s.result == nil || s.result.Subtype != "success" {
		t.Errorf("the events around the long line were not read: %+v", s)
	}
	if s.tooLong != 1 {
		t.Errorf("%d lines were passed over, want 1", s.tooLong)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Errorf("reading a line of %d bytes took %d bytes of memory", long, grew)
	}

	// A line at the limit is read; one without a newline at the end is counted.
	atLimit := `{"type":"result","subtype":"success"}`
	atLimit += strings.Repeat(" ", limit-len(atLimit)-1) + "\n"
	if s, err := readEventsUpTo(strings.NewReader(atLimit), nil, limit); err != nil || s.result == nil || s.tooLong != 0 {
		t.Errorf("a line at the limit: %+v, %v", s, err)
	}
	if s, err := readEventsUpTo(io.MultiReader(strings.NewReader(first), io.LimitReader(repeated('x'), 4*limit)), nil, limit); err != nil ||
		s.model == "" || s.tooLong != 1 {
		t.Errorf("a long line at the end: %+v, %v", s, err)
	}
}
