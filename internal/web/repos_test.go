package web

import (
	"path/filepath"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

// newReposFixture is a fixture whose handler is told the instance's repos.
func newReposFixture(t *testing.T, repos ...string) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
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
