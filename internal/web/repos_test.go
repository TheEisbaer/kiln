package web

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

// newReposFixture is a fixture whose handler is told the instance's repos, by
// name, each with a copy /data/<name>.git.
func newReposFixture(t *testing.T, names ...string) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var repos []Repo
	for _, n := range names {
		repos = append(repos, Repo{Name: n, Copy: "/data/" + n + ".git"})
	}
	act := NewActivity()
	h, err := Handler(s, Info{Instance: "ab12cd34", Repo: "/data/repo.git", Repos: repos, Model: "claude-opus-5-5", Runs: 3}, act, addr)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{store: s, act: act, h: h, path: path}
}

// TestCreateInRepo writes a card in the repo the body names, refuses a repo
// the instance does not have and, with more than one, a body that names
// none, and takes the one repo of an instance that has one.
func TestCreateInRepo(t *testing.T) {
	f := newReposFixture(t, "kiln", "second")
	w := f.do(t, "POST", "/api/cards", `{"text":"a card of the second repo","repo":"second"}`)
	if w.Code != 201 {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	c := decode[cardJSON](t, w)
	if c.Repo != "second" {
		t.Errorf("the card's JSON says the repo %q, want second", c.Repo)
	}
	if tl := f.tileOf(t, c.ID); tl.Repo != "second" {
		t.Errorf("the board's tile says the repo %q, want second", tl.Repo)
	}

	cards := len(decode[boardJSON](t, f.do(t, "GET", "/api/board", "")).Cards)
	for _, tc := range []struct{ body, want string }{
		{`{"text":"x"}`, `the instance has more than one repo; name one in "repo": kiln, second`},
		{`{"text":"x","repo":""}`, `the instance has more than one repo; name one in "repo": kiln, second`},
		{`{"text":"x","repo":"third"}`, `the instance has no repo "third"; its repos: kiln, second`},
	} {
		w := f.do(t, "POST", "/api/cards", tc.body)
		if got := decode[map[string]string](t, w)["error"]; w.Code != 400 || got != tc.want {
			t.Errorf("POST %s: %d %q, want 400 %q", tc.body, w.Code, got, tc.want)
		}
	}
	if b, _ := f.store.Board(); len(b) != cards {
		t.Errorf("refused bodies wrote cards: %d, want %d", len(b), cards)
	}

	for _, repos := range [][]string{nil, {"kiln"}} {
		f := newReposFixture(t, repos...)
		w := f.do(t, "POST", "/api/cards", `{"text":"a card"}`)
		if w.Code != 201 {
			t.Fatalf("POST with the repos %v: %d %s", repos, w.Code, w.Body)
		}
		if c := decode[cardJSON](t, w); c.Repo != store.KilnRepo {
			t.Errorf("with the repos %v the card is in %q, want kiln", repos, c.Repo)
		}
		w = f.do(t, "POST", "/api/cards", `{"text":"a card","repo":"other"}`)
		if got := decode[map[string]string](t, w)["error"]; w.Code != 400 || got != `the instance has no repo "other"; its repos: kiln` {
			t.Errorf("with the repos %v, a card of other: %d %q", repos, w.Code, got)
		}
	}
}

// TestPublishingPerRepo reads whether a card can be published from its own
// repo: with a publish command for kiln only, a review that passed of a card
// of kiln offers publish and takes it, and one of second does neither. A card
// that does not exist answers 404.
func TestPublishingPerRepo(t *testing.T) {
	f := newReposFixture(t)
	h, err := Handler(f.store, Info{Instance: "ab12cd34", Repo: "/data/kiln.git", Model: "m",
		Repos:      []Repo{{Name: "kiln", Copy: "/data/kiln.git"}, {Name: "second", Copy: "/data/second.git"}},
		Publishing: map[string]bool{"kiln": true}}, f.act, addr)
	if err != nil {
		t.Fatal(err)
	}
	f.h = h
	passed := store.State{Stage: store.Review, WaitsFor: store.Approval}
	ids := map[string]int64{}
	for _, repo := range []string{"kiln", "second"} {
		c, err := f.store.CreateIn(repo, "a card of "+repo, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.Move(c.ID, passed, store.CauseRun); err != nil {
			t.Fatal(err)
		}
		ids[repo] = c.ID
	}
	offers := func(id int64) bool {
		t.Helper()
		w := f.do(t, "GET", fmt.Sprintf("/api/cards/%d", id), "")
		if w.Code != 200 {
			t.Fatalf("GET card %d: %d %s", id, w.Code, w.Body)
		}
		return slices.ContainsFunc(decode[cardJSON](t, w).Moves, func(m moveJSON) bool { return m.Action == store.Publish })
	}
	if !offers(ids["kiln"]) {
		t.Error("the card of kiln, whose repo has a publish command, does not offer publish")
	}
	if offers(ids["second"]) {
		t.Error("the card of second, whose repo has no publish command, offers publish")
	}

	w := f.do(t, "POST", fmt.Sprintf("/api/cards/%d/decide", ids["second"]), `{"action":"publish"}`)
	if w.Code != 409 {
		t.Errorf("publish of the card of second: %d %s, want 409", w.Code, w.Body)
	}
	if c, _ := f.store.Card(ids["second"]); c.State != passed {
		t.Errorf("the card of second is %+v after a refused publish", c.State)
	}
	w = f.do(t, "POST", fmt.Sprintf("/api/cards/%d/decide", ids["kiln"]), `{"action":"publish"}`)
	if w.Code != 200 {
		t.Errorf("publish of the card of kiln: %d %s, want 200", w.Code, w.Body)
	}
	if c, _ := f.store.Card(ids["kiln"]); c.State != (store.State{Stage: store.Review, WaitsFor: store.Host}) {
		t.Errorf("the card of kiln is %+v after publish", c.State)
	}
	w = f.do(t, "POST", "/api/cards/99/decide", `{"action":"publish"}`)
	if got := decode[map[string]string](t, w)["error"]; w.Code != 404 || got != "no card 99" {
		t.Errorf("publish of a card that does not exist: %d %q, want 404 %q", w.Code, got, "no card 99")
	}
}

// TestBoardRepos answers every repo's name and copy in the board, in order,
// beside Kiln's own copy, and none for an instance told no repos.
func TestBoardRepos(t *testing.T) {
	b := decode[boardJSON](t, newReposFixture(t, "kiln", "second").do(t, "GET", "/api/board", ""))
	want := []repoJSON{{Name: "kiln", Copy: "/data/kiln.git"}, {Name: "second", Copy: "/data/second.git"}}
	if !reflect.DeepEqual(b.Repos, want) || b.Repo != "/data/repo.git" {
		t.Errorf("the board's repos are %+v and its repo %q, want %+v and /data/repo.git", b.Repos, b.Repo, want)
	}
	w := newReposFixture(t).do(t, "GET", "/api/board", "")
	if got := decode[map[string]any](t, w)["repos"]; !reflect.DeepEqual(got, []any{}) {
		t.Errorf("the board's repos without any: %#v, want []", got)
	}
}
