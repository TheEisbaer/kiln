package web

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

// newReposFixture is a fixture whose handler is told the instance's repos, by
// name, each with a copy /data/<name>.git.
func newReposFixture(t *testing.T, names ...string) *fixture {
	t.Helper()
	return newLimitsFixture(t, store.Limits{}, names...)
}

// newLimitsFixture is newReposFixture with the repos' limits of cards in
// progress.
func newLimitsFixture(t *testing.T, limits store.Limits, names ...string) *fixture {
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
	h, err := Handler(s, Info{Instance: "ab12cd34", Repo: "/data/repo.git", Repos: repos, Model: "claude-opus-5-5", Runs: 3, Limits: limits}, act, addr)
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

// TestBoardRepos answers every repo's name, copy, limit and cards in progress
// in the board, in order, beside Kiln's own copy, and none for an instance
// told no repos. A card in the backlog or in done is not in progress. The
// instance's limit counts the card in spec and not the one at the Git host,
// and the page has the line of the limits.
func TestBoardRepos(t *testing.T) {
	f := newLimitsFixture(t, store.Limits{Repos: map[string]int{"kiln": 1}, Instance: 4}, "kiln", "second")
	for _, st := range []store.State{{Stage: store.Review, WaitsFor: store.Host}, {Stage: store.Spec, WaitsFor: store.Approval},
		{Stage: store.Backlog, WaitsFor: store.Developer}, {Stage: store.Done, WaitsFor: store.Nothing}} {
		c, err := f.store.CreateIn("kiln", "a card", 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.Move(c.ID, st, store.CauseRule); err != nil {
			t.Fatal(err)
		}
	}
	b := decode[boardJSON](t, f.do(t, "GET", "/api/board", ""))
	want := []repoJSON{{Name: "kiln", Copy: "/data/kiln.git", Limit: 1, InProgress: 2}, {Name: "second", Copy: "/data/second.git", Limit: 3}}
	if !reflect.DeepEqual(b.Repos, want) || b.Repo != "/data/repo.git" {
		t.Errorf("the board's repos are %+v and its repo %q, want %+v and /data/repo.git", b.Repos, b.Repo, want)
	}
	if want := (instanceLimitJSON{Limit: 4, InProgress: 1}); b.InstanceLimit != want {
		t.Errorf("the board's instance_limit is %+v, want %+v", b.InstanceLimit, want)
	}
	raw := decode[map[string]any](t, f.do(t, "GET", "/api/board", ""))
	if got, want := raw["instance_limit"], map[string]any{"limit": 4.0, "in_progress": 1.0}; !reflect.DeepEqual(got, want) {
		t.Errorf("the board's JSON has instance_limit %#v, want %#v", got, want)
	}
	page := f.do(t, "GET", "/", "").Body.String()
	for _, part := range []string{`id="limits"`, `"In progress: "`, `" not yet ready to merge"`} {
		if !strings.Contains(page, part) {
			t.Errorf("the page lacks %s", part)
		}
	}
	w := newReposFixture(t).do(t, "GET", "/api/board", "")
	if got := decode[map[string]any](t, w)["repos"]; !reflect.DeepEqual(got, []any{}) {
		t.Errorf("the board's repos without any: %#v, want []", got)
	}
}

// TestCreateUnderLimit has kiln at its limit of 1: a second card of kiln is
// queued for room, with the store's words as its reason, while a card of
// second starts; and the developer's start of a kiln card in the backlog
// queues it too.
func TestCreateUnderLimit(t *testing.T) {
	f := newLimitsFixture(t, store.Limits{Repos: map[string]int{"kiln": 1}}, "kiln", "second")
	if w := f.do(t, "POST", "/api/cards", `{"text":"the first","repo":"kiln"}`); w.Code != 201 || decode[cardJSON](t, w).Stage != store.Spec {
		t.Fatalf("the first card: %d %s", w.Code, w.Body)
	}
	w := f.do(t, "POST", "/api/cards", `{"text":"the second","repo":"kiln"}`)
	c := decode[cardJSON](t, w)
	if want := "waits for room in its repo kiln, whose limit is 1 cards in progress"; w.Code != 201 || c.Stage != store.Backlog ||
		c.WaitsFor != store.Room || c.Reason != want || len(c.QueuedFor) != 0 {
		t.Errorf("the second kiln card: %d %+v, want 201 queued with %q", w.Code, c.tileJSON, want)
	}
	if h := c.History; len(h) == 0 || h[len(h)-1].Note != "" {
		t.Errorf("the start's entry has a note: %+v", h)
	}
	w = f.do(t, "POST", "/api/cards", `{"text":"of second","repo":"second"}`)
	if c := decode[cardJSON](t, w); w.Code != 201 || c.Stage != store.Spec || c.WaitsFor != store.Run {
		t.Errorf("the card of second: %d %s, want 201 in spec waiting for a run", w.Code, w.Body)
	}

	b, err := f.store.CreateIn("kiln", "in the backlog", 0)
	if err != nil {
		t.Fatal(err)
	}
	w = f.do(t, "POST", fmt.Sprintf("/api/cards/%d/decide", b.ID), `{"action":"start"}`)
	if c := decode[cardJSON](t, w); w.Code != 200 || c.Stage != store.Backlog || c.WaitsFor != store.Room {
		t.Errorf("start of a kiln card: %d %s, want 200 queued", w.Code, w.Body)
	}
}

// TestReposPage holds the page's parts for more than one repo: the dialog's
// field Repo and what addCard sends, each option's cards in progress, the
// card page's "In" and the tile's label.
func TestReposPage(t *testing.T) {
	page := newFixture(t).do(t, "GET", "/", "").Body.String()
	for _, want := range []string{
		`id="d-repo"`, `<label for="d-repo">Repo</label>`, `body.repo = $("d-repo").value`, `" in progress"`,
		`"In " + c.repo`, `el("span", "repo-of", c.repo)`, `.tile .repo-of`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
}
