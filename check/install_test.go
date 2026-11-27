package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The fresh-install test's names. The instance's own names are fixed: the
// proxy kiln-proxy on the network kiln, the kiln-* images and the screen on
// port 4747 (cmd/core).
const (
	installUpstream = "kiln-install-upstream" // the scripted upstream's image and container
	installMarker   = "kiln-marker-install"
	installCase     = "kiln-case-install"
	installScreen   = "http://127.0.0.1:4747"
	installNetwork  = "kiln"
	installProxy    = "kiln-proxy"
	// installRepo is the team repo's name, and installLine the line its
	// implement run adds to NOTES.md (internal/run/testdata/claude).
	installRepo = "team"
	installLine = "A line from the fresh install."
)

// installValues are the values TestFreshInstall writes for the guide's values
// block, by name, each given as the guide's own values file would: as shell.
// Their names must be exactly the block's. The upstream is internal/run's
// scripted upstream on the network the core makes, the credential a fake, and
// the hosts of a spec run a name that resolves nowhere, as in TestTick, so
// that nothing leaves the runner. The other values are filled in by the test.
func installValues(kilnGit, teamRepo, publish, checkResult string) [][2]string {
	return [][2]string{
		{"KILN", `"$HOME/kiln"`},
		{"KILN_GIT", shQuote(kilnGit)},
		{"TEAM_REPO", shQuote(teamRepo)},
		{"REPO_NAME", installRepo},
		{"LIMIT", "1"},
		{"WORK_ITEM", "required"},
		{"SPEC_HOSTS", "public.kiln-test.example"},
		{"PUBLISH", shQuote(publish)},
		{"CHECK_RESULT", shQuote(checkResult)},
		{"CLAUDE_UPSTREAM", "http://" + installUpstream + ":8081"},
		{"CLAUDE_CREDENTIAL_COMMAND", shQuote("echo fake-credential")},
	}
}

// installValueNames are the names of installValues, sorted.
func installValueNames() []string {
	var names []string
	for _, v := range installValues("", "", "", "") {
		names = append(names, v[0])
	}
	slices.Sort(names)
	return names
}

// shQuote quotes a text for sh.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// TestFreshInstall follows docs/getting-started.md for a new instance (ADR
// 0061): with values of its own, it runs the guide's run and check blocks in
// order and its start block, scripts/kiln-up, on an engine without Kiln's
// images, and takes one card of a repo that needs a work item from posting to
// published and merged. The Git host is a local bare repo, Claude is
// internal/run's scripted upstream in front of the real run image that
// kiln-up builds, and the publish and check result commands are stand-ins.
// The mac and azure blocks are skipped: only the colleague's Mac runs them.
//
// It runs only with KILN_INSTALL_TEST=1, and then fails, not skips, when it is
// not on the Git host's runner or the engine cannot be reached: it uses the
// instance's fixed names and port, and would replace the instance of the
// machine it runs on. check/in-image --install-only runs it.
func TestFreshInstall(t *testing.T) {
	if os.Getenv("KILN_INSTALL_TEST") != "1" {
		t.Skip("follows docs/getting-started.md on the container engine; check/in-image --install-only runs it")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Fatalf("runs only on the Git host's runner (GITHUB_ACTIONS=true): it uses the instance's fixed names, %s, the network %s, the kiln-* images and port 4747, and would replace the instance on this machine", installProxy, installNetwork)
	}
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Fatalf("the container engine is not reachable: %v\n%s", err, out)
	}

	// The guide, and its values' names against the test's.
	text, err := os.ReadFile("../" + guideFile)
	if err != nil {
		t.Fatal(err)
	}
	blocks, problems := parseGuide(string(text))
	if len(problems) > 0 {
		t.Fatalf("the guide:\n%s", strings.Join(problems, "\n"))
	}
	start := -1
	for i, b := range blocks {
		switch b.Kind {
		case kindValues:
			assertSameNames(t, valueNames(b.Text), installValueNames(), b.Line)
		case kindStart:
			start = i
		}
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// Not t.TempDir: the runs leave files the test may not remove, and the
	// runner is thrown away.
	tmp, err := os.MkdirTemp("", "kiln-install-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the install is under %s", tmp)
	home := filepath.Join(tmp, "home")
	mkdir(t, filepath.Join(home, "kiln"))

	// Kiln's Git host: the checkout's HEAD as main. The runner's checkout is
	// shallow, so the bare repo takes a shallow push.
	kilnGit := filepath.Join(tmp, "kiln-host.git")
	gitRun(t, "", "init", "--quiet", "--bare", "--initial-branch=main", kilnGit)
	gitRun(t, "", "-C", kilnGit, "config", "receive.shallowUpdate", "true")
	gitRun(t, "", "-C", root, "push", "--quiet", kilnGit, "HEAD:refs/heads/main")

	// The team's Git host, with one commit on main.
	teamGit := filepath.Join(tmp, "team-host.git")
	work := filepath.Join(tmp, "team-work")
	gitRun(t, "", "init", "--quiet", "--bare", "--initial-branch=main", teamGit)
	gitRun(t, "", "init", "--quiet", "--initial-branch=main", work)
	writeFile(t, filepath.Join(work, "README.md"), "# Team\n", 0o644)
	writeFile(t, filepath.Join(work, "NOTES.md"), "# Notes\n", 0o644)
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "--quiet", "-m", "The team's first commit")
	gitRun(t, work, "push", "--quiet", teamGit, "main")

	// The stand-in commands. The publish command pushes the card's commit to
	// its branch at the Git host, as a real one would, and says what it was
	// told.
	publishLog := filepath.Join(tmp, "publish.log")
	publish := filepath.Join(tmp, "publish")
	writeFile(t, publish, fmt.Sprintf(`#!/bin/sh
set -eu
printf 'KILN_CARD=%%s\nKILN_BRANCH=%%s\nKILN_COMMIT=%%s\nKILN_WORK_ITEM=%%s\n' \
    "$KILN_CARD" "$KILN_BRANCH" "$KILN_COMMIT" "${KILN_WORK_ITEM-}" >>%s
git -C "$KILN_REPO" push --quiet %s "+$KILN_COMMIT:refs/heads/$KILN_BRANCH" >&2
echo "https://git-host.invalid/pull/$KILN_CARD"
`, shQuote(publishLog), shQuote(teamGit)), 0o755)
	checkResult := filepath.Join(tmp, "check-result")
	writeFile(t, checkResult, "#!/bin/sh\necho passed\n", 0o755)

	var values strings.Builder
	values.WriteString("# The fresh-install test's values (check/install_test.go).\n")
	for _, v := range installValues(kilnGit, teamGit, publish, checkResult) {
		fmt.Fprintf(&values, "%s=%s\n", v[0], v[1])
	}
	writeFile(t, filepath.Join(home, "kiln", "values.sh"), values.String(), 0o644)

	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	runBlocks := func(from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			b := blocks[i]
			switch b.Kind {
			case kindRun, kindCheck:
				runGuideBlock(t, b, home, env)
			case kindValues:
				t.Logf("== %s:%d, step %q: the values block, replaced by the test's own", guideFile, b.Line, b.Step)
			case kindMac, kindAzure:
				t.Logf("== %s:%d, step %q: a %s block, not run here", guideFile, b.Line, b.Step, b.Kind)
			}
		}
	}

	// Up to the start.
	runBlocks(0, start)

	// The start, in a process group of its own, which Ctrl-C would reach.
	// Its output goes to a file, so that waiting for it does not wait for
	// every process that inherited it.
	launcherLog := filepath.Join(tmp, "start.log")
	logFile, err := os.Create(launcherLog)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	launcher := exec.Command("sh", "-eu", "-c", blocks[start].Text)
	launcher.Dir, launcher.Env = home, env
	launcher.Stdout, launcher.Stderr = logFile, logFile
	launcher.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	ended := make(chan error, 1)
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			if launcher.Process == nil {
				return
			}
			syscall.Kill(-launcher.Process.Pid, syscall.SIGINT)
			select {
			case <-ended:
			case <-time.After(2 * time.Minute):
				t.Errorf("the start block's processes did not end within 2 minutes of SIGINT; killed")
				syscall.Kill(-launcher.Process.Pid, syscall.SIGKILL)
				<-ended
			}
		})
	}
	source := filepath.Join(home, "kiln", "source")
	downRan := false
	t.Cleanup(func() {
		stop()
		t.Logf("== the start block's output:\n%s", readOrNil(launcherLog))
		if t.Failed() {
			for _, name := range []string{installUpstream, installProxy} {
				out, _ := exec.Command("docker", "logs", "--tail", "200", name).CombinedOutput()
				t.Logf("== docker logs %s:\n%s", name, out)
			}
		}
		if !downRan {
			if out, err := exec.Command(filepath.Join(source, "scripts", "kiln-down")).CombinedOutput(); err != nil {
				t.Logf("kiln-down: %v\n%s", err, out)
			}
		}
		exec.Command("docker", "rm", "--force", installUpstream).Run()
	})
	t.Logf("== %s:%d, step %q: the start block", guideFile, blocks[start].Line, blocks[start].Step)
	if err := launcher.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { ended <- launcher.Wait() }()

	// The screen answers once kiln-up has built everything and the core runs.
	// The waits add up to less than the test program's -test.timeout of 30
	// minutes, so that a wait that runs out fails the test and its cleanup
	// still prints what went wrong.
	waitFor(t, 12*time.Minute, "the screen to answer", func() (bool, string) {
		select {
		case err := <-ended:
			ended <- err
			t.Fatalf("the start block ended before the screen answered: %v", err)
		default:
		}
		status, _, err := screen(http.MethodGet, "/api/board", nil)
		return err == nil && status == http.StatusOK, fmt.Sprint(status, err)
	})

	// The scripted upstream, on the network the core made for its proxy.
	docker(t, "build", "--quiet", "--network", "none", "--tag", installUpstream,
		"--file", "internal/run/testdata/claude/Dockerfile", root)
	waitFor(t, 2*time.Minute, "the network "+installNetwork, func() (bool, string) {
		err := exec.Command("docker", "network", "inspect", installNetwork).Run()
		return err == nil, fmt.Sprint(err)
	})
	docker(t, "run", "--detach", "--name", installUpstream, "--network", installNetwork, installUpstream, installMarker)

	// The rest of the start's step: its check.
	after := start + 1
	for after < len(blocks) && blocks[after].Step == blocks[start].Step {
		after++
	}
	runBlocks(start+1, after)

	// A first card, to merged, as the screen moves it.
	id := postCard(t, "Add one line to NOTES.md. "+installCase)
	t.Logf("== card %d posted", id)
	c := waitCard(t, id, 4*time.Minute, "its spec to wait for approval", func(c installCard) bool {
		return c.Stage == "spec" && c.WaitsFor == "approval"
	})
	t.Logf("== card %d: the spec waits for approval", id)
	if status, body, err := screen(http.MethodPost, fmt.Sprintf("/api/cards/%d/decide", id),
		map[string]any{"action": "approve", "work_item": 1}); err != nil || status != http.StatusOK {
		t.Fatalf("approving card %d: %d %v\n%s", id, status, err, body)
	}
	t.Logf("== card %d: approved with the work item 1", id)
	c = waitCard(t, id, 7*time.Minute, "it to be published", func(c installCard) bool {
		return c.PullRequest != nil && c.PullRequest.Address != ""
	})
	t.Logf("== card %d: published at %s, commit %s", id, c.PullRequest.Address, c.PullRequest.Commit)
	if want := fmt.Sprintf("https://git-host.invalid/pull/%d", id); c.PullRequest.Address != want {
		t.Errorf("the pull request's address is %q, want %q", c.PullRequest.Address, want)
	}

	// The team merges it: the Git host's main goes to the published commit.
	commit := c.PullRequest.Commit
	gitRun(t, "", "-C", teamGit, "merge-base", "--is-ancestor", "main", commit)
	gitRun(t, "", "-C", teamGit, "update-ref", "refs/heads/main", commit)
	waitCard(t, id, 3*time.Minute, "it to be done as merged", func(c installCard) bool {
		return c.Stage == "done" && c.Merged
	})
	t.Logf("== card %d: done, merged", id)

	if notes := gitRun(t, "", "-C", teamGit, "show", "main:NOTES.md"); !strings.Contains(notes, installLine) {
		t.Errorf("NOTES.md on the Git host's main is %q, without the implement run's line %q", notes, installLine)
	}
	told := string(readOrNil(publishLog))
	for _, want := range []string{"KILN_WORK_ITEM=1\n", fmt.Sprintf("KILN_BRANCH=card/%d-", id), "KILN_COMMIT=" + commit + "\n"} {
		if !strings.Contains(told, want) {
			t.Errorf("the publish command was told:\n%s\nwithout %q", told, want)
		}
	}

	// Ctrl-C, then the rest of the guide: the stop.
	stop()
	waitFor(t, time.Minute, "the screen to stop answering", func() (bool, string) {
		_, _, err := screen(http.MethodGet, "/api/board", nil)
		return err != nil, "it still answers"
	})
	runBlocks(after, len(blocks))
	downRan = true
}

// assertSameNames fails at once when the guide's values block, at line, does
// not assign exactly the names the test sets.
func assertSameNames(t *testing.T, guide, test []string, line int) {
	t.Helper()
	var missing, extra []string
	for _, n := range guide {
		if !slices.Contains(test, n) {
			missing = append(missing, n)
		}
	}
	for _, n := range test {
		if !slices.Contains(guide, n) {
			extra = append(extra, n)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("the values block at %s:%d assigns %v; the test sets no value for %v, and sets %v, which the block does not assign",
			guideFile, line, guide, missing, extra)
	}
}

// runGuideBlock runs one block of the guide as sh -eu, in the home directory,
// with only PATH and HOME, and fails at once when it exits with another
// status than 0.
func runGuideBlock(t *testing.T, b guideBlock, home string, env []string) {
	t.Helper()
	t.Logf("== %s:%d, step %q: the %s block", guideFile, b.Line, b.Step, b.Kind)
	cmd := exec.CommandContext(t.Context(), "sh", "-eu", "-c", b.Text)
	cmd.Dir, cmd.Env = home, env
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the %s block at %s:%d, in step %q, failed: %v\n%s", b.Kind, guideFile, b.Line, b.Step, err, out)
	}
	t.Logf("%s", out)
}

// installCard is what the test reads of a card.
type installCard struct {
	ID          int64  `json:"id"`
	Stage       string `json:"stage"`
	WaitsFor    string `json:"waits_for"`
	Reason      string `json:"reason"`
	Dropped     bool   `json:"dropped"`
	Merged      bool   `json:"merged"`
	PullRequest *struct {
		Address string `json:"address"`
		Commit  string `json:"commit"`
	} `json:"pull_request"`
}

// postCard posts a card for the team repo, as the screen does, again while
// the core is starting and refuses moves, and returns its ID.
func postCard(t *testing.T, text string) int64 {
	t.Helper()
	var id int64
	waitFor(t, 5*time.Minute, "the core to take a card", func() (bool, string) {
		status, body, err := screen(http.MethodPost, "/api/cards", map[string]any{"text": text, "repo": installRepo})
		switch {
		case err != nil || status == http.StatusServiceUnavailable:
			return false, fmt.Sprint(status, err, string(body))
		case status != http.StatusCreated:
			t.Fatalf("posting the card: %d\n%s", status, body)
		}
		var c installCard
		if err := json.Unmarshal(body, &c); err != nil || c.ID == 0 {
			t.Fatalf("posting the card: %v\n%s", err, body)
		}
		id = c.ID
		return true, ""
	})
	return id
}

// waitCard waits for a card to reach what done says, and fails at once when
// it needs the developer or is dropped before that, with its reason.
func waitCard(t *testing.T, id int64, limit time.Duration, what string, done func(installCard) bool) installCard {
	t.Helper()
	var c installCard
	waitFor(t, limit, fmt.Sprintf("card %d: %s", id, what), func() (bool, string) {
		status, body, err := screen(http.MethodGet, fmt.Sprintf("/api/cards/%d", id), nil)
		if err != nil || status != http.StatusOK {
			return false, fmt.Sprint(status, err, string(body))
		}
		c = installCard{}
		if err := json.Unmarshal(body, &c); err != nil {
			t.Fatalf("card %d: %v\n%s", id, err, body)
		}
		if done(c) {
			return true, ""
		}
		if c.WaitsFor == "developer" || c.Dropped {
			t.Fatalf("card %d is in %s and waits for the developer, while the test waits for %s; its reason: %s\n%s",
				id, c.Stage, what, c.Reason, body)
		}
		return false, fmt.Sprintf("in %s, waiting for %s: %s", c.Stage, c.WaitsFor, c.Reason)
	})
	return c
}

// screen asks the core's screen, as the page does: JSON, with no Origin.
func screen(method, path string, body any) (int, []byte, error) {
	var in io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		in = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, installScreen+path, in)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, out, err
}

// waitFor asks ok every two seconds until it says true, and fails at once
// after limit, with what it waited for and the last answer.
func waitFor(t *testing.T, limit time.Duration, what string, ok func() (bool, string)) {
	t.Helper()
	end := time.Now().Add(limit)
	for {
		done, last := ok()
		if done {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("waited %v for %s; the last answer: %s", limit, what, last)
		}
		time.Sleep(2 * time.Second)
	}
}

// docker runs the docker command and fails at once when it fails.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// gitRun runs git in dir, as a person the test names, and fails at once when
// it fails.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Team", "GIT_AUTHOR_EMAIL=team@git-host.invalid",
		"GIT_COMMITTER_NAME=Team", "GIT_COMMITTER_EMAIL=team@git-host.invalid", "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

// readOrNil is a file's content, or nil when it cannot be read.
func readOrNil(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return []byte(err.Error())
	}
	return b
}
