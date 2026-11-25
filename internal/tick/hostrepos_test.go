package tick

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/settings"
	"github.com/TheEisbaer/kiln/internal/store"
)

// twoSides is a bench of two repos at the Git host: kiln, Kiln's own, and
// second, each with its own bare origin, its own copy cloned from it, and its
// own stand-in publish command and check result command, each in a directory
// of its own. Both share the data directory and the store. Each side's
// publishing helpers write the cards of its own repo.
type twoSides struct {
	kiln, second *checking
}

func newTwoSides(t *testing.T) *twoSides {
	t.Helper()
	k := newChecking(t)
	origin, commit := bareRepo(t, map[string]string{"NOTES.md": "# Notes\n", "SECOND.md": "the second repo\n"})
	sb := &bench{cfg: k.cfg, instance: k.instance, commit: commit, clock: k.clock}
	sb.cfg.Repo = filepath.Join(t.TempDir(), "copy.git")
	gitIn(t, filepath.Dir(sb.cfg.Repo), "clone", "--quiet", "--bare", origin, sb.cfg.Repo)
	sp := &publishing{bench: sb, repo: "second", origin: origin, dir: t.TempDir(), data: k.data}
	sb.cfg.PublishCommand = fmt.Sprintf(publishCommand, sp.dir)
	second := &checking{publishing: sp, check: t.TempDir()}
	sb.cfg.CheckResultCommand = fmt.Sprintf(checkCommand, second.check)
	return &twoSides{kiln: k, second: second}
}

// config is the core's configuration: kiln's, with both repos as a list made
// from each side's copy and commands as they are now, and no flags of one
// repo.
func (ts *twoSides) config() Config {
	cfg := ts.kiln.cfg
	cfg.Repos = []settings.Repo{
		{Name: store.KilnRepo, Copy: ts.kiln.cfg.Repo, PublishCommand: ts.kiln.cfg.PublishCommand,
			CheckResultCommand: ts.kiln.cfg.CheckResultCommand, SpecHosts: ts.kiln.cfg.Hosts, Own: true},
		{Name: "second", Copy: ts.second.cfg.Repo, PublishCommand: ts.second.cfg.PublishCommand,
			CheckResultCommand: ts.second.cfg.CheckResultCommand},
	}
	cfg.Repo, cfg.PublishCommand, cfg.CheckResultCommand, cfg.Hosts = "", "", "", nil
	return cfg
}

// start starts a whole core of both repos.
func (ts *twoSides) start(t *testing.T, e *fakeEngine) *running {
	t.Helper()
	b := &bench{cfg: ts.config(), instance: ts.kiln.instance, commit: ts.kiln.commit, clock: ts.kiln.clock}
	return b.start(t, e, 3)
}

// published writes a card of each side whose review passed, and makes the
// move publish on both, kiln's first.
func (ts *twoSides) published(t *testing.T) (kiln, second int64) {
	t.Helper()
	return ts.kiln.published(t), ts.second.published(t)
}

// hasRef reports whether a copy has the ref.
func hasRef(t *testing.T, repo, ref string) bool {
	t.Helper()
	return gitIn(t, repo, "for-each-ref", "--format=%(refname)", ref) == ref
}

// checkEnv is what the nth call of a check result command was told, by name.
func (p *checking) checkEnv(t *testing.T, n int) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(string(readFile(t, filepath.Join(p.check, fmt.Sprintf("env.%d", n)))), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			out[name] = value
		}
	}
	return out
}

// TestPublishPerRepo publishes a card of each repo with its own repo's
// command, from its own repo's copy, whose ref it is kept in alone.
func TestPublishPerRepo(t *testing.T) {
	ts := newTwoSides(t)
	kiln, second := ts.published(t)
	core := ts.start(t, newFakeEngine())
	ts.kiln.waitFor(t, kiln, atHost, "published as ")
	ts.second.waitFor(t, second, atHost, "published as ")
	core.stop(t)

	for _, side := range []struct {
		p, other *checking
		card     int64
	}{{ts.kiln, ts.second, kiln}, {ts.second, ts.kiln, second}} {
		name := side.p.repo
		if n := side.p.calls(t); n != 1 {
			t.Errorf("%s's publish command ran %d times, want once", cmpName(name), n)
			continue
		}
		env := side.p.env(t, 1)
		if env[envRepo] != side.p.cfg.Repo || env[envCard] != fmt.Sprint(side.card) {
			t.Errorf("%s's publish command was told %s=%q and %s=%q, want %s and %d", cmpName(name), envRepo, env[envRepo],
				envCard, env[envCard], side.p.cfg.Repo, side.card)
		}
		ref := publishRef(side.card)
		if !hasRef(t, side.p.cfg.Repo, ref) || hasRef(t, side.other.cfg.Repo, ref) {
			t.Errorf("%s is in %s's copy: %v, and in the other's: %v, want only its own", ref, cmpName(name),
				hasRef(t, side.p.cfg.Repo, ref), hasRef(t, side.other.cfg.Repo, ref))
		}
		if e := side.p.waitFor(t, side.card, atHost, "published as "); e.Reason != fmt.Sprintf("published as https://git.example/pull/%d", side.card) {
			t.Errorf("the card of %s: %+v", cmpName(name), e)
		}
	}
	out := core.out.String()
	for _, want := range []string{
		"kiln core: repo kiln: a card is published with the publish command\n",
		"kiln core: repo kiln: a card at the Git host is checked with the check result command\n",
		"kiln core: repo second: a card is published with the publish command\n",
		"kiln core: repo second: a card at the Git host is checked with the check result command\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the core did not say %q:\n%s", want, out)
		}
	}
}

// cmpName is a side's name for a message.
func cmpName(repo string) string {
	if repo == "" {
		return store.KilnRepo
	}
	return repo
}

// TestCheckPerRepo reads each card's check with its own repo's command: the
// second repo's fails and sends its card back to implement, and Kiln's card,
// whose check is pending, stays at the Git host.
func TestCheckPerRepo(t *testing.T) {
	ts := newTwoSides(t)
	ts.second.checkMode(t, 0, "failed")
	kiln, second := ts.published(t)
	core := ts.start(t, newFakeEngine())
	last := ts.second.waitFor(t, second, implementWaits, store.CheckFailedReason)
	waitUntil(t, "kiln's card is checked", func() bool { return ts.kiln.checks(t) >= 1 })
	core.stop(t)

	if !strings.Contains(last.Reason, failedOutput) {
		t.Errorf("the second card's entry is %+v, want the second repo's check output in it", last)
	}
	ts.kiln.waitFor(t, kiln, atHost, "published as ")
	for _, side := range []struct {
		p    *checking
		card int64
	}{{ts.kiln, kiln}, {ts.second, second}} {
		commit := gitIn(t, side.p.cfg.Repo, "rev-parse", publishRef(side.card))
		n := side.p.checks(t)
		if n == 0 {
			t.Errorf("%s's check result command never ran", cmpName(side.p.repo))
		}
		for i := 1; i <= n; i++ {
			if got := side.p.checkEnv(t, i)[envCommit]; got != commit {
				t.Errorf("%s's check result command was told %s=%q in call %d, want its own card's %s", cmpName(side.p.repo), envCommit, got, i, commit)
			}
		}
	}
}

// TestPassedRulePerRepo publishes a card of the second repo whose review
// passed by the rule, from the second repo's copy alone; and with no publish
// command for the second repo, its card's passed review waits for approval
// while Kiln's card in the same core is published by the rule.
func TestPassedRulePerRepo(t *testing.T) {
	t.Run("published", func(t *testing.T) {
		ts := newTwoSides(t)
		var card int64
		ts.second.with(t, func(s *store.Store) {
			card = ts.second.reviewing(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		})
		kilnRefs := refs(t, ts.kiln.cfg.Repo)
		e := newFakeEngine()
		core := ts.start(t, e)
		e.open(card, "exit")
		ts.second.waitFor(t, card, atHost, "published as ")
		core.stop(t)
		lines, reasons := ts.second.states(t, card)
		if want := []string{"review/run by run", "review/approval by run", "review/host by rule", "review/host by rule"}; !slices.Equal(lines, want) || reasons[2] != ruleReason {
			t.Errorf("the history is %q, %q", lines, reasons)
		}
		if n, m := ts.second.calls(t), ts.kiln.calls(t); n != 1 || m != 0 {
			t.Errorf("the second repo's command ran %d times and Kiln's %d, want once and never", n, m)
		}
		if got := refs(t, ts.kiln.cfg.Repo); got != kilnRefs {
			t.Errorf("Kiln's own copy has the refs\n%s\nwas\n%s", got, kilnRefs)
		}
	})
	t.Run("no publish command", func(t *testing.T) {
		ts := newTwoSides(t)
		ts.second.cfg.PublishCommand = ""
		var kiln, second int64
		ts.kiln.with(t, func(s *store.Store) {
			kiln = ts.kiln.reviewing(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
			second = ts.second.reviewing(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		})
		e := newFakeEngine()
		core := ts.start(t, e)
		e.open(kiln, "exit")
		e.open(second, "exit")
		ts.kiln.waitFor(t, kiln, atHost, "published as ")
		got := ts.second.waitFor(t, second, reviewPassed, "")
		e.waitTicks(t, 5)
		core.stop(t)
		if got.Cause != store.CauseRun {
			t.Errorf("the second card's entry is %+v, want the run's", got)
		}
		if lines, _ := ts.second.states(t, second); !slices.Equal(lines, []string{"review/run by run", "review/approval by run"}) {
			t.Errorf("the second card's history is %q", lines)
		}
		if n := ts.kiln.calls(t); n != 1 {
			t.Errorf("Kiln's command ran %d times, want once", n)
		}
		out := core.out.String()
		if strings.Contains(out, "kiln core: repo second: a card is published") || !strings.Contains(out, "kiln core: repo kiln: a card is published with the publish command\n") {
			t.Errorf("the core said:\n%s", out)
		}
	})
}

// TestUpdateOnlyByOwnMerge has a core built by hand under a launcher: a card
// of the second repo that merges is closed, and the core builds nothing; a
// card of Kiln's own that merges then has it replaced by the program of
// Kiln's main.
func TestUpdateOnlyByOwnMerge(t *testing.T) {
	ts := newTwoSides(t)
	withImages(&ts.kiln.cfg)
	setFetchEvery(t, 20*time.Millisecond)
	gomod := imageTree()
	gomod["go.mod"] = "module " + kilnModule + "\n"
	ts.kiln.moveOrigin(t, gomod)
	ts.second.mode(t, 0, "merge")
	ts.kiln.mode(t, 0, "merge")
	second := ts.second.published(t)
	e := newFakeEngine()
	h := startHanding(t, ts.config(), e, true)
	if line := h.line(t); line != "healthy" {
		t.Fatalf("the core wrote %q", line)
	}
	ts.second.waitFor(t, second, closed, store.MergedReason)
	waitRounds(t, e, 10)
	h.noLine(t, 50*time.Millisecond)
	if e.mu.Lock(); len(e.builds) != 0 {
		t.Errorf("the core built %d times after a merge in the second repo", len(e.builds))
	}
	e.mu.Unlock()
	if !strings.Contains(h.out.String(), fmt.Sprintf("card %d: merged\n", second)) {
		t.Errorf("the core did not say the second card merged:\n%s", h.out)
	}

	var kiln int64
	ts.kiln.look(t, func(s *store.Store) {
		kiln = ts.kiln.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		ts.kiln.publish(t, s, kiln, false)
	})
	line := h.line(t)
	b := gitIn(t, ts.kiln.cfg.Repo, "rev-parse", "refs/heads/main")
	if want := "replace " + filepath.Join(ts.kiln.cfg.Data, "programs", "kiln-"+b); line != want {
		t.Fatalf("the core wrote %q, want %q\n%s", line, want, h.out)
	}
	h.returned(t)
	if !strings.Contains(h.out.String(), fmt.Sprintf("card %d: merged\n", kiln)) {
		t.Errorf("the core did not say Kiln's card merged:\n%s", h.out)
	}
}

// TestAtHostPerRepo tests the cards at the Git host of each repo against its
// own main: the second repo's main moves so that its card no longer merges,
// and that card is published again and sent back with main merged into its
// branch in the second repo's copy; Kiln's card is left as it was, and later
// closed by a merge on Kiln's origin.
func TestAtHostPerRepo(t *testing.T) {
	ts := newTwoSides(t)
	kiln, second := ts.published(t)
	core := ts.start(t, newFakeEngine())
	ts.kiln.waitFor(t, kiln, atHost, "published as ")
	ts.second.waitFor(t, second, atHost, "published as ")
	kilnBranch := gitIn(t, ts.kiln.cfg.Repo, "rev-parse", "refs/heads/kiln/card-"+fmt.Sprint(kiln))
	kilnPublished := gitIn(t, ts.kiln.cfg.Repo, "rev-parse", publishRef(kiln))
	secondTip := gitIn(t, ts.second.cfg.Repo, "rev-parse", "refs/heads/kiln/card-"+fmt.Sprint(second))

	moved := ts.second.moveOrigin(t, map[string]string{"NOTES.md": "# Notes\nA line of other work.\n"})
	ts.second.waitFor(t, second, implementWaits, conflictReason+": NOTES.md")
	merge := gitIn(t, ts.second.cfg.Repo, "rev-parse", "refs/heads/kiln/card-"+fmt.Sprint(second))
	if parents := gitIn(t, ts.second.cfg.Repo, "log", "-1", "--format=%P", merge); parents != secondTip+" "+moved {
		t.Errorf("the second card's branch is at %s with the parents %q, want a merge of %s and %s", merge, parents, secondTip, moved)
	}
	if got := gitIn(t, ts.kiln.cfg.Repo, "rev-parse", "refs/heads/kiln/card-"+fmt.Sprint(kiln)); got != kilnBranch {
		t.Errorf("Kiln's card's branch moved from %s to %s", kilnBranch, got)
	}
	if got := gitIn(t, ts.kiln.cfg.Repo, "rev-parse", publishRef(kiln)); got != kilnPublished {
		t.Errorf("Kiln's card's published ref moved from %s to %s", kilnPublished, got)
	}
	ts.kiln.waitFor(t, kiln, atHost, "published as ")

	gitIn(t, ts.kiln.cfg.Repo, "push", "--quiet", "origin", kilnPublished+":refs/heads/main")
	ts.kiln.waitFor(t, kiln, closed, store.MergedReason)
	core.stop(t)
}

// TestTestedPerRepo has one round at the Git host test a card of each repo:
// what each was tested with is kept, the round of the second repo does not
// drop Kiln's, and a card that has left the Git host is dropped.
func TestTestedPerRepo(t *testing.T) {
	ts := newTwoSides(t)
	kiln, second := ts.published(t)
	r := ts.start(t, newFakeEngine())
	ts.kiln.waitFor(t, kiln, atHost, "published as ")
	ts.second.waitFor(t, second, atHost, "published as ")
	r.stop(t)

	ts.kiln.with(t, func(s *store.Store) {
		ctx, cancel := context.WithCancel(t.Context())
		c := &core{Config: ts.config(), store: s, out: io.Discard, errOut: io.Discard, publishing: map[int64]bool{},
			checking: map[int64]*checkState{}, tested: map[int64]hostTest{}}
		defer func() { cancel(); c.followers.Wait() }()
		if err := c.atHostRound(ctx, nil); err != nil {
			t.Fatal(err)
		}
		for _, id := range []int64{kiln, second} {
			if _, ok := c.tested[id]; !ok {
				t.Errorf("card %d has no test kept after the round: %v", id, c.tested)
			}
		}
		if c.checked[store.KilnRepo] != 1 || c.checked["second"] != 1 {
			t.Errorf("the rounds tested are %v, want one of each repo", c.checked)
		}
		if _, err := s.Decide(second, store.MarkDone, ""); err != nil {
			t.Fatal(err)
		}
		if err := c.atHostRound(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.tested[second]; ok {
			t.Errorf("the second card, done, still has a test kept: %v", c.tested)
		}
		if _, ok := c.tested[kiln]; !ok {
			t.Errorf("Kiln's card has no test kept: %v", c.tested)
		}
	})
}

// TestTwoReposStart starts a core of two repos under a launcher, with no
// store before, and it reports healthy.
func TestTwoReposStart(t *testing.T) {
	ts := newTwoSides(t)
	h := startHanding(t, ts.config(), newFakeEngine(), true)
	if line := h.line(t); line != "healthy" {
		t.Fatalf("the core wrote %q", line)
	}
	h.ended(t)
}

// TestCompareEveryRepo reads the main of every repo before the report: the
// second repo's origin hangs, which is said once with its name, and the core
// still reports healthy.
func TestCompareEveryRepo(t *testing.T) {
	ts := newTwoSides(t)
	hangingOrigin(t, ts.second.cfg.Repo)
	was := compareLimit
	compareLimit = 200 * time.Millisecond
	t.Cleanup(func() { compareLimit = was })
	e := newFakeEngine()
	core := ts.start(t, e)
	waitUntil(t, "the proxy is kept", func() bool { e.mu.Lock(); defer e.mu.Unlock(); return len(e.keeps) > 0 })
	core.stop(t)
	out := core.out.String()
	line := "kiln core: the comparison before the report: main is not read at the Git host, which does not hold the report: repo second: "
	if n := strings.Count(out, line); n != 1 || !strings.Contains(out, "; healthy\n") {
		t.Errorf("the comparison's line for the second repo is there %d times:\n%s", n, out)
	}
	if strings.Contains(out, "does not hold the report: repo kiln: ") {
		t.Errorf("Kiln's own repo, which answers, was said:\n%s", out)
	}
}

// TestNotTestedNamesRepo: the line that says the cards at the Git host are not
// tested names the repo when the instance has more than one, and not when it
// has one, as the comparison's line does.
func TestNotTestedNamesRepo(t *testing.T) {
	broken := settings.Repo{Name: "second", Copy: t.TempDir()} // not a git repo
	published := map[int64]string{1: "abc"}

	var two strings.Builder
	c := &core{Config: Config{Repos: []settings.Repo{{Name: store.KilnRepo, Own: true}, broken}}, errOut: &two}
	c.testAtHost(t.Context(), broken, published)
	want := "kiln core: the cards at the Git host are not tested: repo second: git rev-parse"
	if !strings.HasPrefix(two.String(), want) {
		t.Errorf("two repos: want a line that starts with %q, got:\n%s", want, two.String())
	}

	var one strings.Builder
	c = &core{Config: Config{Repo: broken.Copy}, errOut: &one}
	c.testAtHost(t.Context(), broken, published)
	want = "kiln core: the cards at the Git host are not tested: git rev-parse"
	if !strings.HasPrefix(one.String(), want) {
		t.Errorf("one repo: want a line that starts with %q, got:\n%s", want, one.String())
	}
	if strings.Contains(one.String(), "repo ") {
		t.Errorf("one repo: the line names a repo:\n%s", one.String())
	}
}
