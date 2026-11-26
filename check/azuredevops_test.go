package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The tests of the shipped Azure DevOps commands, scripts/azure-devops/. They
// run each command with stand-ins for az and gitleaks first on the PATH, the
// real git and jq, and a bare repo reached as an Azure DevOps address through
// Git's insteadOf, so that nothing reaches the network.

// adoURL is the stand-in host's repo, as the copy's origin has it.
const adoURL = "https://dev.azure.com/org/My%20Project/_git/repo"

// adoCommit is the published commit of the check result command's tests.
var adoCommit = strings.Repeat("ab", 20)

// The ids of the check result command's pull request.
const (
	adoCreator   = "6f1c2a3b-1d2e-4f50-8a6b-7c8d9e0f1a2b"
	adoReviewer  = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	adoRepoID    = "11111111-2222-4333-8444-555555555555"
	adoProjectID = "66666666-7777-4888-9999-aaaaaaaaaaaa"
)

// The stand-in az: it records each call's arguments, NUL-separated, one file
// per call, and answers from the file of the call's key, or fails with the
// standard error of the key's .fail file. Any other call exits 3.
const azStandIn = `#!/bin/sh
log=%s
answers=%s
n=$(ls "$log" | wc -l | tr -d ' ')
printf '%%s\0' "$@" >"$log/$(printf '%%04d' "$n")"
status= branch= resource= build= logid= pr= id= prev=
for a in "$@"; do
  case "$prev" in
    --status) status=$a ;;
    --source-branch) branch=1 ;;
    --resource) resource=$a ;;
    --id) id=$a ;;
  esac
  case "$a" in
    buildId=*) build=${a#buildId=} ;;
    logId=*) logid=${a#logId=} ;;
    pullRequestId=*) pr=${a#pullRequestId=} ;;
  esac
  prev=$a
done
key=
case "$1 $2 $3" in
  "extension show "*) key=extension ;;
  "repos pr list") if [ -n "$branch" ]; then key=list-branch; else key=list-$status; fi ;;
  "repos pr create") key=create ;;
  "repos pr update") key=update ;;
  "repos pr work-item") [ "$4" = add ] && key=work-item-add ;;
  "repos pr policy") [ "$4" = list ] && key=policy-$id ;;
  "devops invoke "*)
    case "$resource" in
      pullRequestThreads) key=threads-$pr ;;
      timeline) key=timeline-$build ;;
      logs) key=logs-$build-$logid ;;
    esac ;;
esac
[ -n "$key" ] || exit 3
if [ -f "$answers/$key.fail" ]; then cat "$answers/$key.fail" >&2; exit 1; fi
[ -f "$answers/$key" ] || exit 3
cat "$answers/$key"
`

// The stand-in gitleaks: it saves what it read and flags LEAK-MARKER.
const gitleaksStandIn = `#!/bin/sh
[ "$1" = stdin ] || exit 3
cat >%s
grep -q LEAK-MARKER %s && exit 1
exit 0
`

// ado is one test's world: the stand-ins, the host, the copy and the data
// directory.
type ado struct {
	t                                                   *testing.T
	dir, bin, answers, log, leaks, host, copy, data, pa string
}

func newADO(t *testing.T) *ado {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("jq is needed; the toolchain image has it")
	}
	dir := t.TempDir()
	e := &ado{t: t, dir: dir, bin: filepath.Join(dir, "bin"), answers: filepath.Join(dir, "answers"),
		log: filepath.Join(dir, "log"), leaks: filepath.Join(dir, "gitleaks-read"),
		host: filepath.Join(dir, "host", "org", "My Project", "_git", "repo"),
		copy: filepath.Join(dir, "copy.git"), data: filepath.Join(dir, "data")}
	for _, d := range []string{e.bin, e.answers, e.log, e.data, filepath.Join(dir, "home")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.write(filepath.Join(e.bin, "az"), fmt.Sprintf(azStandIn, quote(e.log), quote(e.answers)), 0o755)
	e.write(filepath.Join(e.bin, "gitleaks"), fmt.Sprintf(gitleaksStandIn, quote(e.leaks), quote(e.leaks)), 0o755)
	e.pa = e.bin + string(os.PathListSeparator) + os.Getenv("PATH")
	e.answer("extension", "{}")

	e.git("", "", "init", "--quiet", "--bare", "--initial-branch=main", e.host)
	main := e.commit(e.host, "", "hello\n", "initial")
	e.git(e.host, "", "update-ref", "refs/heads/main", main)
	e.git("", "", "clone", "--quiet", "--bare", adoURL, e.copy)
	if got := e.git(e.copy, "", "config", "--get", "remote.origin.url"); got != adoURL {
		t.Fatalf("the copy's origin is %q", got)
	}
	return e
}

// quote quotes a path for sh.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (e *ado) write(path, content string, mode os.FileMode) {
	e.t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		e.t.Fatal(err)
	}
}

// answer sets what the stand-in az prints for a key.
func (e *ado) answer(key, body string) { e.write(filepath.Join(e.answers, key), body, 0o644) }

// answerJSON sets the answer for a key to a value as JSON.
func (e *ado) answerJSON(key string, v any) {
	e.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		e.t.Fatal(err)
	}
	e.answer(key, string(b))
}

// refuse makes the stand-in az fail for a key, with a standard error.
func (e *ado) refuse(key, stderr string) {
	e.write(filepath.Join(e.answers, key+".fail"), stderr, 0o644)
}

// env is the environment of git and of the commands.
func (e *ado) env(path string) []string {
	return []string{
		"PATH=" + path,
		"HOME=" + filepath.Join(e.dir, "home"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url.file://" + filepath.Join(e.dir, "host") + "/.insteadOf",
		"GIT_CONFIG_VALUE_0=https://dev.azure.com/",
		"GIT_AUTHOR_NAME=Kiln", "GIT_AUTHOR_EMAIL=kiln@example.invalid",
		"GIT_COMMITTER_NAME=Kiln", "GIT_COMMITTER_EMAIL=kiln@example.invalid",
	}
}

// git runs git in repo, with input, and returns its output trimmed.
func (e *ado) git(repo, input string, args ...string) string {
	e.t.Helper()
	if repo != "" {
		args = append([]string{"-C", repo}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = e.env(os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit makes a commit in repo of one file, file.txt, with the content.
func (e *ado) commit(repo, parent, content, message string) string {
	e.t.Helper()
	blob := e.git(repo, content, "hash-object", "-w", "--stdin")
	tree := e.git(repo, "100644 blob "+blob+"\tfile.txt\n", "mktree")
	args := []string{"commit-tree", tree, "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return e.git(repo, "", args...)
}

// card puts a card's commit, with main as its parent, under
// refs/kiln/publish/card-7 of the copy.
func (e *ado) card(content, message string) string {
	e.t.Helper()
	c := e.commit(e.copy, e.git(e.copy, "", "rev-parse", "refs/heads/main"), content, message)
	e.git(e.copy, "", "update-ref", "refs/kiln/publish/card-7", c)
	return c
}

// without is a PATH with the stand-ins and every program of the PATH but
// name, by links in a directory of their own.
func (e *ado) without(name string) string {
	e.t.Helper()
	if err := os.Remove(filepath.Join(e.bin, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.t.Fatal(err)
	}
	links := filepath.Join(e.dir, "without-"+name)
	if err := os.MkdirAll(links, 0o755); err != nil {
		e.t.Fatal(err)
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		entries, _ := os.ReadDir(d)
		for _, entry := range entries {
			if entry.Name() == name {
				continue
			}
			target := filepath.Join(links, entry.Name())
			if _, err := os.Lstat(target); err == nil {
				continue
			}
			if info, err := os.Stat(filepath.Join(d, entry.Name())); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				if err := os.Symlink(filepath.Join(d, entry.Name()), target); err != nil {
					e.t.Fatal(err)
				}
			}
		}
	}
	return e.bin + string(os.PathListSeparator) + links
}

// result is what a command did.
type result struct {
	stdout, stderr string
	code           int
}

// last is the last line of the standard output.
func (r result) last() string {
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	return lines[len(lines)-1]
}

// run runs a shipped command in the data directory, with the environment and
// the extra variables.
func (e *ado) run(name, path string, extra []string, args ...string) result {
	e.t.Helper()
	script, err := filepath.Abs(filepath.Join("..", scriptsDir, "azure-devops", name))
	if err != nil {
		e.t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Dir = e.data
	cmd.Env = append(e.env(path), extra...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	r := result{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		r.code = exit.ExitCode()
	case err != nil:
		e.t.Fatal(err)
	}
	return r
}

// calls are the stand-in az's calls, in order.
func (e *ado) calls() [][]string {
	e.t.Helper()
	entries, err := os.ReadDir(e.log)
	if err != nil {
		e.t.Fatal(err)
	}
	var calls [][]string
	for _, entry := range entries {
		b, err := os.ReadFile(filepath.Join(e.log, entry.Name()))
		if err != nil {
			e.t.Fatal(err)
		}
		calls = append(calls, strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"))
	}
	return calls
}

// call is the first call whose first words are these, or nil.
func (e *ado) call(words ...string) []string {
	for _, c := range e.calls() {
		if len(c) >= len(words) && strings.Join(c[:len(words)], " ") == strings.Join(words, " ") {
			return c
		}
	}
	return nil
}

// has says whether args holds each of want in a row: "--option value", an
// option without = and its value, is two arguments, anything else one.
func has(args []string, want ...string) bool {
	joined := "\x00" + strings.Join(args, "\x00") + "\x00"
	for _, w := range want {
		if strings.HasPrefix(w, "--") && !strings.Contains(w, "=") {
			w = strings.Replace(w, " ", "\x00", 1)
		}
		if !strings.Contains(joined, "\x00"+w+"\x00") {
			return false
		}
	}
	return true
}

// snapshot is every file's path, size, mode and modification time under dir.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %d %v %d\n", p, info.Size(), info.Mode(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The check result command's data.

type obj = map[string]any

func pullRequest(id int, commit, status string) obj {
	return obj{"pullRequestId": id, "status": status,
		"lastMergeSourceCommit": obj{"commitId": commit},
		"createdBy":             obj{"id": adoCreator, "displayName": "Kiln Publisher"},
		"repository":            obj{"id": adoRepoID, "project": obj{"id": adoProjectID}}}
}

func comment(author, name, date, text string) obj {
	return obj{"id": 1, "author": obj{"id": author, "displayName": name}, "content": text,
		"publishedDate": date, "commentType": "text", "isDeleted": false}
}

func thread(date string, comments ...obj) obj {
	list := []any{}
	for _, c := range comments {
		list = append(list, c)
	}
	return obj{"id": 1, "publishedDate": date, "isDeleted": false, "comments": list, "properties": obj{}}
}

func vote(voter, result, date string) obj {
	str := func(v string) obj { return obj{"$type": "System.String", "$value": v} }
	return obj{"id": 2, "publishedDate": date, "isDeleted": false,
		"comments": []any{obj{"id": 1, "author": obj{"id": voter, "displayName": "Bo Voter"},
			"content": "Bo Voter voted " + result, "publishedDate": date, "commentType": "system"}},
		"properties": obj{"CodeReviewThreadType": str("VoteUpdate"), "CodeReviewVoteResult": str(result),
			"CodeReviewVotedByTfId": str(voter)}}
}

func threads(list ...obj) obj {
	value := []any{}
	for _, th := range list {
		value = append(value, th)
	}
	return obj{"count": len(value), "value": value}
}

func build(status, commit string, blocking bool, kind string) obj {
	return obj{"status": status,
		"configuration": obj{"isBlocking": blocking, "type": obj{"displayName": kind}},
		"context":       obj{"buildId": 101, "lastMergeSourceCommitId": commit}}
}

// adoSince is the feedback mark of the check result command's tests.
const adoSince = "2026-11-26T10:00:00Z"

// checkWorld is a world with an active pull request 17 for adoCommit, beside
// one for another commit, no feedback and an approved build.
func checkWorld(t *testing.T) *ado {
	e := newADO(t)
	e.answerJSON("list-active", []any{pullRequest(16, strings.Repeat("cd", 20), "active"), pullRequest(17, adoCommit, "active")})
	e.answerJSON("threads-17", threads())
	e.answerJSON("policy-17", []any{build("approved", adoCommit, true, "Build")})
	return e
}

func (e *ado) check(extra ...string) result {
	e.t.Helper()
	return e.runCheck(e.pa, append([]string{"KILN_COMMIT=" + adoCommit, "KILN_SINCE=" + adoSince}, extra...), e.copy)
}

func (e *ado) runCheck(path string, env []string, args ...string) result {
	e.t.Helper()
	return e.run("check-result", path, env, args...)
}

func expect(t *testing.T, r result, code int, stdout string) {
	t.Helper()
	if r.code != code || r.stdout != stdout {
		t.Errorf("exit %d, standard output %q, standard error %q; want exit %d and %q", r.code, r.stdout, r.stderr, code, stdout)
	}
}

func TestAzureDevOpsCheckResult(t *testing.T) {
	t.Run("no pull request", func(t *testing.T) {
		e := newADO(t)
		e.answerJSON("list-active", []any{})
		e.answerJSON("list-all", []any{pullRequest(16, strings.Repeat("cd", 20), "abandoned")})
		expect(t, e.check(), 0, "pending\n")
	})

	t.Run("passed and pending by the policies", func(t *testing.T) {
		for _, c := range []struct {
			name     string
			policies []any
			want     string
		}{
			{"approved", []any{build("approved", adoCommit, true, "Build")}, "passed"},
			{"not applicable", []any{build("notApplicable", adoCommit, true, "Build")}, "passed"},
			{"another commit", []any{build("approved", strings.Repeat("cd", 20), true, "Build")}, "pending"},
			{"queued", []any{build("approved", adoCommit, true, "Build"), build("queued", adoCommit, true, "Build")}, "pending"},
			{"no blocking build", []any{build("rejected", adoCommit, false, "Build"), build("rejected", adoCommit, true, "Minimum number of reviewers")}, "passed"},
		} {
			t.Run(c.name, func(t *testing.T) {
				e := checkWorld(t)
				e.answerJSON("policy-17", c.policies)
				expect(t, e.check(), 0, c.want+"\n")
			})
		}
	})

	t.Run("failed with the log", func(t *testing.T) {
		e := checkWorld(t)
		e.answerJSON("policy-17", []any{build("rejected", adoCommit, true, "Build")})
		e.answerJSON("timeline-101", obj{"records": []any{
			obj{"type": "Task", "result": "succeeded", "name": "Build", "log": obj{"id": 6}},
			obj{"type": "Task", "result": "failed", "name": "Run tests", "log": obj{"id": 7},
				"issues": []any{obj{"type": "warning", "message": "slow"}, obj{"type": "error", "message": "3 tests failed"}}},
		}})
		e.answerJSON("logs-101-7", obj{"count": 2, "value": []any{
			"2026-11-26T10:00:00.1234567Z Running tests", "2026-11-26T10:00:01.0000000Z FAIL: TestX"}})
		expect(t, e.check(), 0, "=== build 101, failed task: Run tests\nRunning tests\nFAIL: TestX\n"+
			"=== what failed:\n- build 101, Run tests: 3 tests failed\nfailed\n")
	})

	t.Run("closed", func(t *testing.T) {
		e := newADO(t)
		e.answerJSON("list-active", []any{pullRequest(16, strings.Repeat("cd", 20), "active")})
		e.answerJSON("list-all", []any{pullRequest(17, adoCommit, "abandoned"), pullRequest(18, adoCommit, "abandoned")})
		expect(t, e.check(), 0, "closed\n")
		e.answerJSON("list-all", []any{pullRequest(17, adoCommit, "abandoned"), pullRequest(18, adoCommit, "completed")})
		expect(t, e.check(), 0, "pending\n")
	})

	t.Run("review", func(t *testing.T) {
		e := checkWorld(t)
		line := thread("2026-11-26T10:00:01.25Z", comment(adoReviewer, "Ada Reviewer", "2026-11-26T10:00:01.25Z", "Please rename this."))
		line["threadContext"] = obj{"filePath": "/src/a.go", "rightFileStart": obj{"line": 12, "offset": 1}}
		left := thread("2026-11-26T10:00:00.5Z", comment(adoReviewer, "Ada Reviewer", "2026-11-26T10:00:00.5Z", "Why?"))
		left["threadContext"] = obj{"filePath": "/src/b.go", "leftFileStart": obj{"line": 3, "offset": 1}}
		e.answerJSON("threads-17", threads(line, left))
		e.answerJSON("policy-17", []any{build("rejected", adoCommit, true, "Build")})
		expect(t, e.check(), 0, "=== Ada Reviewer, /src/b.go:3, 2026-11-26T10:00:00.5Z\nWhy?\n"+
			"=== Ada Reviewer, /src/a.go:12, 2026-11-26T10:00:01.25Z\nPlease rename this.\nreview\n")

		for _, v := range []struct{ result, text string }{{"-5", "waiting for author"}, {"-10", "rejected"}} {
			e.answerJSON("threads-17", threads(vote(adoReviewer, v.result, "2026-11-26T10:00:02.5Z")))
			expect(t, e.check(), 0, "=== Bo Voter, 2026-11-26T10:00:02.5Z\nvoted: "+v.text+"\nreview\n")
		}
	})

	t.Run("not review", func(t *testing.T) {
		own := strings.ToUpper(strings.ReplaceAll(adoCreator, "-", ""))
		deletedComment := comment(adoReviewer, "Ada Reviewer", "2026-11-26T10:00:01Z", "gone")
		deletedComment["isDeleted"] = true
		deletedThread := thread("2026-11-26T10:00:01Z", comment(adoReviewer, "Ada Reviewer", "2026-11-26T10:00:01Z", "gone"))
		deletedThread["isDeleted"] = true
		system := comment(adoReviewer, "Ada Reviewer", "2026-11-26T10:00:01Z", "Ada Reviewer joined")
		system["commentType"] = "system"
		for _, c := range []struct {
			name   string
			thread obj
		}{
			{"before KILN_SINCE", thread("2026-11-26T09:59:59.999Z", comment(adoReviewer, "Ada Reviewer", "2026-11-26T09:59:59.999Z", "old"))},
			{"the creator's own", thread("2026-11-26T10:00:01Z", comment(own, "Kiln Publisher", "2026-11-26T10:00:01Z", "mine"))},
			{"a deleted comment", thread("2026-11-26T10:00:01Z", deletedComment)},
			{"a deleted thread", deletedThread},
			{"a system comment", thread("2026-11-26T10:00:01Z", system)},
			{"a vote of 10", vote(adoReviewer, "10", "2026-11-26T10:00:01Z")},
			{"the creator's own -5", vote(adoCreator, "-5", "2026-11-26T10:00:01Z")},
		} {
			t.Run(c.name, func(t *testing.T) {
				e := checkWorld(t)
				e.answerJSON("threads-17", threads(c.thread))
				expect(t, e.check(), 0, "passed\n")
			})
		}
	})

	t.Run("failures", func(t *testing.T) {
		failing := func(t *testing.T, r result, reason string) {
			t.Helper()
			if r.code != 2 || strings.Contains(r.stdout, "passed") || !strings.Contains(r.stderr, reason) {
				t.Errorf("exit %d, standard output %q, standard error %q; want exit 2, no passed, and %q", r.code, r.stdout, r.stderr, reason)
			}
		}
		for _, key := range []string{"policy-17", "list-active", "threads-17"} {
			t.Run("a failing "+key, func(t *testing.T) {
				e := checkWorld(t)
				e.refuse(key, "ERROR: Please run 'az login' to setup account.")
				failing(t, e.check(), "is az signed in?")
			})
		}
		t.Run("a bad KILN_COMMIT", func(t *testing.T) {
			e := checkWorld(t)
			failing(t, e.runCheck(e.pa, []string{"KILN_COMMIT=" + strings.ToUpper(adoCommit), "KILN_SINCE=" + adoSince}, e.copy), "KILN_COMMIT")
		})
		t.Run("a bad KILN_SINCE", func(t *testing.T) {
			e := checkWorld(t)
			failing(t, e.runCheck(e.pa, []string{"KILN_COMMIT=" + adoCommit, "KILN_SINCE=2026-11-26 10:00:00"}, e.copy), "KILN_SINCE")
			failing(t, e.runCheck(e.pa, []string{"KILN_COMMIT=" + adoCommit}, e.copy), "KILN_SINCE")
		})
		t.Run("no argument", func(t *testing.T) {
			e := checkWorld(t)
			failing(t, e.runCheck(e.pa, []string{"KILN_COMMIT=" + adoCommit, "KILN_SINCE=" + adoSince}), "usage: check-result")
		})
		t.Run("an origin not on Azure DevOps", func(t *testing.T) {
			e := checkWorld(t)
			e.git(e.copy, "", "config", "remote.origin.url", "https://example.invalid/org/repo.git")
			failing(t, e.check(), "not an Azure DevOps repository")
		})
		t.Run("no jq", func(t *testing.T) {
			e := checkWorld(t)
			failing(t, e.runCheck(e.without("jq"), []string{"KILN_COMMIT=" + adoCommit, "KILN_SINCE=" + adoSince}, e.copy), "jq is not on the PATH")
		})
		t.Run("no extension", func(t *testing.T) {
			e := checkWorld(t)
			e.refuse("extension", "ERROR: The extension azure-devops is not installed.")
			failing(t, e.check(), "az extension add --name azure-devops")
		})
	})

	t.Run("read only", func(t *testing.T) {
		e := checkWorld(t)
		before := snapshot(t, e.copy)
		e.answerJSON("threads-17", threads(thread("2026-11-26T10:00:01Z", comment(adoReviewer, "Ada Reviewer", "2026-11-26T10:00:01Z", "Hm."))))
		if r := e.check(); r.last() != "review" {
			t.Errorf("standard output %q, standard error %q", r.stdout, r.stderr)
		}
		e.answerJSON("threads-17", threads())
		e.answerJSON("policy-17", []any{build("rejected", adoCommit, true, "Build")})
		e.answerJSON("timeline-101", obj{"records": []any{obj{"type": "Task", "result": "failed", "name": "Run tests"}}})
		if r := e.check(); r.last() != "failed" {
			t.Errorf("standard output %q, standard error %q", r.stdout, r.stderr)
		}
		for _, c := range e.calls() {
			for _, word := range []string{"create", "update", "set-vote", "work-item"} {
				if has(c, word) {
					t.Errorf("az was called with %s: %q", word, c)
				}
			}
		}
		if after := snapshot(t, e.copy); after != before {
			t.Errorf("the copy changed:\n%s\nwas\n%s", after, before)
		}
	})
}

// The publish command's data.

const (
	adoTitle  = "feat(x): add a thing"
	adoBranch = "card/7-add-a-thing"
	adoText   = "## Why the change\n\nIt adds a thing.\n"
)

// publish runs the publish command for card 7 and its commit, with the text
// file holding text and the extra variables last.
func (e *ado) publish(path, commit, title, text string, extra ...string) result {
	e.t.Helper()
	textFile := filepath.Join(e.data, "publish", "card-7.md")
	if err := os.MkdirAll(filepath.Dir(textFile), 0o755); err != nil {
		e.t.Fatal(err)
	}
	e.write(textFile, text, 0o644)
	env := []string{"KILN_CARD=7", "KILN_REPO=" + e.copy, "KILN_COMMIT=" + commit, "KILN_BRANCH=" + adoBranch,
		"KILN_TITLE=" + title, "KILN_TEXT_FILE=" + textFile, "KILN_WORK_ITEM=42"}
	return e.run("publish", path, append(env, extra...))
}

// refs are the host's refs and the commits they point at.
func (e *ado) refs() string {
	return e.git(e.host, "", "for-each-ref", "--format=%(refname) %(objectname)")
}

const adoAddress = "https://dev.azure.com/org/My%20Project/_git/repo/pullrequest/17"

func TestAzureDevOpsPublish(t *testing.T) {
	published := func(t *testing.T, r result) {
		t.Helper()
		if r.code != 0 || r.last() != adoAddress {
			t.Errorf("exit %d, standard output %q, standard error %q; want exit 0 and the address last", r.code, r.stdout, r.stderr)
		}
	}
	refused := func(t *testing.T, r result, reasons ...string) {
		t.Helper()
		if r.code != 1 || strings.Contains(r.stdout, "https://") {
			t.Errorf("exit %d, standard output %q; want exit 1 and no address", r.code, r.stdout)
		}
		for _, reason := range reasons {
			if !strings.Contains(r.stderr, reason) {
				t.Errorf("standard error %q does not hold %q", r.stderr, reason)
			}
		}
	}

	t.Run("a new pull request", func(t *testing.T) {
		e := newADO(t)
		commit := e.card("hello\nthing\n", adoTitle+"\n\nKiln-Card: 7")
		before := snapshot(t, e.copy)
		e.answer("list-branch", "")
		e.answer("create", "17\n")
		published(t, e.publish(e.pa, commit, adoTitle, adoText))
		main := e.git(e.copy, "", "rev-parse", "refs/heads/main")
		if got, want := e.refs(), "refs/heads/feat/42-add-a-thing "+commit+"\nrefs/heads/main "+main; got != want {
			t.Errorf("the host's refs are\n%s\nwant\n%s", got, want)
		}
		create := e.call("repos", "pr", "create")
		if !has(create, "--draft true", "--work-items 42", "--target-branch main", "--source-branch feat/42-add-a-thing",
			"--title="+adoTitle, "--description="+strings.TrimRight(adoText, "\n"), "--project My Project", "--repository repo",
			"--organization https://dev.azure.com/org") {
			t.Errorf("pr create got %q", create)
		}
		if after := snapshot(t, e.copy); after != before {
			t.Errorf("the copy changed:\n%s\nwas\n%s", after, before)
		}
		if _, err := os.Stat(filepath.Join(e.data, "azure-devops", "org", "My%20Project", "repo", ".git")); err != nil {
			t.Errorf("the clone is not under the data directory: %v", err)
		}
	})

	t.Run("an existing pull request after main moved", func(t *testing.T) {
		e := newADO(t)
		first := e.card("hello\nthing\n", adoTitle)
		e.answer("list-branch", "")
		e.answer("create", "17\n")
		published(t, e.publish(e.pa, first, adoTitle, adoText))

		moved := e.commit(e.copy, e.git(e.copy, "", "rev-parse", "refs/heads/main"), "hello again\n", "main moved")
		e.git(e.copy, "", "update-ref", "refs/heads/main", moved)
		e.git(e.copy, "", "push", "--quiet", "origin", "refs/heads/main:refs/heads/main")
		second := e.card("hello again\nthing\n", adoTitle)
		if err := os.RemoveAll(e.log); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(e.log, 0o755); err != nil {
			t.Fatal(err)
		}
		e.answer("list-branch", "17\n")
		e.answer("update", "")
		e.answer("work-item-add", "")
		published(t, e.publish(e.pa, second, adoTitle, adoText))
		if got := e.git(e.host, "", "rev-parse", "refs/heads/feat/42-add-a-thing"); got != second {
			t.Errorf("the branch is %s, want %s", got, second)
		}
		if c := e.call("repos", "pr", "create"); c != nil {
			t.Errorf("pr create was called: %q", c)
		}
		if c := e.call("repos", "pr", "update"); !has(c, "--id 17", "--title="+adoTitle) {
			t.Errorf("pr update got %q", c)
		}
		if c := e.call("repos", "pr", "work-item", "add"); !has(c, "--id 17", "--work-items 42") {
			t.Errorf("pr work-item add got %q", c)
		}
	})

	t.Run("a refused work item", func(t *testing.T) {
		e := newADO(t)
		commit := e.card("hello\nthing\n", adoTitle)
		e.answer("list-branch", "")
		e.refuse("create", "ERROR: TF401180: the work item 42 cannot be linked.")
		refused(t, e.publish(e.pa, commit, adoTitle, adoText), "TF401180", "cannot open the pull request")

		e.answer("list-branch", "17\n")
		e.answer("update", "")
		e.refuse("work-item-add", "ERROR: TF401180: the work item 42 cannot be linked.")
		refused(t, e.publish(e.pa, commit, adoTitle, adoText), "TF401180", "cannot link work item 42")
	})

	t.Run("a gitleaks hit", func(t *testing.T) {
		for _, c := range []struct{ name, content, title, text string }{
			{"in the change", "hello\nLEAK-MARKER\n", adoTitle, adoText},
			{"in the title", "hello\nthing\n", adoTitle + " LEAK-MARKER", adoText},
			{"in the text", "hello\nthing\n", adoTitle, adoText + "LEAK-MARKER\n"},
		} {
			t.Run(c.name, func(t *testing.T) {
				e := newADO(t)
				commit := e.card(c.content, adoTitle+"\n\nmessage-marker")
				e.answer("list-branch", "")
				e.answer("create", "17\n")
				refused(t, e.publish(e.pa, commit, c.title, c.text), "gitleaks flags", "nothing was pushed")
				if got, want := e.refs(), "refs/heads/main "+e.git(e.host, "", "rev-parse", "refs/heads/main"); got != want {
					t.Errorf("the host's refs are\n%s", got)
				}
				for _, call := range e.calls() {
					if has(call, "repos") {
						t.Errorf("az was called: %q", call)
					}
				}
				read, err := os.ReadFile(e.leaks)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"+++ b/file.txt", "+" + strings.Split(c.content, "\n")[1], "message-marker", c.title, c.text} {
					if !strings.Contains(string(read), want) {
						t.Errorf("gitleaks did not read %q in %q", want, read)
					}
				}
			})
		}
	})

	t.Run("the cut", func(t *testing.T) {
		e := newADO(t)
		commit := e.card("hello\nthing\n", adoTitle)
		e.answer("list-branch", "")
		e.answer("create", "17\n")
		published(t, e.publish(e.pa, commit, adoTitle, "a"+strings.Repeat("é", 4000)))
		var description string
		for _, a := range e.call("repos", "pr", "create") {
			if d, ok := strings.CutPrefix(a, "--description="); ok {
				description = d
			}
		}
		if !utf8.ValidString(description) || len(description) > 3902 || len(description) < 3800 {
			t.Errorf("the description is %d bytes, valid UTF-8 %v", len(description), utf8.ValidString(description))
		}
	})

	t.Run("the type", func(t *testing.T) {
		for _, c := range []struct{ title, branch string }{
			{"fix(x): mend a thing", "fix/42-add-a-thing"},
			{"Update readme", "chore/42-add-a-thing"},
		} {
			e := newADO(t)
			commit := e.card("hello\nthing\n", c.title)
			e.answer("list-branch", "")
			e.answer("create", "17\n")
			published(t, e.publish(e.pa, commit, c.title, adoText))
			if got := e.git(e.host, "", "rev-parse", "refs/heads/"+c.branch); got != commit {
				t.Errorf("%q: the branch %s is %q", c.title, c.branch, got)
			}
		}
	})

	t.Run("repeats", func(t *testing.T) {
		e := newADO(t)
		commit := e.card("hello\nthing\n", adoTitle)
		leftover := filepath.Join(e.data, "azure-devops", "org", "My%20Project", "repo.tmp.99999")
		if err := os.MkdirAll(leftover, 0o755); err != nil {
			t.Fatal(err)
		}
		e.write(filepath.Join(leftover, "junk"), "junk", 0o644)
		e.answer("list-branch", "")
		e.answer("create", "17\n")
		published(t, e.publish(e.pa, commit, adoTitle, adoText))
		published(t, e.publish(e.pa, commit, adoTitle, adoText))
		e.answer("list-branch", "17\n")
		e.answer("update", "")
		e.answer("work-item-add", "")
		published(t, e.publish(e.pa, commit, adoTitle, adoText))
		if got := e.git(e.host, "", "rev-parse", "refs/heads/feat/42-add-a-thing"); got != commit {
			t.Errorf("the branch is %s, want %s", got, commit)
		}
	})

	t.Run("at once with another card", func(t *testing.T) {
		const otherURL = "https://dev.azure.com/org/My%20Project/_git/other"
		world := func(t *testing.T) (*ado, string, string) {
			e := newADO(t)
			commit := e.card("hello\nthing\n", adoTitle)
			e.answer("list-branch", "")
			e.answer("create", "17\n")
			published(t, e.publish(e.pa, commit, adoTitle, adoText))
			return e, commit, filepath.Join(e.data, "azure-devops", "org", "My%20Project", "repo")
		}
		lock := func(t *testing.T, clone string) string {
			path := filepath.Join(clone, ".git", "config.lock")
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			return path
		}
		stays := func(t *testing.T, path string) {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("the lock is gone: %v", err)
			}
		}

		t.Run("a held lock, the same origin", func(t *testing.T) {
			e, commit, clone := world(t)
			l := lock(t, clone)
			published(t, e.publish(e.pa, commit, adoTitle, adoText))
			stays(t, l)
		})
		t.Run("a held lock, another origin", func(t *testing.T) {
			e, commit, clone := world(t)
			e.git(clone, "", "config", "remote.origin.url", otherURL)
			l := lock(t, clone)
			start := time.Now()
			r := e.publish(e.pa, commit, adoTitle, adoText)
			refused(t, r, "cannot set the clone's origin", ".git/config.lock")
			if strings.Contains(r.stderr, "_git/") {
				t.Errorf("standard error %q holds the origin", r.stderr)
			}
			if time.Since(start) < time.Second {
				t.Errorf("the run took %v, want the retry's second", time.Since(start))
			}
			stays(t, l)
		})
		t.Run("another origin", func(t *testing.T) {
			e, commit, clone := world(t)
			e.git(clone, "", "config", "remote.origin.url", otherURL)
			published(t, e.publish(e.pa, commit, adoTitle, adoText))
			if got := e.git(clone, "", "config", "--get", "remote.origin.url"); got != adoURL {
				t.Errorf("the clone's origin is %q, want %q", got, adoURL)
			}
			if got := e.git(e.host, "", "rev-parse", "refs/heads/feat/42-add-a-thing"); got != commit {
				t.Errorf("the branch is %s, want %s", got, commit)
			}
		})
		t.Run("a clone made between the check and the move", func(t *testing.T) {
			e := newADO(t)
			commit := e.card("hello\nthing\n", adoTitle)
			e.answer("list-branch", "")
			e.answer("create", "17\n")
			realMv, err := exec.LookPath("mv")
			if err != nil {
				t.Fatal(err)
			}
			e.write(filepath.Join(e.bin, "mv"), "#!/bin/sh\ncp -R \"$1\" \"$2\" || exit 1\nexec "+quote(realMv)+" \"$@\"\n", 0o755)
			published(t, e.publish(e.pa, commit, adoTitle, adoText))
			clone := filepath.Join(e.data, "azure-devops", "org", "My%20Project", "repo")
			entries, err := os.ReadDir(clone)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, d := range entries {
				names = append(names, d.Name())
			}
			if strings.Join(names, " ") != ".git" {
				t.Errorf("the clone holds %v, want only .git", names)
			}
		})
	})

	t.Run("failures", func(t *testing.T) {
		t.Run("no work item", func(t *testing.T) {
			e := newADO(t)
			commit := e.card("hello\nthing\n", adoTitle)
			refused(t, e.publish(e.pa, commit, adoTitle, adoText, "KILN_WORK_ITEM="), "KILN_WORK_ITEM is not set")
		})
		t.Run("inside Claude Code", func(t *testing.T) {
			e := newADO(t)
			commit := e.card("hello\nthing\n", adoTitle)
			refused(t, e.publish(e.pa, commit, adoTitle, adoText, "CLAUDECODE=1"), "Claude Code")
		})
		t.Run("no gitleaks", func(t *testing.T) {
			e := newADO(t)
			commit := e.card("hello\nthing\n", adoTitle)
			refused(t, e.publish(e.without("gitleaks"), commit, adoTitle, adoText), "gitleaks is not on the PATH")
		})
		t.Run("an origin on visualstudio.com", func(t *testing.T) {
			e := newADO(t)
			commit := e.card("hello\nthing\n", adoTitle)
			e.git(e.copy, "", "config", "remote.origin.url", "https://org.visualstudio.com/My%20Project/_git/repo")
			refused(t, e.publish(e.pa, commit, adoTitle, adoText), "not an Azure DevOps repository")
		})
	})
}
