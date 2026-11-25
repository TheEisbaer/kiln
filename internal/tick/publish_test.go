package tick

import (
	"bytes"
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
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
	repo string // the name of the repo its cards are written in, kiln when empty
	// workItem is the work item number its cards are written with, 0 for none.
	workItem int64
	origin   string // the origin, a bare repo
	dir      string // the publish command's directory
	data     string // the store's file
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
	return p.stage(t, s, files, true)
}

// reviewing writes a card as card does, without its review run: it waits for
// one, with its branch saved, and the run is the core's to start.
func (p *publishing) reviewing(t *testing.T, s *store.Store, files map[string]string) int64 {
	t.Helper()
	return p.stage(t, s, files, false)
}

func (p *publishing) stage(t *testing.T, s *store.Store, files map[string]string, passed bool) int64 {
	t.Helper()
	c, err := s.CreateWithWorkItem(cmp.Or(p.repo, store.KilnRepo), "Add a line to the notes.\n\nThe second paragraph.", 0, p.workItem)
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
		if _, err := s.EndRun(id, store.Finished, "staged", to, store.Cost{}, docs...); err != nil {
			t.Fatal(err)
		}
	}
	end(1, waiting, finished, &store.Doc{Kind: store.KindSpec, Text: "# Spec\n\nModel for implement: `claude-haiku-5-5`\n\nPull request title: `" + publishTitle + "`\n\n## 1. What\n"})
	end(2, implementWaits, reviewWaits, &store.Doc{Kind: store.KindReport, Text: "# Report\n"},
		&store.Doc{Kind: store.KindPullRequest, Text: publishText})
	p.save(t, c.ID, "refs/heads/main", files, fmt.Sprintf("%s-%d-2", p.instance, c.ID))
	if passed {
		end(3, reviewWaits, reviewPassed, &store.Doc{Kind: store.KindReview, Text: "Verdict: passes\n"})
	}
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

// waitFor waits until the card is in the state, and its last entry's reason
// begins with what, and returns that entry; on failure it says where the
// card is.
func (p *publishing) waitFor(t *testing.T, card int64, want store.State, what string) store.Entry {
	t.Helper()
	var last store.Entry
	var at store.State
	deadline := time.Now().Add(waitLimit)
	for {
		p.look(t, func(s *store.Store) {
			c, err := s.Card(card)
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.History(card)
			if err != nil {
				t.Fatal(err)
			}
			at, last = c.State, h[len(h)-1]
		})
		if at == want && strings.HasPrefix(last.Reason, what) {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("card %d is in %v with the last entry %+v, want %v and a reason that begins %q", card, at, last, want, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
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
		if pub, err := s.Publication(card); err != nil || pub != (store.Publication{Commit: commit, Address: "https://git.example/pull/1", Branch: "card/1-add-a-line-to-the-notes"}) {
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
		// Main moved counts as a send-back (ADR 0054, rule 3), so the run is
		// on the stronger model and not on the spec's claude-haiku-5-5.
		if r.Model != "claude-opus-5-5" {
			t.Errorf("the implement run is on %s, want claude-opus-5-5", r.Model)
		}
		if c, _ := s.Card(card); c.SendBacks != 1 {
			t.Errorf("the card has %d send-backs, want 1", c.SendBacks)
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
		if _, err := s.EndRun(run, store.Failed, "stopped by the test", implementNeedsYou, store.Cost{}); err != nil {
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

// TestPublishConflictThird has main move under a card that was sent back
// twice already: after the merge of main the card waits in implement for the
// developer, its number goes up to 3, and no run starts. The developer's
// again starts the implement run from the merge.
func TestPublishConflictThird(t *testing.T) {
	p := newPublishing(t)
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line of the card.\n"})
		// Two send-backs, as two returns by the rule would leave the card.
		if _, err := s.MoveSentBack(card, reviewPassed, reviewNeedsYou, "sent back by the test"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MoveSentBack(card, reviewNeedsYou, reviewPassed, "sent back by the test"); err != nil {
			t.Fatal(err)
		}
		p.publish(t, s, card, false)
	})
	tip := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/kiln/card-1")
	main := p.moveOrigin(t, map[string]string{"NOTES.md": "# Notes\nA line of other work.\n"})
	e := newFakeEngine()
	core := p.start(t, e, 3)
	last := p.waitFor(t, card, implementNeedsYou, "")
	e.waitTicks(t, 3)
	core.stop(t)
	want := "main moved and the change no longer applies: NOTES.md; the card was sent back twice already, so it waits for you"
	if last.Reason != want || last.Cause != store.CauseRule {
		t.Errorf("the entry is %+v, want the reason %q", last, want)
	}
	if s := e.snapshot(); len(s.started) != 0 || p.calls(t) != 0 {
		t.Errorf("runs started for %v, and the command ran %d times", s.started, p.calls(t))
	}
	merge := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/kiln/card-1")
	if got := gitIn(t, p.cfg.Repo, "log", "--format=%P", "-1", merge); got != tip+" "+main {
		t.Errorf("the merge's parents are %q, want the tip %s and main %s", got, tip, main)
	}
	p.with(t, func(s *store.Store) {
		c, err := s.Card(card)
		if err != nil || c.SendBacks != 3 {
			t.Errorf("the card is %+v (%v), want 3 send-backs", c, err)
		}
		if n, _ := s.RunCount(card); n != 3 {
			t.Errorf("the card has %d runs, want 3", n)
		}
		if _, err := s.Decide(card, store.Again, ""); err != nil {
			t.Fatal(err)
		}
	})
	e = newFakeEngine()
	core = p.start(t, e, 3)
	waitUntil(t, "the implement run starts", func() bool { return slices.Contains(e.snapshot().started, card) })
	core.stop(t)
	run := fmt.Sprintf("%s-1-4", p.instance)
	p.with(t, func(s *store.Store) {
		if r, err := s.Run(run); err != nil || r.Commit != merge || r.Model != "claude-opus-5-5" {
			t.Errorf("the implement run is %+v (%v), want it from the merge %s on claude-opus-5-5", r, err, merge)
		}
	})
	if told := string(readFile(t, filepath.Join(p.cfg.Data, "instructions", run+".md"))); !strings.HasSuffix(told, mainMoved(card, []string{"NOTES.md"})) {
		t.Errorf("the implement run is told\n%s", told)
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
		address, failure := c.runPublish(t.Context(), c.PublishCommand, nil)
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
	if _, failure := c.runPublish(t.Context(), c.PublishCommand, nil); failure != "the publish command did not end within 200ms" || time.Since(start) > 10*time.Second {
		t.Errorf("a command that takes too long: %q after %s", failure, time.Since(start))
	}
	var n int
	fmt.Sscan(string(readFile(t, pid)), &n)
	waitUntil(t, "the command's child has ended", func() bool { return ended(n) })
}

// hangingSSH is a stand-in for ssh that says whether it has a terminal it
// could ask on and what it was given, and then hangs, as an ssh that waits for
// a passphrase or a host's answer does, holding git's standard error. Its
// process is the sleep it ends in.
const hangingSSH = `#!/bin/sh
dir=%s
echo "$$" > "$dir/pid"
echo "$*" > "$dir/args"
if ( : < /dev/tty ) 2>/dev/null; then echo tty > "$dir/tty"; else echo none > "$dir/tty"; fi
exec sleep 60
`

// hangingOrigin points a copy's origin at an ssh address whose ssh is the
// stand-in, and returns the stand-in's directory.
func hangingOrigin(t *testing.T, repo string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "ssh")
	if err := os.WriteFile(script, []byte(fmt.Sprintf(hangingSSH, dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "remote", "set-url", "origin", "ssh://kiln-test.invalid/repo.git")
	gitIn(t, repo, "config", "core.sshCommand", script)
	t.Setenv("GIT_SSH_COMMAND", "")
	return dir
}

// TestFetchCannotAsk fetches from an origin whose ssh hangs: ssh is told to
// ask nothing and has no terminal to ask on, and at the fetch's limit it is
// ended with git, and the fetch with them.
func TestFetchCannotAsk(t *testing.T) {
	c, _ := stagedCore(t)
	dir := hangingOrigin(t, c.Repo)
	var errOut strings.Builder
	c.errOut = &errOut
	was := fetchLimit
	fetchLimit = 500 * time.Millisecond
	t.Cleanup(func() { fetchLimit = was })

	start := time.Now()
	if rounds := c.refreshMain(t.Context(), c.own(), time.Minute); rounds != 1 {
		t.Errorf("%d rounds of fetching ended, want 1", rounds)
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("the fetch took %s with a limit of %s", took, fetchLimit)
	}
	if !strings.Contains(errOut.String(), "git fetch did not end within 500ms") {
		t.Errorf("the core said %q", errOut.String())
	}
	if got := strings.TrimSpace(string(readFile(t, filepath.Join(dir, "tty")))); got != "none" {
		t.Errorf("ssh had a terminal: %s", got)
	}
	if args := string(readFile(t, filepath.Join(dir, "args"))); !strings.HasPrefix(args, "-o BatchMode=yes ") {
		t.Errorf("ssh was given %q", args)
	}
	var pid int
	fmt.Sscan(string(readFile(t, filepath.Join(dir, "pid"))), &pid)
	waitUntil(t, "ssh has ended", func() bool { return ended(pid) })
}

// TestFetchDoesNotHoldRuns starts a spec run while the fetch hangs: the tick
// waits for it no longer than fetchWait. The comparison before the report
// waits for the hanging origin up to compareLimit, shortened here.
func TestFetchDoesNotHoldRuns(t *testing.T) {
	b := newBench(t, 3)
	hangingOrigin(t, b.cfg.Repo)
	was, wasCompare := fetchWait, compareLimit
	fetchWait, compareLimit = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { fetchWait, compareLimit = was, wasCompare })
	var card int64
	b.with(t, func(s *store.Store) { card = b.card(t, s, store.Spec) })
	e := newFakeEngine()
	start := time.Now()
	core := b.start(t, e, 3)
	waitUntil(t, "the spec run starts", func() bool { return slices.Contains(e.snapshot().started, card) })
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the run started after %s, while the fetch hung", took)
	}
	e.open(card, "exit")
	e.waitGone(t, card)
	core.stop(t)
}

// resolve commits files on top of the card's branch, as a run's save would,
// and returns the new tip.
func (p *publishing) resolve(t *testing.T, card int64, files map[string]string) string {
	t.Helper()
	branch := fmt.Sprintf("refs/heads/kiln/card-%d", card)
	return p.save(t, card, branch, files, fmt.Sprintf("%s-%d-9", p.instance, card))
}

// TestPublishMarkers has a run after a conflict save only part of its work:
// the next implement run starts there and is told the files that still hold
// Kiln's conflict markers, and not the review that passed; a branch that
// still holds one, as after a run that finished and left it, is not
// published: the card goes back to implement, and the command is not run.
func TestPublishMarkers(t *testing.T) {
	p := newPublishing(t)
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line of the card.\n", "card.txt": "the card's\n"})
		p.publish(t, s, card, false)
	})
	p.moveOrigin(t, map[string]string{"NOTES.md": "# Notes\nA line of other work.\n", "card.txt": "main's\n"})
	e := newFakeEngine()
	core := p.start(t, e, 3)
	last := p.waitFor(t, card, implementWaits, "")
	waitUntil(t, "the implement run starts", func() bool { return slices.Contains(e.snapshot().started, card) })
	core.stop(t)
	if last.Reason != "main moved and the change no longer applies: NOTES.md, card.txt" {
		t.Errorf("the entry is %+v", last)
	}

	// The run resolves NOTES.md, its save is kept, and it fails.
	partial := p.resolve(t, card, map[string]string{"NOTES.md": "# Notes\nA line of other work.\nA line of the card.\n"})
	p.with(t, func(s *store.Store) {
		if _, err := s.EndRun(fmt.Sprintf("%s-1-4", p.instance), store.Failed, "exit 1, no result, saved", implementNeedsYou, store.Cost{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Decide(card, store.Again, ""); err != nil {
			t.Fatal(err)
		}
	})
	e = newFakeEngine()
	core = p.start(t, e, 3)
	waitUntil(t, "the second implement run starts", func() bool { return slices.Contains(e.snapshot().started, card) })
	core.stop(t)
	run := fmt.Sprintf("%s-1-5", p.instance)
	p.with(t, func(s *store.Store) {
		if r, err := s.Run(run); err != nil || r.Commit != partial {
			t.Errorf("the run is %+v (%v), want it from the partial save %s", r, err, partial)
		}
		// Main moved counts as a send-back (ADR 0054, rule 3): both runs are
		// on the stronger model, not on the spec's claude-haiku-5-5.
		for _, id := range []string{fmt.Sprintf("%s-1-4", p.instance), run} {
			if r, err := s.Run(id); err != nil || r.Model != "claude-opus-5-5" {
				t.Errorf("the run %s is %+v (%v), want it on claude-opus-5-5", id, r, err)
			}
		}
		if _, err := s.EndRun(run, store.Finished, "exit 0, result success, 1 turns, $0.0001, no change", reviewPassed, store.Cost{}); err != nil {
			t.Fatal(err)
		}
	})
	told := string(readFile(t, filepath.Join(p.cfg.Data, "instructions", run+".md")))
	if h := toldParts(t, told, instructions.Implement); !slices.Equal(h, []string{"## The card", "## The approved spec", "## What changed in main"}) {
		t.Errorf("the run after a partial save is told the headings %q", h)
	}
	if !strings.HasSuffix(told, mainMoved(card, []string{"card.txt"})) {
		t.Errorf("the run after a partial save is told\n%s", told)
	}

	// Published as it is, with card.txt still marked: back to implement.
	p.with(t, func(s *store.Store) { p.publish(t, s, card, false) })
	core = p.start(t, newFakeEngine(), 3)
	last = p.waitFor(t, card, implementWaits, "")
	core.stop(t)
	if last.Reason != "main moved and the change no longer applies: card.txt" || p.calls(t) != 0 {
		t.Errorf("the entry is %+v, and the command ran %d times", last, p.calls(t))
	}
	if got := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/kiln/card-1"); got != partial {
		t.Errorf("the branch moved from %s to %s", partial, got)
	}
}

// TestPublishBranchKept publishes a card again after its spec's title changed:
// the command is given the branch of the first publishing, so that the card
// keeps one pull request.
func TestPublishBranchKept(t *testing.T) {
	p := newPublishing(t)
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		p.publish(t, s, card, false)
	})
	core := p.start(t, newFakeEngine(), 3)
	p.waitFor(t, card, atHost, "published as ")
	core.stop(t)
	p.with(t, func(s *store.Store) {
		if _, err := s.DecideWith(card, store.SendBack, "Name it otherwise.", store.Options{Publishing: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Move(card, waiting, store.CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("%s-%d-4", p.instance, card)
		if err := s.AddRun(store.RunRow{ID: id, Card: card, Commit: p.commit, Model: "m", Effort: "high"}, MaxRuns); err != nil {
			t.Fatal(err)
		}
		if _, err := s.EndRun(id, store.Finished, "staged", finished, store.Cost{}, &store.Doc{Kind: store.KindSpec, Text: "# Spec\n\nPull request title: `feat: another name entirely`\n"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Move(card, reviewPassed, store.CauseRun); err != nil {
			t.Fatal(err)
		}
		p.publish(t, s, card, false)
	})
	core = p.start(t, newFakeEngine(), 3)
	p.waitFor(t, card, atHost, "published as ")
	core.stop(t)
	first, second := p.env(t, 1), p.env(t, 2)
	if first[envBranch] != "card/1-add-a-line-to-the-notes" || second[envBranch] != first[envBranch] || second[envTitle] != "feat: another name entirely" {
		t.Errorf("the branches are %q and %q, the second title %q", first[envBranch], second[envBranch], second[envTitle])
	}
}

// TestPublishError has the publishing itself fail, not the command, as with a
// data directory it cannot write: the card needs you, with the reason, once,
// and has its moves again.
func TestPublishError(t *testing.T) {
	p := newPublishing(t)
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		p.publish(t, s, card, false)
	})
	if err := os.WriteFile(filepath.Join(p.cfg.Data, "publish"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e := newFakeEngine()
	core := p.start(t, e, 3)
	last := p.waitFor(t, card, reviewNeedsYou, "publishing failed: ")
	e.waitTicks(t, 20)
	core.stop(t)
	if !strings.Contains(last.Reason, "not a directory") || p.calls(t) != 0 {
		t.Errorf("the entry is %+v, and the command ran %d times", last, p.calls(t))
	}
	p.with(t, func(s *store.Store) {
		h, _ := s.History(card)
		if h[len(h)-1].Number != last.Number {
			t.Errorf("entries after the failure: %+v", h[len(h)-1])
		}
		c, _ := s.Card(card)
		if m, _ := s.Moves(c, true); len(m) == 0 || m[0] != (store.Allowed{Action: store.Again, Note: store.OptionalNote}) {
			t.Errorf("the moves after the failure: %v", m)
		}
	})
	if strings.Count(core.out.String(), "publishing failed") != 1 {
		t.Errorf("the core said\n%s", core.out)
	}
}

// TestMergedBySquash closes a card whose change the Git host merged as a
// squash commit of its own, on a main that had moved on meanwhile; a main
// that has only part of the change leaves the card at the Git host.
func TestMergedBySquash(t *testing.T) {
	for _, tc := range []struct {
		what   string
		merged map[string]string
		closed bool
	}{
		{"the whole change", map[string]string{"NOTES.md": "# Notes\nA line.\n", "b.txt": "b\n"}, true},
		{"part of it", map[string]string{"NOTES.md": "# Notes\nA line.\n"}, false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := newPublishing(t)
			var card int64
			p.with(t, func(s *store.Store) {
				card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n", "b.txt": "b\n"})
				p.publish(t, s, card, false)
			})
			e := newFakeEngine()
			core := p.start(t, e, 3)
			p.waitFor(t, card, atHost, "published as ")
			p.moveOrigin(t, map[string]string{"other.txt": "other work\n"})
			p.moveOrigin(t, tc.merged)
			if tc.closed {
				e := p.waitFor(t, card, closed, "")
				if e.Reason != "merged" || e.Cause != store.CauseHost {
					t.Errorf("the entry is %+v", e)
				}
			} else {
				from := time.Now()
				waitUntil(t, "five rounds of fetching", func() bool { return time.Since(from) > 5*fetchEvery })
				e.waitTicks(t, 5)
				p.waitFor(t, card, atHost, "published as ")
			}
			core.stop(t)
		})
	}
}

// TestWorkflowEntries counts as a change to the workflow files a .github or a
// .github/workflows that the change makes a symlink or a submodule.
func TestWorkflowEntries(t *testing.T) {
	ctx := t.Context()
	c, main := stagedCore(t)
	work := filepath.Join(t.TempDir(), "work")
	gitIn(t, filepath.Dir(work), "clone", "--quiet", c.Repo, work)
	if err := os.Symlink("ci", filepath.Join(work, ".github")); err != nil {
		t.Fatal(err)
	}
	write(t, work, map[string]string{"ci/workflows/check.yml": "on: push\n"})
	gitIn(t, work, "add", "--all")
	gitIn(t, work, "commit", "--quiet", "--message", "a symlink")
	gitIn(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/kiln/card-1")
	tip := gitIn(t, work, "rev-parse", "HEAD")
	if files, err := workflowFiles(ctx, c.Repo, tip); err != nil || !slices.Equal(files, []string{".github"}) {
		t.Errorf("a .github that is a symlink: %q, %v", files, err)
	}

	gitIn(t, work, "rm", "--quiet", ".github")
	gitIn(t, work, "update-index", "--add", "--cacheinfo", "160000,"+main+",.github/workflows")
	gitIn(t, work, "commit", "--quiet", "--message", "a submodule")
	gitIn(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/kiln/card-1")
	tip = gitIn(t, work, "rev-parse", "HEAD")
	if files, err := workflowFiles(ctx, c.Repo, tip); err != nil || !slices.Equal(files, []string{".github/workflows"}) {
		t.Errorf("a .github/workflows that is a submodule: %q, %v", files, err)
	}
}

// ended reports whether a process has ended: it is gone, or it is a zombie,
// dead and not yet reaped. A child whose parent was killed with it is the
// child of the first process then, which in a container, such as the one the
// check runs the tests in, often reaps nothing.
func ended(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return true
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false // no /proc, as on macOS: the kill above decides
	}
	// The state follows the command's name, which is in parentheses.
	_, rest, ok := strings.Cut(string(stat[bytes.LastIndexByte(stat, ')')+1:]), " ")
	return ok && strings.HasPrefix(rest, "Z")
}

// states reads a card's history from its last entry that waits for a review
// run on, as "stage/waits by cause" (ADR 0043), with each entry's reason.
func (p *publishing) states(t *testing.T, card int64) (lines, reasons []string) {
	t.Helper()
	p.look(t, func(s *store.Store) {
		h, err := s.History(card)
		if err != nil {
			t.Fatal(err)
		}
		from := 0
		for i, e := range h {
			if e.State == reviewWaits {
				from = i
			}
		}
		for _, e := range h[from:] {
			lines = append(lines, fmt.Sprintf("%s/%s by %s", e.State.Stage, e.State.WaitsFor, e.Cause))
			reasons = append(reasons, e.Reason)
		}
	})
	return lines, reasons
}

const ruleReason = "the review passed; the core publishes the change by the rule"

// TestPublishedByRule has a review pass with no move by the developer: the
// card goes to the Git host by the rule and is published, and the history
// names the rule.
func TestPublishedByRule(t *testing.T) {
	p := newPublishing(t)
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.reviewing(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
	})
	e := newFakeEngine()
	core := p.start(t, e, 3)
	e.open(card, "exit")
	got := p.waitFor(t, card, atHost, "published as ")
	core.stop(t)
	if got.Reason != "published as https://git.example/pull/1" {
		t.Errorf("the entry is %+v", got)
	}
	p.look(t, func(s *store.Store) {
		h, _ := s.History(card)
		for i, e := range h {
			if e.Cause == store.CauseDeveloper && i > 3 {
				t.Errorf("an entry by the developer after the approval of the spec: %+v", e)
			}
		}
	})
	lines, reasons := p.states(t, card)
	want := []string{"review/run by run", "review/approval by run", "review/host by rule", "review/host by rule"}
	if !slices.Equal(lines, want) {
		t.Errorf("the history is %q, want %q", lines, want)
	} else if reasons[2] != ruleReason || reasons[3] != "published as https://git.example/pull/1" {
		t.Errorf("the reasons are %q", reasons)
	}
	if n := p.calls(t); n != 1 {
		t.Errorf("the command ran %d times", n)
	}
	p.look(t, func(s *store.Store) {
		if pub, err := s.Publication(card); err != nil || pub.Branch != "card/1-add-a-line-to-the-notes" {
			t.Errorf("the publication is %+v (%v)", pub, err)
		}
	})
}

// TestPublishByRuleWorkflows has a change to a workflow file wait for the
// developer's yes: the rule does not publish it, and "Publish" with the yes
// does.
func TestPublishByRuleWorkflows(t *testing.T) {
	p := newPublishing(t)
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.reviewing(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n", ".github/workflows/check.yml": "on: push\n"})
	})
	e := newFakeEngine()
	core := p.start(t, e, 3)
	e.open(card, "exit")
	want := "the review passed; the change touches workflow files and waits for your yes before it is published: .github/workflows/check.yml"
	got := p.waitFor(t, card, reviewPassed, want)
	if got.Reason != want || got.Cause != store.CauseRule {
		t.Errorf("the entry is %+v", got)
	}
	e.waitTicks(t, 10)
	if n := p.calls(t); n != 0 {
		t.Errorf("the command ran %d times", n)
	}
	p.look(t, func(s *store.Store) {
		c, _ := s.Card(card)
		moves, err := s.Moves(c, true)
		if err != nil || len(moves) == 0 || moves[0].Action != store.Publish {
			t.Errorf("the moves are %v, %v", moves, err)
		}
		p.publish(t, s, card, true)
	})
	p.waitFor(t, card, atHost, "published as ")
	core.stop(t)
	if n := p.calls(t); n != 1 || p.env(t, 1)[envWorkflowYes] != "1" {
		t.Errorf("the command ran %d times, and was told %q", n, p.env(t, 1)[envWorkflowYes])
	}
}

// TestPassedWithoutPublishCommand leaves a card whose review passed waiting
// for approval when the core has no publish command.
func TestPassedWithoutPublishCommand(t *testing.T) {
	p := newPublishing(t)
	p.cfg.PublishCommand = ""
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.reviewing(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
	})
	e := newFakeEngine()
	core := p.start(t, e, 3)
	e.open(card, "exit")
	got := p.waitFor(t, card, reviewPassed, "")
	e.waitTicks(t, 5)
	core.stop(t)
	if got.Cause != store.CauseRun {
		t.Errorf("the entry is %+v, want the run's", got)
	}
	lines, _ := p.states(t, card)
	if want := []string{"review/run by run", "review/approval by run"}; !slices.Equal(lines, want) {
		t.Errorf("the history is %q, want %q", lines, want)
	}
	p.look(t, func(s *store.Store) {
		if c, _ := s.Card(card); c.State != reviewPassed {
			t.Errorf("the card is in %+v", c.State)
		}
	})
}

// TestPublishByRuleFails puts a card whose publishing by the rule fails under
// "needs you", with the command's reason, and "Publish" then works.
func TestPublishByRuleFails(t *testing.T) {
	p := newPublishing(t)
	p.mode(t, 1, "fail")
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.reviewing(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
	})
	e := newFakeEngine()
	core := p.start(t, e, 3)
	e.open(card, "exit")
	got := p.waitFor(t, card, reviewNeedsYou, "")
	if want := "publishing failed: the publish command failed: exit status 1: the Git host said no\n\uFFFD[2Jto card/1-add-a-line-to-the-notes"; got.Reason != want {
		t.Errorf("the reason is %q, want %q", got.Reason, want)
	}
	p.look(t, func(s *store.Store) {
		c, _ := s.Card(card)
		moves, err := s.Moves(c, true)
		if err != nil || !slices.Contains(moves, store.Allowed{Action: store.Publish, Note: store.NoNote}) {
			t.Errorf("the moves after a failure: %v, %v", moves, err)
		}
		p.publish(t, s, card, false)
	})
	p.waitFor(t, card, atHost, "published as https://git.example/pull/1")
	core.stop(t)
	if n := p.calls(t); n != 2 {
		t.Errorf("the command ran %d times, want 2", n)
	}
}

// TestPublishByRuleAfterConflict sends a change that collides with main back
// to implement, and publishes it by itself when its next review passes, on the
// branch name the first publishing would have had.
func TestPublishByRuleAfterConflict(t *testing.T) {
	p := newPublishing(t)
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.reviewing(t, s, map[string]string{"NOTES.md": "# Notes\nA line of the card.\n"})
	})
	p.moveOrigin(t, map[string]string{"NOTES.md": "# Notes\nA line of other work.\n"})
	e := newFakeEngine()
	core := p.start(t, e, 3)
	e.open(card, "exit")
	// The fake engine's implement run changes nothing, and needs the developer;
	// before it, the rule's step and the collision are in the history.
	p.waitFor(t, card, implementNeedsYou, "the run changed nothing")
	core.stop(t)
	if _, reasons := p.states(t, card); !slices.Contains(reasons, "main moved and the change no longer applies: NOTES.md") {
		t.Errorf("the reasons are %q, want the collision among them", reasons)
	}
	if n := p.calls(t); n != 0 {
		t.Errorf("the command ran %d times", n)
	}

	// The developer resolves the branch and has the run go again; the run, held
	// by the test, ends by hand at review, waiting for a run.
	p.resolve(t, card, map[string]string{"NOTES.md": "# Notes\nA line of other work.\nA line of the card.\n"})
	p.with(t, func(s *store.Store) {
		if _, err := s.Decide(card, store.Again, ""); err != nil {
			t.Fatal(err)
		}
	})
	e = newFakeEngine()
	core = p.start(t, e, 3)
	waitUntil(t, "the second implement run starts", func() bool { return slices.Contains(e.snapshot().started, card) })
	core.stop(t)
	p.with(t, func(s *store.Store) {
		if _, err := s.EndRun(fmt.Sprintf("%s-1-5", p.instance), store.Finished, "resolved by the test", reviewWaits, store.Cost{}); err != nil {
			t.Fatal(err)
		}
	})
	e = newFakeEngine()
	core = p.start(t, e, 3)
	e.open(card, "exit")
	p.waitFor(t, card, atHost, "published as ")
	core.stop(t)
	if n := p.calls(t); n != 1 {
		t.Errorf("the command ran %d times", n)
	}
	if got := p.env(t, 1)[envBranch]; got != "card/1-add-a-line-to-the-notes" {
		t.Errorf("the command was told the branch %q", got)
	}
	lines, _ := p.states(t, card)
	if n := len(lines); n < 4 || lines[n-2] != "review/host by rule" || lines[n-1] != "review/host by rule" {
		t.Errorf("the history is %q", lines)
	}
}

// TestMergingOffHoldsPublish has a review pass while automatic merging is off
// after a failed update: the rule does not publish the card, which waits for
// the developer, and the command never runs; "Publish" works as before.
func TestMergingOffHoldsPublish(t *testing.T) {
	p := newPublishing(t)
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.reviewing(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		if _, err := s.TurnMergingOff("the update to b failed: it exited; the core went back to a", "b"); err != nil {
			t.Fatal(err)
		}
	})
	e := newFakeEngine()
	core := p.start(t, e, 3)
	e.open(card, "exit")
	want := "the review passed; automatic merging is off after a failed update, so the rule does not publish it, and it waits for you"
	got := p.waitFor(t, card, reviewPassed, want)
	if got.Reason != want || got.Cause != store.CauseRule {
		t.Errorf("the entry is %+v", got)
	}
	e.waitTicks(t, 10)
	if n := p.calls(t); n != 0 {
		t.Errorf("the command ran %d times", n)
	}
	p.look(t, func(s *store.Store) {
		if c, _ := s.Card(card); c.State != reviewPassed {
			t.Errorf("the card is in %+v", c.State)
		}
		p.publish(t, s, card, false)
	})
	p.waitFor(t, card, atHost, "published as ")
	core.stop(t)
	if n := p.calls(t); n != 1 {
		t.Errorf("the command ran %d times", n)
	}
	lines, _ := p.states(t, card)
	if want := []string{"review/run by run", "review/approval by run", "review/approval by rule"}; len(lines) < 3 || !slices.Equal(lines[:3], want) {
		t.Errorf("the history is %q, want it to begin %q", lines, want)
	}
}

// The files of a card at the Git host whose change main collides with, the
// change of main that collides with it, and the reason of the rule's entry
// that publishes it again, after the published commit.
var (
	hostFiles     = map[string]string{"NOTES.md": "# Notes\nA line of the card.\n"}
	hostCollision = map[string]string{"NOTES.md": "# Notes\nA line of other work.\n"}
)

func publishAgainReason(commit string) string {
	return "main moved, and the published commit " + commit + " no longer merges onto it: NOTES.md; the core publishes the change again"
}

// atHost has a core on the engine publish a card that writes hostFiles, or
// the card stage writes and publishes, and waits until the card is at the Git
// host; it returns the card, the published commit and the core.
func (p *publishing) atHost(t *testing.T, e *fakeEngine, stage func(s *store.Store) int64) (int64, string, *running) {
	t.Helper()
	var card int64
	p.with(t, func(s *store.Store) {
		if stage != nil {
			card = stage(s)
			return
		}
		card = p.card(t, s, hostFiles)
		p.publish(t, s, card, false)
	})
	core := p.start(t, e, 3)
	p.waitFor(t, card, atHost, "published as ")
	return card, gitIn(t, p.cfg.Repo, "rev-parse", fmt.Sprintf("refs/kiln/publish/card-%d", card)), core
}

// countAt counts the calls of at with the point, for one test.
func countAt(t *testing.T, point string) *atomic.Int64 {
	var n atomic.Int64
	atPoint = func(p string) {
		if p == point {
			n.Add(1)
		}
	}
	t.Cleanup(func() { atPoint = nil })
	return &n
}

// fetchRounds waits for n rounds of fetching and n ticks.
func fetchRounds(t *testing.T, e *fakeEngine, n int) {
	t.Helper()
	from := time.Now()
	waitUntil(t, fmt.Sprintf("%d rounds of fetching", n), func() bool { return time.Since(from) > time.Duration(n)*fetchEvery })
	e.waitTicks(t, n)
}

// TestAtHostConflict has main move under a card at the Git host so that its
// published commit no longer merges: the rule publishes it again, and the
// publisher sends it back to implement with main merged into its branch and
// the files, as a send-back; the implement run starts from the merge and is
// told what changed in main, and the publish command does not run again.
func TestAtHostConflict(t *testing.T) {
	p := newPublishing(t)
	e := newFakeEngine()
	card, commit, core := p.atHost(t, e, nil)
	tip := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/kiln/card-1")
	main := p.moveOrigin(t, hostCollision)
	p.waitFor(t, card, implementWaits, conflictReason)
	waitUntil(t, "the implement run starts", func() bool { return slices.Contains(e.snapshot().started, card) })
	core.stop(t)

	lines, reasons := p.states(t, card)
	n := len(lines)
	if n < 2 || lines[n-2] != "review/host by rule" || reasons[n-2] != publishAgainReason(commit) ||
		lines[n-1] != "implement/run by rule" || reasons[n-1] != conflictReason+": NOTES.md" {
		t.Errorf("the history is %q, with the reasons %q", lines, reasons)
	}
	if !strings.Contains(core.out.String(), "card 1: "+publishAgainReason(commit)+"\n") {
		t.Errorf("the core said\n%s", core.out)
	}
	merge := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/kiln/card-1")
	if got := gitIn(t, p.cfg.Repo, "log", "--format=%P", "-1", merge); got != tip+" "+main {
		t.Errorf("the merge's parents are %q, want the tip %s and main %s", got, tip, main)
	}
	notes := gitIn(t, p.cfg.Repo, "show", merge+":NOTES.md")
	for _, want := range []string{"<<<<<<< refs/heads/kiln/card-1", ">>>>>>> refs/heads/main"} {
		if !strings.Contains(notes, want) {
			t.Errorf("NOTES.md of the merge lacks %q:\n%s", want, notes)
		}
	}
	run := fmt.Sprintf("%s-1-4", p.instance)
	p.look(t, func(s *store.Store) {
		if c, _ := s.Card(card); c.SendBacks != 1 {
			t.Errorf("the card has %d send-backs, want 1", c.SendBacks)
		}
		if r, err := s.Run(run); err != nil || r.Commit != merge || r.Model != "claude-opus-5-5" {
			t.Errorf("the implement run is %+v (%v), want it from the merge %s on claude-opus-5-5", r, err, merge)
		}
	})
	if told := string(readFile(t, filepath.Join(p.cfg.Data, "instructions", run+".md"))); !strings.Contains(told, "## What changed in main") {
		t.Errorf("the implement run is told\n%s", told)
	}
	if n := p.calls(t); n != 1 {
		t.Errorf("the command ran %d times, want 1", n)
	}
}

// TestAtHostStillMerges leaves a card at the Git host whose published commit
// still merges onto a main that moved.
func TestAtHostStillMerges(t *testing.T) {
	p := newPublishing(t)
	e := newFakeEngine()
	card, commit, core := p.atHost(t, e, nil)
	tip := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/kiln/card-1")
	p.moveOrigin(t, map[string]string{"other.txt": "other work\n"})
	fetchRounds(t, e, 5)
	core.stop(t)
	p.waitFor(t, card, atHost, "published as ")
	p.look(t, func(s *store.Store) {
		if c, _ := s.Card(card); c.SendBacks != 0 {
			t.Errorf("the card has %d send-backs", c.SendBacks)
		}
	})
	if got := gitIn(t, p.cfg.Repo, "rev-parse", "refs/heads/kiln/card-1"); got != tip {
		t.Errorf("the branch moved from %s to %s", tip, got)
	}
	if got := gitIn(t, p.cfg.Repo, "rev-parse", "refs/kiln/publish/card-1"); got != commit {
		t.Errorf("the published commit moved from %s to %s", commit, got)
	}
	if n := p.calls(t); n != 1 {
		t.Errorf("the command ran %d times, want 1", n)
	}
}

// TestAtHostMerged closes a card whose published commit is in origin's main,
// also when other work followed it, as merged, and does not send it back.
func TestAtHostMerged(t *testing.T) {
	for _, tc := range []struct {
		what  string
		other bool
	}{{"the commit", false}, {"the commit and other work", true}} {
		t.Run(tc.what, func(t *testing.T) {
			p := newPublishing(t)
			card, commit, core := p.atHost(t, newFakeEngine(), nil)
			core.stop(t)
			gitIn(t, p.cfg.Repo, "push", "--quiet", "origin", commit+":refs/heads/main")
			if tc.other {
				p.moveOrigin(t, map[string]string{"other.txt": "other work\n"})
			}
			core = p.start(t, newFakeEngine(), 3)
			last := p.waitFor(t, card, closed, "")
			core.stop(t)
			if last.Reason != store.MergedReason || last.Cause != store.CauseHost {
				t.Errorf("the entry is %+v", last)
			}
			if _, reasons := p.states(t, card); slices.ContainsFunc(reasons, func(r string) bool { return strings.HasPrefix(r, "main moved") }) {
				t.Errorf("the reasons are %q", reasons)
			}
			p.look(t, func(s *store.Store) {
				if c, _ := s.Card(card); c.SendBacks != 0 {
					t.Errorf("the card has %d send-backs", c.SendBacks)
				}
			})
		})
	}
}

// TestAtHostConflictThird has the published commit of a card that was sent
// back twice already stop merging: it waits in implement for the developer,
// with 3 send-backs, and no run starts.
func TestAtHostConflictThird(t *testing.T) {
	p := newPublishing(t)
	e := newFakeEngine()
	card, _, core := p.atHost(t, e, func(s *store.Store) int64 {
		card := p.card(t, s, hostFiles)
		if _, err := s.MoveSentBack(card, reviewPassed, reviewNeedsYou, "sent back by the test"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MoveSentBack(card, reviewNeedsYou, reviewPassed, "sent back by the test"); err != nil {
			t.Fatal(err)
		}
		p.publish(t, s, card, false)
		return card
	})
	p.moveOrigin(t, hostCollision)
	last := p.waitFor(t, card, implementNeedsYou, "")
	e.waitTicks(t, 3)
	core.stop(t)
	if want := conflictReason + ": NOTES.md; the card was sent back twice already, so it waits for you"; last.Reason != want {
		t.Errorf("the entry is %+v, want the reason %q", last, want)
	}
	p.look(t, func(s *store.Store) {
		if c, _ := s.Card(card); c.SendBacks != 3 {
			t.Errorf("the card has %d send-backs, want 3", c.SendBacks)
		}
	})
	if s := e.snapshot(); len(s.started) != 0 {
		t.Errorf("runs started for %v", s.started)
	}
}

// TestAtHostTestedOnce tests a card at the Git host once while main does not
// move, once more after it moved, and once at once in a core started again.
func TestAtHostTestedOnce(t *testing.T) {
	p := newPublishing(t)
	tested := countAt(t, "host-tested")
	e := newFakeEngine()
	_, _, core := p.atHost(t, e, nil)
	waitUntil(t, "the first test", func() bool { return tested.Load() >= 1 })
	fetchRounds(t, e, 10)
	if n := tested.Load(); n != 1 {
		t.Errorf("the card was tested %d times while main did not move, want 1", n)
	}
	p.moveOrigin(t, map[string]string{"other.txt": "other work\n"})
	waitUntil(t, "the test after main moved", func() bool { return tested.Load() >= 2 })
	fetchRounds(t, e, 5)
	core.stop(t)
	if n := tested.Load(); n != 2 {
		t.Errorf("the card was tested %d times after main moved once, want 2", n)
	}

	tested.Store(0)
	e = newFakeEngine()
	core = p.start(t, e, 3)
	waitUntil(t, "the test of the core started again", func() bool { return tested.Load() >= 1 })
	fetchRounds(t, e, 5)
	core.stop(t)
	if n := tested.Load(); n != 1 {
		t.Errorf("the core started again tested the card %d times, want 1", n)
	}
}

// TestAtHostPublishedAgain publishes a card sent back from the Git host
// because main moved, once it is resolved and its review passed, on the
// branch it was published on, and so onto the same pull request, with a
// commit on the new main.
func TestAtHostPublishedAgain(t *testing.T) {
	p := newPublishing(t)
	const kept = "card/1-kept-by-the-test"
	e := newFakeEngine()
	card, _, core := p.atHost(t, e, func(s *store.Store) int64 {
		card := p.card(t, s, hostFiles)
		p.publish(t, s, card, false)
		if _, err := s.PublishBranch(card, kept); err != nil {
			t.Fatal(err)
		}
		return card
	})
	main := p.moveOrigin(t, hostCollision)
	p.waitFor(t, card, implementWaits, conflictReason)
	waitUntil(t, "the implement run starts", func() bool { return slices.Contains(e.snapshot().started, card) })
	core.stop(t)

	p.resolve(t, card, map[string]string{"NOTES.md": "# Notes\nA line of other work.\nA line of the card.\n"})
	p.with(t, func(s *store.Store) {
		if _, err := s.EndRun(fmt.Sprintf("%s-1-4", p.instance), store.Finished, "resolved by the test", reviewWaits, store.Cost{}); err != nil {
			t.Fatal(err)
		}
	})
	e = newFakeEngine()
	core = p.start(t, e, 3)
	e.open(card, "exit")
	p.waitFor(t, card, atHost, "published as https://git.example/pull/1")
	core.stop(t)

	if _, reasons := p.states(t, card); slices.Index(reasons, "published as https://git.example/pull/1") < 0 {
		t.Errorf("the reasons are %q", reasons)
	}
	if n := p.calls(t); n != 2 {
		t.Fatalf("the command ran %d times, want 2", n)
	}
	first, second := p.env(t, 1), p.env(t, 2)
	if first[envBranch] != kept || second[envBranch] != kept {
		t.Errorf("the command was told the branches %q and %q, want %q", first[envBranch], second[envBranch], kept)
	}
	commit := second[envCommit]
	if got := gitIn(t, p.cfg.Repo, "log", "--format=%P", "-1", commit); got != main {
		t.Errorf("the second commit's parent is %q, want the new main %s", got, main)
	}
	p.look(t, func(s *store.Store) {
		if pub, err := s.Publication(card); err != nil || pub.Commit != commit || pub.Address != "https://git.example/pull/1" || pub.Branch != kept {
			t.Errorf("the publication is %+v (%v)", pub, err)
		}
	})
}

// TestAtHostAndCheckMoveOnce has a check fail for the published commit while
// the card goes back to implement because main moved: the failed check finds
// the card gone and writes nothing, and the card is sent back once.
func TestAtHostAndCheckMoveOnce(t *testing.T) {
	p := newChecking(t)
	p.checkMode(t, 1, "gate")
	checked := countAt(t, "checked")
	e := newFakeEngine()
	card, _, core := p.atHost(t, e, nil)
	waitUntil(t, "the check waits", func() bool { return p.exists("waiting") })
	p.moveOrigin(t, hostCollision)
	p.waitFor(t, card, implementWaits, conflictReason)
	waitUntil(t, "the implement run starts", func() bool { return slices.Contains(e.snapshot().started, card) })
	write(t, p.check, map[string]string{"gate": "failed"})
	waitUntil(t, "the check ends", func() bool { return checked.Load() >= 1 })
	e.waitTicks(t, 5)
	core.stop(t)

	if got := p.checkEntries(t, card); len(got) != 0 {
		t.Errorf("entries of the check: %+v", got)
	}
	p.look(t, func(s *store.Store) {
		c, _ := s.Card(card)
		if c.SendBacks != 1 || c.State != implementWaits {
			t.Errorf("the card is %+v, want it in implement with 1 send-back", c)
		}
		if n, err := s.RunCount(card); err != nil || n != 4 {
			t.Errorf("the card has %d runs (%v), want 4", n, err)
		}
	})
	if s := e.snapshot(); !slices.Equal(s.started, []int64{card}) {
		t.Errorf("runs started for %v, want one for %d", s.started, card)
	}
}
