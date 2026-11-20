package tick

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/instructions"
	"github.com/TheEisbaer/kiln/internal/store"
)

// The title the approved spec of the publishing tests names, and the text
// their implement run wrote.
const (
	publishTitle = "feat(notes): add a line to the notes"
	publishText  = "Adds a line to NOTES.md.\n\nChecked by reading it.\n"
)

// publishCommand is the stand-in for the developer's publish command, which a
// test writes into its directory dir. Each call is counted in dir/calls, and
// writes its environment, its working directory and its standard input to
// dir/env.<n>, and the text it is given to dir/text.<n>. What it does then is
// in dir/mode.<n>, or else dir/mode: "fail" exits 1 with a reason;
// "gate" waits for dir/gate after it has made dir/waiting; "merge" puts the
// commit on origin's main, as a merge on the Git host would; "silent" prints
// no address. Every call that does not fail prints the address
// https://git.example/pull/<card>, after a line of something else.
const publishCommand = `set -eu
dir=%s
n=$(( $(cat "$dir/calls" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$dir/calls"
{ env; echo "WORKDIR=$(pwd)"; echo "STDIN=$(cat)"; } > "$dir/env.$n"
cp "$KILN_TEXT_FILE" "$dir/text.$n"
mode=$(cat "$dir/mode.$n" 2>/dev/null || cat "$dir/mode" 2>/dev/null || true)
case "$mode" in
    fail) printf 'the Git host said no\n\033[2Jto %%s\n' "$KILN_BRANCH" >&2; exit 1 ;;
    gate) touch "$dir/waiting"; while [ ! -e "$dir/gate" ]; do sleep 0.05; done ;;
    merge) GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git --git-dir="$KILN_REPO" push --quiet origin "$KILN_COMMIT:refs/heads/main" ;;
    silent) echo "nothing"; exit 0 ;;
esac
echo "https://git.example/pull/not-this-one"
echo "Opened the pull request:"
echo "https://git.example/pull/$KILN_CARD"
`

// publishing is a bench for the publishing tests: an origin, a bare repo the
// Git host stands in for, a copy cloned from it that the core works on, and
// the publish command's directory.
type publishing struct {
	*bench
	origin string // the origin, a bare repo
	dir    string // the publish command's directory
	data   string // the store's file
}

// newPublishing makes a bench whose copy has a bare origin, with a core that
// publishes with the stand-in command and fetches at least every 50 ms.
func newPublishing(t *testing.T) *publishing {
	t.Helper()
	b := newBench(t, 3)
	origin := b.cfg.Repo
	b.cfg.Repo = filepath.Join(t.TempDir(), "copy.git")
	gitIn(t, filepath.Dir(b.cfg.Repo), "clone", "--quiet", "--bare", origin, b.cfg.Repo)
	p := &publishing{bench: b, origin: origin, dir: t.TempDir(), data: filepath.Join(b.cfg.Data, "store.db")}
	b.cfg.PublishCommand = fmt.Sprintf(publishCommand, p.dir)
	setFetchEvery(t, 50*time.Millisecond)
	return p
}

// setFetchEvery sets the least time between two fetches for one test.
func setFetchEvery(t *testing.T, d time.Duration) {
	was := fetchEvery
	fetchEvery = d
	t.Cleanup(func() { fetchEvery = was })
}

// look opens the store beside a core that may run, gives it to f, and closes
// it.
func (p *publishing) look(t *testing.T, f func(s *store.Store)) {
	t.Helper()
	s, err := store.Open(p.data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f(s)
}

// card writes a card whose review passed: its spec names publishTitle, its
// implement run, run 2, wrote a report and publishText and saved the files on
// the card's branch, on top of main, and its review run, run 3, passed. It
// returns the card's ID.
func (p *publishing) card(t *testing.T, s *store.Store, files map[string]string) int64 {
	t.Helper()
	c, err := s.Create("Add a line to the notes.\n\nThe second paragraph.")
	if err != nil {
		t.Fatal(err)
	}
	end := func(n int, from, to store.State, docs ...*store.Doc) {
		t.Helper()
		if _, err := s.Move(c.ID, from, store.CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("%s-%d-%d", p.instance, c.ID, n)
		if err := s.AddRun(store.RunRow{ID: id, Card: c.ID, Commit: p.commit, Model: "m", Effort: "high"}, MaxRuns); err != nil {
			t.Fatal(err)
		}
		if _, err := s.EndRun(id, store.Finished, "staged", to, docs...); err != nil {
			t.Fatal(err)
		}
	}
	end(1, waiting, finished, &store.Doc{Kind: store.KindSpec, Text: "# Spec\n\nPull request title: `" + publishTitle + "`\n\n## 1. What\n"})
	end(2, implementWaits, reviewWaits, &store.Doc{Kind: store.KindReport, Text: "# Report\n"},
		&store.Doc{Kind: store.KindPullRequest, Text: publishText})
	p.save(t, c.ID, "refs/heads/main", files, fmt.Sprintf("%s-%d-2", p.instance, c.ID))
	end(3, reviewWaits, reviewPassed, &store.Doc{Kind: store.KindReview, Text: "Verdict: passes\n"})
	return c.ID
}

// save commits files on top of a commit of the copy and puts the card's
// branch there, as an implement run's save would, with Kiln-Run.
func (p *publishing) save(t *testing.T, card int64, from string, files map[string]string, run string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	gitIn(t, filepath.Dir(work), "clone", "--quiet", "--no-checkout", p.cfg.Repo, work)
	gitIn(t, work, "checkout", "--quiet", "--detach", gitIn(t, p.cfg.Repo, "rev-parse", from))
	write(t, work, files)
	gitIn(t, work, "add", "--all")
	gitIn(t, work, "commit", "--quiet", "--message", publishTitle+"\n\nKiln-Run: "+run)
	gitIn(t, work, "push", "--quiet", "--force", "origin", fmt.Sprintf("HEAD:refs/heads/kiln/card-%d", card))
	return gitIn(t, work, "rev-parse", "HEAD")
}

// moveOrigin commits files on origin's main, as a merge of other work on the
// Git host would, and returns the new main.
func (p *publishing) moveOrigin(t *testing.T, files map[string]string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	gitIn(t, filepath.Dir(work), "clone", "--quiet", p.origin, work)
	write(t, work, files)
	gitIn(t, work, "add", "--all")
	gitIn(t, work, "commit", "--quiet", "--message", "other work")
	gitIn(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	return gitIn(t, work, "rev-parse", "HEAD")
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// publish makes the developer's move publish on a card, with the yes for its
// branch's tip when yes is set.
func (p *publishing) publish(t *testing.T, s *store.Store, card int64, yes bool) {
	t.Helper()
	o := store.Options{Publishing: true}
	if yes {
		o.WorkflowYes = gitIn(t, p.cfg.Repo, "rev-parse", fmt.Sprintf("refs/heads/kiln/card-%d", card))
	}
	if _, err := s.DecideWith(card, store.Publish, "", o); err != nil {
		t.Fatal(err)
	}
}

// waitFor waits until the card is in the state, and returns its last entry.
func (p *publishing) waitFor(t *testing.T, card int64, want store.State, what string) store.Entry {
	t.Helper()
	var last store.Entry
	waitUntil(t, fmt.Sprintf("card %d is in %s waiting for %s, %s", card, want.Stage, want.WaitsFor, what), func() bool {
		ok := false
		p.look(t, func(s *store.Store) {
			c, err := s.Card(card)
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.History(card)
			if err != nil {
				t.Fatal(err)
			}
			last = h[len(h)-1]
			ok = c.State == want && what == "" || c.State == want && strings.HasPrefix(last.Reason, what)
		})
		return ok
	})
	return last
}

// calls is how often the publish command ran.
func (p *publishing) calls(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(p.dir, "calls"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	var n int
	fmt.Sscan(string(data), &n)
	return n
}

// env is what the nth call of the publish command was told, by name.
func (p *publishing) env(t *testing.T, n int) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(string(readFile(t, filepath.Join(p.dir, fmt.Sprintf("env.%d", n)))), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			out[name] = value
		}
	}
	return out
}

// mode sets what the publish command does: for every call, or for call n.
func (p *publishing) mode(t *testing.T, n int, mode string) {
	t.Helper()
	name := "mode"
	if n > 0 {
		name = fmt.Sprintf("mode.%d", n)
	}
	if err := os.WriteFile(filepath.Join(p.dir, name), []byte(mode), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPublished publishes a card: the command is told the seven variables and
// nothing else is added, in the data directory with no standard input; the
// commit is the change on main as one commit, kept under its ref, and the
// card's branch stays; the address is the last line that starts with https://,
// kept with an entry, and the card waits for the Git host.
func TestPublished(t *testing.T) {
	p := newPublishing(t)
	t.Setenv(envWorkflowYes, "1") // the core's own, which the command is not told
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		p.publish(t, s, card, false)
	})
	tip := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/kiln/card-1")
	core := p.start(t, newFakeEngine(), 3)
	e := p.waitFor(t, card, atHost, "published as ")
	core.stop(t)

	if e.Reason != "published as https://git.example/pull/1" || e.Cause != store.CauseRule {
		t.Errorf("the entry is %+v", e)
	}
	commit := gitIn(t, p.cfg.Repo, "rev-parse", "refs/kiln/publish/card-1")
	p.with(t, func(s *store.Store) {
		if pub, err := s.Publication(card); err != nil || pub != (store.Publication{Commit: commit, Address: "https://git.example/pull/1"}) {
			t.Errorf("the publication is %+v (%v)", pub, err)
		}
	})
	if n := p.calls(t); n != 1 {
		t.Errorf("the command ran %d times", n)
	}
	env := p.env(t, 1)
	data, err := filepath.EvalSymlinks(p.cfg.Data)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		envCard: "1", envRepo: p.cfg.Repo, envCommit: commit, envBranch: "card/1-add-a-line-to-the-notes", envTitle: publishTitle,
		envTextFile: filepath.Join(p.cfg.Data, "publish", "card-1.md"), "WORKDIR": data, "STDIN": "",
	}
	for name, value := range want {
		if env[name] != value {
			t.Errorf("the command was told %s=%q, want %q", name, env[name], value)
		}
	}
	if v, ok := env[envWorkflowYes]; ok {
		t.Errorf("the command was told %s=%q for a change without workflow files", envWorkflowYes, v)
	}
	// Nothing else is added to the core's environment.
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if name != envWorkflowYes && name != "PWD" && name != "SHLVL" && name != "OLDPWD" && name != "_" && env[name] != value && !strings.Contains(value, "\n") {
			t.Errorf("the command was told %s=%q, the core has %q", name, env[name], value)
		}
	}
	for name := range env {
		if strings.HasPrefix(name, "KILN_") && want[name] == "" && os.Getenv(name) == "" {
			t.Errorf("the command was told %s as well", name)
		}
	}
	if got := string(readFile(t, filepath.Join(p.dir, "text.1"))); got != publishText {
		t.Errorf("the text is %q, want the implement run's %q", got, publishText)
	}

	// One commit on main: the merge's tree, main as its only parent, the title
	// and the card's and the run's trailers.
	if got := gitIn(t, p.cfg.Repo, "log", "--format=%P", "-1", commit); got != p.commit {
		t.Errorf("the commit's parents are %q, want main's %s", got, p.commit)
	}
	if got := gitIn(t, p.cfg.Repo, "show", commit+":NOTES.md"); got != "# Notes\nA line." {
		t.Errorf("NOTES.md in the commit is %q", got)
	}
	if got, want := gitIn(t, p.cfg.Repo, "log", "--format=%B", "-1", commit), publishTitle+"\n\nKiln-Card: 1\nKiln-Run: "+p.instance+"-1-2"; got != want {
		t.Errorf("the commit's message is %q, want %q", got, want)
	}
	if got := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/kiln/card-1"); got != tip {
		t.Errorf("the card's branch moved from %s to %s", tip, got)
	}
}

// TestPublishFails moves a card whose command fails to "needs you" with the
// command's reason, made printable; publish again from there works. A command
// that prints no address fails as well.
func TestPublishFails(t *testing.T) {
	p := newPublishing(t)
	p.mode(t, 1, "fail")
	p.mode(t, 2, "silent")
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		p.publish(t, s, card, false)
	})
	core := p.start(t, newFakeEngine(), 3)
	e := p.waitFor(t, card, reviewNeedsYou, "")
	if want := "publishing failed: the publish command failed: exit status 1: the Git host said no\n�[2Jto card/1-add-a-line-to-the-notes"; e.Reason != want {
		t.Errorf("the reason is %q, want %q", e.Reason, want)
	}
	p.look(t, func(s *store.Store) {
		c, _ := s.Card(card)
		moves, err := s.Moves(c, true)
		if err != nil || !slices.Contains(moves, store.Allowed{Action: store.Publish, Note: store.NoNote}) {
			t.Errorf("the moves after a failure: %v, %v", moves, err)
		}
		p.publish(t, s, card, false)
	})
	e = p.waitFor(t, card, reviewNeedsYou, "publishing failed: the publish command printed no address")
	p.look(t, func(s *store.Store) { p.publish(t, s, card, false) })
	p.waitFor(t, card, atHost, "published as https://git.example/pull/1")
	core.stop(t)
	if n := p.calls(t); n != 3 {
		t.Errorf("the command ran %d times, want 3", n)
	}
}

// TestPublishWorkflows publishes a change to a workflow file only with the
// developer's yes for its tip, and then tells the command so; without it, or
// with a yes for another tip, the command is not run (ADR 0060).
func TestPublishWorkflows(t *testing.T) {
	p := newPublishing(t)
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n", ".github/workflows/check.yml": "on: push\n"})
		p.publish(t, s, card, false)
	})
	core := p.start(t, newFakeEngine(), 3)
	e := p.waitFor(t, card, reviewNeedsYou, "")
	if want := "publishing failed: the change touches workflow files, and you did not say yes to this change: .github/workflows/check.yml"; e.Reason != want {
		t.Errorf("the reason is %q", e.Reason)
	}
	if n := p.calls(t); n != 0 {
		t.Fatalf("the command ran %d times without the yes", n)
	}
	p.look(t, func(s *store.Store) {
		if _, err := s.DecideWith(card, store.Publish, "", store.Options{Publishing: true, WorkflowYes: p.commit}); err != nil {
			t.Fatal(err)
		}
	})
	p.waitFor(t, card, reviewNeedsYou, "publishing failed: the change touches workflow files")
	p.look(t, func(s *store.Store) { p.publish(t, s, card, true) })
	p.waitFor(t, card, atHost, "published as ")
	core.stop(t)
	if n := p.calls(t); n != 1 || p.env(t, 1)[envWorkflowYes] != "1" {
		t.Errorf("the command ran %d times, told %s=%q", n, envWorkflowYes, p.env(t, 1)[envWorkflowYes])
	}
}

// TestPublishConflict has origin's main move under a card, so that its change
// collides with it: the card goes back to implement with the files; its
// branch is Kiln's merge of main, conflict markers included, and the
// implement run that follows starts there and is told what changed in main.
// A core that finds the branch at that merge sends the card back again.
func TestPublishConflict(t *testing.T) {
	p := newPublishing(t)
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line of the card.\n", "card.txt": "the card's\n"})
		p.publish(t, s, card, false)
	})
	tip := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/kiln/card-1")
	main := p.moveOrigin(t, map[string]string{"NOTES.md": "# Notes\nA line of other work.\n", "other.txt": "other\n"})
	e := newFakeEngine()
	core := p.start(t, e, 3)
	last := p.waitFor(t, card, implementWaits, "")
	if want := "main moved and the change no longer applies: NOTES.md"; last.Reason != want || last.Cause != store.CauseRule {
		t.Errorf("the entry is %+v, want the reason %q", last, want)
	}
	run := fmt.Sprintf("%s-1-4", p.instance)
	waitUntil(t, "the implement run starts", func() bool { return slices.Contains(e.snapshot().started, card) })
	core.stop(t)
	if n := p.calls(t); n != 0 {
		t.Errorf("the command ran %d times", n)
	}

	merge := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/kiln/card-1")
	if got := gitIn(t, p.cfg.Repo, "log", "--format=%P", "-1", merge); got != tip+" "+main {
		t.Errorf("the merge's parents are %q, want the tip %s and main %s", got, tip, main)
	}
	notes := gitIn(t, p.cfg.Repo, "show", merge+":NOTES.md")
	for _, want := range []string{"<<<<<<< refs/heads/kiln/card-1", "A line of the card.", "|||||||", "=======", "A line of other work.", ">>>>>>> refs/heads/main"} {
		if !strings.Contains(notes, want) {
			t.Errorf("NOTES.md of the merge lacks %q:\n%s", want, notes)
		}
	}
	if got := gitIn(t, p.cfg.Repo, "show", merge+":other.txt"); got != "other" {
		t.Errorf("other.txt of the merge is %q", got)
	}
	p.with(t, func(s *store.Store) {
		r, err := s.Run(run)
		if err != nil || r.Commit != merge {
			t.Errorf("the implement run is %+v (%v), want it from the merge %s", r, err, merge)
		}
	})
	told := string(readFile(t, filepath.Join(p.cfg.Data, "instructions", run+".md")))
	if h := toldParts(t, told, instructions.Implement); !slices.Equal(h, []string{"## The card", "## The approved spec", "## What changed in main"}) {
		t.Errorf("the implement run is told the headings %q", h)
	}
	if want := "## What changed in main\n\n" + mainMoved(card, []string{"NOTES.md"}); !strings.HasSuffix(told, want) || !strings.Contains(want, "```text\nNOTES.md\n```") {
		t.Errorf("the implement run is told\n%s\nwant it to end with\n%s", told, want)
	}

	// A kill after the merge and before the card moved leaves the branch at
	// the merge: the next publishing sends the card back as it is.
	p.with(t, func(s *store.Store) {
		if _, err := s.EndRun(run, store.Failed, "stopped by the test", implementNeedsYou); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Move(card, atHost, store.CauseDeveloper); err != nil {
			t.Fatal(err)
		}
	})
	core = p.start(t, newFakeEngine(), 3)
	last = p.waitFor(t, card, implementWaits, "")
	core.stop(t)
	if last.Reason != "main moved and the change no longer applies: NOTES.md" {
		t.Errorf("the entry is %+v", last)
	}
	if got := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/kiln/card-1"); got != merge {
		t.Errorf("the branch is at %s, not at the merge %s", got, merge)
	}
	if n := p.calls(t); n != 0 {
		t.Errorf("the command ran %d times", n)
	}
}

// TestPublishEmpty closes a card whose change is all in main already, without
// the command.
func TestPublishEmpty(t *testing.T) {
	p := newPublishing(t)
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		p.publish(t, s, card, false)
	})
	p.moveOrigin(t, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
	core := p.start(t, newFakeEngine(), 3)
	e := p.waitFor(t, card, closed, "")
	core.stop(t)
	if e.Reason != "everything the change does is in main already" || e.Cause != store.CauseRule || p.calls(t) != 0 {
		t.Errorf("the entry is %+v, and the command ran %d times", e, p.calls(t))
	}
}

// TestPublishMerged closes a card whose change the Git host merged: the
// stand-in moves origin's main to the published commit, and after the next
// fetch the card is done, by the Git host, merged.
func TestPublishMerged(t *testing.T) {
	p := newPublishing(t)
	p.mode(t, 0, "merge")
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		p.publish(t, s, card, false)
	})
	core := p.start(t, newFakeEngine(), 3)
	e := p.waitFor(t, card, closed, "")
	core.stop(t)
	if e.Reason != "merged" || e.Cause != store.CauseHost || p.calls(t) != 1 {
		t.Errorf("the entry is %+v, and the command ran %d times", e, p.calls(t))
	}
	if got, want := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/main"), gitIn(t, p.cfg.Repo, "rev-parse", "refs/kiln/publish/card-1"); got != want {
		t.Errorf("main is at %s, want the published %s", got, want)
	}
	p.with(t, func(s *store.Store) {
		b, err := s.Board()
		if err != nil || len(b) != 1 || !b[0].Merged {
			t.Errorf("the board: %+v, %v", b, err)
		}
	})
}

// TestFetch brings the copy's main up to date from an origin that moved, and
// a spec run starts from it; a fetch that fails is printed once per text and
// the main that is there is used; a copy without origin is used as it is.
func TestFetch(t *testing.T) {
	p := newPublishing(t)
	main := p.moveOrigin(t, map[string]string{"NOTES.md": "# Notes\nMoved.\n"})
	var card int64
	p.with(t, func(s *store.Store) { card = p.bench.card(t, s, store.Spec) })
	e := newFakeEngine()
	core := p.start(t, e, 3)
	waitUntil(t, "the spec run starts", func() bool { return slices.Contains(e.snapshot().started, card) })
	core.stop(t)
	if got := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/main"); got != main {
		t.Errorf("main is at %s, want origin's %s", got, main)
	}
	p.with(t, func(s *store.Store) {
		if r, err := s.InFlight(card); err != nil || r == nil || r.Commit != main {
			t.Errorf("the spec run is %+v (%v), want it from %s", r, err, main)
		}
	})

	// An origin that is gone: printed once, and main stays.
	if err := os.RemoveAll(p.origin); err != nil {
		t.Fatal(err)
	}
	e.open(card, "exit")
	p.with(t, func(s *store.Store) { p.bench.card(t, s, store.Spec) })
	core = p.start(t, e, 3)
	from := time.Now()
	waitUntil(t, "five rounds of fetching", func() bool { return time.Since(from) > 5*fetchEvery })
	e.waitTicks(t, 5)
	core.stop(t)
	if n := strings.Count(core.out.String(), "main is not brought up to date from origin"); n != 1 {
		t.Errorf("the failure was printed %d times:\n%s", n, core.out)
	}
	if got := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/main"); got != main {
		t.Errorf("main is at %s after a failed fetch, want %s", got, main)
	}

	// A copy without origin is used as it is.
	gitIn(t, p.cfg.Repo, "remote", "remove", "origin")
	core = p.start(t, e, 3)
	e.waitTicks(t, 10)
	core.stop(t)
	if strings.Contains(core.out.String(), "origin") {
		t.Errorf("a copy without origin: %s", core.out)
	}
}

// TestPublishKilled kills a core while the command runs: the next core
// publishes the same commit again, and the card ends with one address.
func TestPublishKilled(t *testing.T) {
	p := newPublishing(t)
	p.mode(t, 1, "gate")
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		p.publish(t, s, card, false)
	})
	ch := child{Command: "core", Config: p.cfg, NoEngine: true}
	cmd, done := startChild(t, ch)
	waitUntil(t, "the command waits", func() bool { _, err := os.Stat(filepath.Join(p.dir, "waiting")); return err == nil })
	cmd.Process.Signal(syscall.SIGKILL)
	<-done
	// The command outlives the core, as it would; it is let go.
	if err := os.WriteFile(filepath.Join(p.dir, "gate"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd, done = startChild(t, ch)
	p.waitFor(t, card, atHost, "published as ")
	cmd.Process.Signal(os.Interrupt)
	if code := <-done; code != 0 {
		t.Errorf("the core exited with %d\n%s%s", code, cmd.Stdout, cmd.Stderr)
	}
	if n := p.calls(t); n != 2 || p.env(t, 1)[envCommit] != p.env(t, 2)[envCommit] {
		t.Errorf("the command ran %d times, with %s and %s", n, p.env(t, 1)[envCommit], p.env(t, 2)[envCommit])
	}
	p.with(t, func(s *store.Store) {
		h, err := s.History(card)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range h {
			if strings.HasPrefix(e.Reason, store.PublishedReason) {
				n++
			}
		}
		if pub, _ := s.Publication(card); n != 1 || pub.Address != "https://git.example/pull/1" {
			t.Errorf("%d entries of an address, the publication %+v", n, pub)
		}
	})
}

// startChild starts the test binary as a child, as the engine test's
// fixture does, without its engine.
func startChild(t *testing.T, ch child) (*exec.Cmd, <-chan int) {
	t.Helper()
	return (&fixture{}).start(t, ch)
}

// TestPublishBranch names the branch a card is published on.
func TestPublishBranch(t *testing.T) {
	for title, want := range map[string]string{
		"feat(tick): publish a card's change from the screen and close the card when it is in main": "card/7-publish-a-card-s-change-from-the-screen-and-close",
		"Add a line to NOTES.md":                         "card/7-add-a-line-to-notes-md",
		"fix!: Ärger mit Umlauten":                       "card/7-rger-mit-umlauten",
		"docs: " + strings.Repeat("x", 60) + " and more": "card/7-" + strings.Repeat("x", 50),
		"«»":           "card/7",
		"feat: v2 API": "card/7-v2-api",
	} {
		if got := publishBranch(7, title); got != want {
			t.Errorf("publishBranch(%q) = %q, want %q", title, got, want)
		}
	}
}

// TestTitleOf takes the approved spec's title, or the card's first line cut
// to 72 characters, its control characters spaces.
func TestTitleOf(t *testing.T) {
	c, _ := stagedCore(t)
	for text, want := range map[string]string{
		"The card's text.":              "The card's text.",
		"\n  \nFirst line\tthere\nmore": "First line there",
		strings.Repeat("é", 80):         strings.Repeat("é", 72),
		"a\x1b[2Jb":                     "a [2Jb",
		" \n ":                          "Card 1",
	} {
		got, err := c.titleOf(store.Card{ID: 1, Text: text})
		if err != nil || got != want {
			t.Errorf("titleOf(%q) = %q, %v; want %q", text, got, err, want)
		}
	}
	endRun(t, c, store.Spec, finished, &store.Doc{Kind: store.KindSpec, Text: "# Spec\n\nPull request title: `feat: the title`\n"})
	if got, _ := c.titleOf(store.Card{ID: 1, Text: "The card's text."}); got != "feat: the title" {
		t.Errorf("the title with a spec that names one is %q", got)
	}
}

// TestRunPublish runs a command to its end: its address, its failure with the
// last 2 KiB of its standard error, and one that takes too long, which ends
// with its children.
func TestRunPublish(t *testing.T) {
	c := &core{Config: Config{Data: t.TempDir()}}
	for _, tc := range []struct {
		command, address, failure string
	}{
		{"echo https://a.example/1; echo 'https://b.example/2\r'; echo done", "https://b.example/2", ""},
		{"echo http://a.example/1", "", "the publish command printed no address that starts with https://"},
		{"echo 'https:// not one'", "", "the publish command printed an address that is not one: https:// not one"},
		{"echo https://a.example/1; exit 3", "", "the publish command failed: exit status 3"},
		// The last 2 KiB of what it said.
		{"printf 'x%.0s' $(seq 3000) >&2; echo >&2; echo the end >&2; exit 1", "", "the publish command failed: exit status 1: …" +
			strings.Repeat("x", 2048-len("\nthe end")) + "\nthe end"},
	} {
		c.PublishCommand = tc.command
		address, failure := c.runPublish(t.Context(), nil)
		if address != tc.address || failure != tc.failure {
			t.Errorf("%s: %q, %q; want %q, %q", tc.command, address, failure, tc.address, tc.failure)
		}
	}

	was := publishLimit
	publishLimit = 200 * time.Millisecond
	t.Cleanup(func() { publishLimit = was })
	pid := filepath.Join(t.TempDir(), "pid")
	c.PublishCommand = "sleep 60 & echo $! > " + pid + "; wait"
	start := time.Now()
	if _, failure := c.runPublish(t.Context(), nil); failure != "the publish command did not end within 200ms" || time.Since(start) > 10*time.Second {
		t.Errorf("a command that takes too long: %q after %s", failure, time.Since(start))
	}
	var n int
	fmt.Sscan(string(readFile(t, pid)), &n)
	waitUntil(t, "the command's child has ended", func() bool { return syscall.Kill(n, 0) != nil })
}
