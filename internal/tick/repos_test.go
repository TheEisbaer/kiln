package tick

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/settings"
	"github.com/TheEisbaer/kiln/internal/store"
)

// twoRepos is a core on a store of its own and two local bare repos, kiln,
// Kiln's own, and second, whose mains differ, and those mains.
type twoRepos struct {
	c                    *core
	kiln, second         settings.Repo
	kilnMain, secondMain string
}

func newTwoRepos(t *testing.T) *twoRepos {
	t.Helper()
	data := t.TempDir()
	s, err := store.Open(filepath.Join(data, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	kilnCopy, kilnMain := bareRepo(t, map[string]string{"NOTES.md": "# Notes of kiln\n"})
	secondCopy, secondMain := bareRepo(t, map[string]string{"NOTES.md": "# Notes of the second repo\n"})
	r := &twoRepos{
		kiln:     settings.Repo{Name: store.KilnRepo, Copy: kilnCopy, SpecHosts: []string{"go.dev"}, Own: true},
		second:   settings.Repo{Name: "second", Copy: secondCopy, SpecHosts: []string{"example.org"}},
		kilnMain: kilnMain, secondMain: secondMain,
	}
	r.c = &core{Config: Config{Data: data, Repos: []settings.Repo{r.kiln, r.second}, Image: "kiln-run", Proxy: "kiln-proxy",
		Model: "claude-opus-5-5", Runs: 3}, store: s, instance: "ab12cd34", out: io.Discard, errOut: io.Discard,
		engine: newFakeEngine(), followed: map[int64]string{}, refused: map[int64]bool{}, stuck: map[string]stuckNote{},
		publishing: map[int64]bool{}, checking: map[int64]*checkState{}, tested: map[int64]hostTest{}}
	return r
}

// card writes a card of the repo waiting for a spec run, and returns it.
func (r *twoRepos) card(t *testing.T, repo string) store.Card {
	t.Helper()
	c, err := r.c.store.CreateIn(repo, "A card of "+repo+".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c, err = r.c.store.Move(c.ID, waiting, store.CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	return c
}

// approve ends a spec run of the card with a spec and moves it to the stage,
// waiting for a run, and returns the card.
func (r *twoRepos) approve(t *testing.T, card store.Card, to store.Stage) store.Card {
	t.Helper()
	id := fmt.Sprintf("ab12cd34-%d-1", card.ID)
	if err := r.c.store.AddRun(store.RunRow{ID: id, Card: card.ID, Commit: r.secondMain, Model: "m", Effort: "high"}, MaxRuns); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.store.EndRun(id, store.Finished, "", finished, store.Cost{}, &store.Doc{Kind: store.KindSpec, Text: "# The spec\n"}); err != nil {
		t.Fatal(err)
	}
	c, err := r.c.store.Move(card.ID, store.State{Stage: to, WaitsFor: store.Run}, store.CauseDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// refs are a copy's refs and where they are.
func refs(t *testing.T, repo string) string {
	t.Helper()
	return gitIn(t, repo, "for-each-ref", "--format=%(refname) %(objectname)")
}

// TestCardRunsOnItsRepo builds the spec, implement and review runs of a card
// of the second repo from that repo's copy, and leaves Kiln's own copy alone.
func TestCardRunsOnItsRepo(t *testing.T) {
	r := newTwoRepos(t)
	c, ctx := r.c, t.Context()
	kilnRefs := refs(t, r.kiln.Copy)
	card := r.card(t, "second")
	row := func(cd store.Card, commit string) store.RunRow {
		n, err := c.store.RunCount(cd.ID)
		if err != nil {
			t.Fatal(err)
		}
		return store.RunRow{ID: fmt.Sprintf("ab12cd34-%d-%d", cd.ID, n+1), Card: cd.ID, Commit: commit, Model: "m", Effort: stages[cd.Stage].effort}
	}

	// The spec run: the second repo's copy, main and hosts.
	commit, held, err := c.startsFrom(ctx, card)
	if err != nil || held != "" || commit != r.secondMain {
		t.Fatalf("the spec run starts from %q (%q, %v), want the second repo's main %s", commit, held, err, r.secondMain)
	}
	in, _, err := c.inputs(ctx, row(card, commit), card, false)
	if err != nil {
		t.Fatal(err)
	}
	if in.Repo != r.second.Copy || !slices.Equal(in.Hosts, []string{"example.org"}) || in.Commit != r.secondMain {
		t.Errorf("the spec run's inputs have the repo %s, the hosts %v and the commit %s, want %s, [example.org] and %s",
			in.Repo, in.Hosts, in.Commit, r.second.Copy, r.secondMain)
	}

	// The implement run: from the card's branch in the second repo's copy.
	card = r.approve(t, card, store.Implement)
	tip := commitOnCardBranch(t, r.second.Copy, r.secondMain, card.ID, map[string]string{"NOTES.md": "# Notes of the second repo\nA line of the card.\n"})
	if commit, held, err := c.startsFrom(ctx, card); err != nil || held != "" || commit != tip {
		t.Fatalf("the implement run starts from %q (%q, %v), want the branch's tip %s", commit, held, err, tip)
	}
	if in, _, err := c.inputs(ctx, row(card, tip), card, false); err != nil || in.Repo != r.second.Copy || in.Hosts != nil {
		t.Errorf("the implement run's inputs: %+v, %v", in, err)
	}

	// The review run: told the diff made in the second repo's copy.
	card, err = c.store.Move(card.ID, reviewWaits, store.CauseRun)
	if err != nil {
		t.Fatal(err)
	}
	if commit, _, err := c.startsFrom(ctx, card); err != nil || commit != tip {
		t.Fatalf("the review run starts from %q, %v, want %s", commit, err, tip)
	}
	in, held, err = c.inputs(ctx, row(card, tip), card, false)
	if err != nil || held != "" {
		t.Fatal(err, held)
	}
	if told := string(readFile(t, in.Instruction)); !strings.Contains(told, "+A line of the card.") {
		t.Errorf("the review run is not told the diff of the second repo:\n%s", told)
	}

	if got := refs(t, r.kiln.Copy); got != kilnRefs {
		t.Errorf("Kiln's own copy has the refs\n%s\nwas\n%s", got, kilnRefs)
	}
}

// commitOnCardBranch commits files on top of a commit of a bare repo and
// moves the card's branch to it, and returns the new commit.
func commitOnCardBranch(t *testing.T, repo, parent string, card int64, files map[string]string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	gitIn(t, filepath.Dir(work), "clone", "--quiet", repo, work)
	gitIn(t, work, "checkout", "--quiet", "--detach", parent)
	write(t, work, files)
	gitIn(t, work, "add", "--all")
	gitIn(t, work, "commit", "--quiet", "--message", "a save")
	gitIn(t, work, "push", "--quiet", "--force", "origin", fmt.Sprintf("HEAD:refs/heads/kiln/card-%d", card))
	return gitIn(t, work, "rev-parse", "HEAD")
}

// TestCardOfUnknownRepo starts no run for a card of a repo the instance does
// not have, and says which.
func TestCardOfUnknownRepo(t *testing.T) {
	r := newTwoRepos(t)
	card := r.card(t, "third")
	f, err := r.c.card(t.Context(), card.ID, true)
	want := fmt.Sprintf(`card %d is of the repo "third", which is not in the settings`, card.ID)
	if f != nil || err == nil || err.Error() != want {
		t.Fatalf("the card gave %v and %v, want the error %q", f, err, want)
	}
	if n, err := r.c.store.RunCount(card.ID); err != nil || n != 0 {
		t.Errorf("the card has %d runs, %v", n, err)
	}
	e := r.c.engine.(*fakeEngine)
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.inspected) != 0 || len(e.started) != 0 {
		t.Errorf("the engine was asked: %v inspected, %v started", e.inspected, e.started)
	}
}

// withOrigin gives a copy an origin of its own: the copy is cloned from a new
// bare origin made from it, and the function returns a function that moves
// that origin's main to a new commit, and returns the commit.
func withOrigin(t *testing.T, repo *settings.Repo) func() string {
	t.Helper()
	origin := repo.Copy
	repo.Copy = filepath.Join(t.TempDir(), "copy.git")
	gitIn(t, filepath.Dir(repo.Copy), "clone", "--quiet", "--bare", origin, repo.Copy)
	return func() string {
		work := filepath.Join(t.TempDir(), "work")
		gitIn(t, filepath.Dir(work), "clone", "--quiet", origin, work)
		write(t, work, map[string]string{"OTHER.md": "other work\n"})
		gitIn(t, work, "add", "--all")
		gitIn(t, work, "commit", "--quiet", "--message", "other work")
		gitIn(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/main")
		return gitIn(t, work, "rev-parse", "HEAD")
	}
}

// TestFetchPerRepo fetches the main of the repo whose card waits for a run
// from that repo's origin, and not Kiln's own, which no card needs.
func TestFetchPerRepo(t *testing.T) {
	r := newTwoRepos(t)
	moveKiln, moveSecond := withOrigin(t, &r.kiln), withOrigin(t, &r.second)
	r.c.Repos = []settings.Repo{r.kiln, r.second}
	card := r.card(t, "second")
	moveKiln()
	moved := moveSecond()

	ctx, cancel := context.WithCancel(t.Context())
	defer func() { cancel(); r.c.followers.Wait() }()
	fetch := r.c.fetchFor([]int64{card.ID}, map[int64]string{}, "")
	if !fetch["second"] || fetch[store.KilnRepo] {
		t.Fatalf("the tick fetches %v, want second only", fetch)
	}
	if err := r.c.atHostRound(ctx, fetch); err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, r.second.Copy, "rev-parse", "refs/heads/main"); got != moved {
		t.Errorf("the second repo's main is at %s, want its origin's %s", got, moved)
	}
	if got := gitIn(t, r.kiln.Copy, "rev-parse", "refs/heads/main"); got != r.kilnMain {
		t.Errorf("Kiln's own main is at %s, want %s, as before its origin moved", got, r.kilnMain)
	}
	if n := r.c.rounds(store.KilnRepo); n != 0 {
		t.Errorf("Kiln's own repo had %d rounds of fetching", n)
	}
	// A paused instance, and a card with its run in flight, fetch nothing.
	if f := r.c.fetchFor([]int64{card.ID}, map[int64]string{}, "paused"); len(f) != 0 {
		t.Errorf("a paused instance fetches %v", f)
	}
	if f := r.c.fetchFor([]int64{card.ID}, map[int64]string{card.ID: "ab12cd34-1-1"}, ""); len(f) != 0 {
		t.Errorf("a card with its run in flight fetches %v", f)
	}
}

// TestFetchWaitIsForAllRepos waits for the fetches of two hanging origins no
// longer than fetchWait in all.
func TestFetchWaitIsForAllRepos(t *testing.T) {
	r := newTwoRepos(t)
	hangingOrigin(t, r.kiln.Copy)
	hangingOrigin(t, r.second.Copy)
	was := fetchWait
	fetchWait = 300 * time.Millisecond
	t.Cleanup(func() { fetchWait = was })
	kiln, second := r.card(t, store.KilnRepo), r.card(t, "second")

	ctx, cancel := context.WithCancel(t.Context())
	defer func() { cancel(); r.c.followers.Wait() }()
	fetch := r.c.fetchFor([]int64{kiln.ID, second.ID}, map[int64]string{}, "")
	if !fetch["second"] || !fetch[store.KilnRepo] {
		t.Fatalf("the tick fetches %v, want both repos", fetch)
	}
	start := time.Now()
	if err := r.c.atHostRound(ctx, fetch); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took >= 600*time.Millisecond {
		t.Errorf("the tick waited %s for two fetches, with a wait of %s for all", took, fetchWait)
	}
}

// TestReposRefused refuses, at the core's start and before anything is made,
// a list of repos that the core cannot work with.
func TestReposRefused(t *testing.T) {
	kiln := settings.Repo{Name: store.KilnRepo, Copy: "/kiln.git", Own: true}
	second := settings.Repo{Name: "second", Copy: "/second.git"}
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"a list and --repo", Config{Repos: []settings.Repo{kiln}, Repo: "/repo.git"}, "each setting has one place"},
		{"a list and --spec-hosts", Config{Repos: []settings.Repo{kiln}, Hosts: []string{"go.dev"}}, "each setting has one place"},
		{"a name twice", Config{Repos: []settings.Repo{kiln, {Name: store.KilnRepo, Copy: "/b.git"}}}, "given twice"},
		{"a name with /", Config{Repos: []settings.Repo{{Name: "a/b", Copy: "/a.git", Own: true}}}, "is not only ASCII letters"},
		{"none own", Config{Repos: []settings.Repo{second}}, "no repo is marked as Kiln's own"},
		{"two own", Config{Repos: []settings.Repo{kiln, {Name: "second", Copy: "/second.git", Own: true}}}, "more than one repo is marked"},
		{"no copy", Config{Repos: []settings.Repo{{Name: store.KilnRepo, Own: true}}}, "has no copy"},
		{"no repo", Config{}, "no repo is given"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := filepath.Join(t.TempDir(), "data")
			cfg := tc.cfg
			cfg.Data, cfg.Model, cfg.Runs = data, "m", 3
			err := runCore(t.Context(), cfg, io.Discard, io.Discard, newFakeEngine(), time.Millisecond, realClock{}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the core returned %v, want a refusal with %q", err, tc.want)
			}
			if _, err := os.Stat(filepath.Join(data, "store.db")); !os.IsNotExist(err) {
				t.Errorf("the refused core made the store: %v", err)
			}
		})
	}
}

// TestOneRepoList starts a core with a list of one repo as with its flags: a
// spec run of a card starts on the list's copy, with its hosts.
func TestOneRepoList(t *testing.T) {
	b := newBench(t, 3)
	b.cfg.Repos = []settings.Repo{{Name: store.KilnRepo, Copy: b.cfg.Repo, SpecHosts: []string{"example.org"}, Own: true}}
	copy := b.cfg.Repo
	b.cfg.Repo, b.cfg.Hosts = "", nil
	var card int64
	b.with(t, func(s *store.Store) { card = b.card(t, s, store.Spec) })
	e := newFakeEngine()
	core := b.start(t, e, 3)
	waitUntil(t, "the spec run starts", func() bool { return slices.Contains(e.snapshot().started, card) })
	e.mu.Lock()
	in := e.inputs[card]
	e.mu.Unlock()
	if in.Repo != copy || !slices.Equal(in.Hosts, []string{"example.org"}) {
		t.Errorf("the spec run has the repo %s and the hosts %v, want %s and [example.org]", in.Repo, in.Hosts, copy)
	}
	e.open(card, "exit")
	e.waitGone(t, card)
	core.stop(t)
}

// TestChangeOfUnknownCard gives no change for a card the store does not have,
// so that a publish of it reaches the store and is answered 404.
func TestChangeOfUnknownCard(t *testing.T) {
	r := newTwoRepos(t)
	ch, err := r.c.change(t.Context(), 99)
	if ch != nil || err != nil {
		t.Errorf("the change of a card the store does not have is %v, %v, want nil, nil", ch, err)
	}
}

// TestRepoGoneRefused starts a core from its flags on a store that has an
// open card of a repo it does not have: the start is refused, naming the repo
// and the card, before anything outside the store is made.
func TestRepoGoneRefused(t *testing.T) {
	b := newBench(t, 3)
	b.cfg.Listen = freeAddress(t)
	var gone, open int64
	b.with(t, func(s *store.Store) {
		c, err := s.CreateIn("second", "A card of second.", 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Move(c.ID, waiting, store.CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		gone = c.ID
		open = b.card(t, s, store.Spec) // a card of kiln is no trouble
	})
	e := newFakeEngine()
	for range 2 {
		err := runCore(t.Context(), b.cfg, io.Discard, io.Discard, e, 5*time.Millisecond, realClock{}, nil)
		want := fmt.Sprintf(`the repo "second" is not in the settings, and its cards %d are not done or dropped`, gone)
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), fmt.Sprint(open)+" are") {
			t.Fatalf("the core returned %v, want a refusal with %q", err, want)
		}
	}
	e.mu.Lock()
	keeps, lists := len(e.keeps), e.lists
	e.mu.Unlock()
	if keeps != 0 || lists != 0 {
		t.Errorf("the refused core asked the engine: %d keeps, %d lists", keeps, lists)
	}
	if conn, err := net.DialTimeout("tcp", b.cfg.Listen, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Errorf("something listens on %s after the refusal", b.cfg.Listen)
	}
}

// TestRepoGoneDoneStarts has a dropped card of a repo the core does not have:
// the core starts, and the card's change says the repo is not in the settings.
func TestRepoGoneDoneStarts(t *testing.T) {
	b := newBench(t, 3)
	b.cfg.Listen = freeAddress(t)
	var ids []int64
	b.with(t, func(s *store.Store) {
		for range 2 {
			c, err := s.CreateIn("second", "A card of second.", 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Decide(c.ID, store.Drop, ""); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, c.ID)
		}
	})
	core := b.start(t, newFakeEngine(), 3)
	for _, id := range ids {
		var card struct {
			Change struct {
				Error string `json:"error"`
			} `json:"change"`
		}
		getJSON(t, fmt.Sprintf("http://%s/api/cards/%d", b.cfg.Listen, id), &card)
		if want := fmt.Sprintf(`card %d is of the repo "second", which is not in the settings`, id); card.Change.Error != want {
			t.Errorf("the change error of card %d is %q, want %q", id, card.Change.Error, want)
		}
	}
	core.stop(t)
}
