package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/store"
)

// addr is the address the test's handler is told it listens on.
const addr = "127.0.0.1:4747"

type fixture struct {
	store *store.Store
	act   *Activity
	h     http.Handler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	act := NewActivity()
	h, err := Handler(s, Info{Instance: "ab12cd34", Repo: "/data/repo.git", Model: "claude-opus-5-5", Runs: 3}, act, addr)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{store: s, act: act, h: h}
}

// do sends a request as the page would: to the listen address, and a POST
// with a JSON body and the page's Origin.
func (f *fixture) do(t *testing.T, method, path, body string, edit ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://"+addr)
	} else {
		r = httptest.NewRequest(method, "http://"+addr+path, nil)
	}
	for _, e := range edit {
		e(r)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("the body %q: %v", w.Body.String(), err)
	}
	return v
}

// card makes a card in the given state.
func (f *fixture) card(t *testing.T, text string, to store.State) int64 {
	t.Helper()
	c, err := f.store.Create(text)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Move(c.ID, to, store.CauseRule); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func TestPage(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, "GET", "/", "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Body.String(), "<title>Kiln</title>") {
		t.Fatalf("GET /: %d %s", w.Code, w.Header())
	}
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'sha256-", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("the policy %q lacks %q", csp, want)
		}
	}
	// The page loads nothing from the network.
	for _, bad := range []string{"src=\"http", "href=\"http", "@import", "url(http"} {
		if strings.Contains(string(page), bad) {
			t.Errorf("the page has %q", bad)
		}
	}
	if w := f.do(t, "GET", "/nothing", ""); w.Code != 404 {
		t.Errorf("GET /nothing: %d", w.Code)
	}
}

func TestBoard(t *testing.T) {
	f := newFixture(t)
	if b := decode[boardJSON](t, f.do(t, "GET", "/api/board", "")); b.Instance != "ab12cd34" || b.Repo != "/data/repo.git" ||
		b.Models != (modelsJSON{"claude-opus-5-5", "claude-opus-5-5", "claude-opus-5-5", "claude-opus-5-5"}) || b.Runs != 3 || b.Cards == nil || len(b.Cards) != 0 {
		t.Fatalf("an empty board: %+v", b)
	}
	spec := f.card(t, "\n  Write the screen  \nwith more", store.State{Stage: store.Spec, WaitsFor: store.Run})
	if err := f.store.AddRun(store.RunRow{ID: "r1", Card: spec, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EndRun("r1", store.Finished, "exit 0, not saved", store.State{Stage: store.Spec, WaitsFor: store.Approval}, ptr("# Spec")); err != nil {
		t.Fatal(err)
	}
	running := f.card(t, "Runs", store.State{Stage: store.Spec, WaitsFor: store.Run})
	long := f.card(t, longTitle+"\n\nMore text.", store.State{Stage: store.Spec, WaitsFor: store.Approval})
	if err := f.store.AddRun(store.RunRow{ID: "r2", Card: running, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}, 8); err != nil {
		t.Fatal(err)
	}

	w := f.do(t, "GET", "/api/board", "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("GET /api/board: %d %s", w.Code, w.Header())
	}
	b := decode[boardJSON](t, w)
	if len(b.Cards) != 3 {
		t.Fatalf("the cards: %+v", b.Cards)
	}
	if c := b.Cards[0]; c.ID != spec || c.Title != "Write the screen" || c.Headline != "Write the screen" || c.Stage != "spec" || c.WaitsFor != "approval" ||
		c.Reason != "exit 0, not saved" || c.Run != nil || !reflect.DeepEqual(c.Documents, []string{"spec"}) {
		t.Errorf("the card with a spec: %+v", c)
	}
	if c := b.Cards[1]; c.Run == nil || c.Run.ID != "r2" || c.Run.Model != "claude-opus-5-5" || c.Run.Since == nil ||
		time.Since(*c.Run.Since) > time.Minute || len(c.Documents) != 0 || c.Documents == nil {
		t.Errorf("the card with a run in flight: %+v, run %+v", c, c.Run)
	}
	if c := b.Cards[2]; c.ID != long || c.Title != longTitle || c.Headline != longHeadline || c.WaitsFor != "approval" {
		t.Errorf("the card with a long first line: %+v", c)
	}
}

// TestSendBacks checks that the board and the card's page carry the card's
// number of send-backs, and the board the model of an implement run after a
// send-back.
func TestSendBacks(t *testing.T) {
	f := newFixture(t)
	id := f.card(t, "Sent back", store.State{Stage: store.Review, WaitsFor: store.Run})
	other := f.card(t, "Not sent back", store.State{Stage: store.Implement, WaitsFor: store.Run})
	if err := f.store.AddRun(store.RunRow{ID: "r1", Card: id, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}, 8); err != nil {
		t.Fatal(err)
	}
	rule := store.Rule{To: store.State{Stage: store.Implement, WaitsFor: store.Run}, Reason: "sent back by the rule", SendBack: true}
	if _, err := f.store.EndRunByRule("r1", store.Finished, "goes back", store.State{Stage: store.Review, WaitsFor: store.Developer}, rule,
		&store.Doc{Kind: store.KindReview, Text: "Verdict: goes back\n"}); err != nil {
		t.Fatal(err)
	}
	w := f.do(t, "GET", "/api/board", "")
	if !strings.Contains(w.Body.String(), `"send_backs":1`) || !strings.Contains(w.Body.String(), `"sent_back":"claude-opus-5-5"`) {
		t.Errorf("the board does not carry send_backs or sent_back: %s", w.Body)
	}
	b := decode[boardJSON](t, w)
	if len(b.Cards) != 2 || b.Cards[0].ID != id || b.Cards[0].SendBacks != 1 || b.Cards[1].ID != other || b.Cards[1].SendBacks != 0 ||
		b.Models.SentBack != "claude-opus-5-5" {
		t.Errorf("the board: %+v, models %+v", b.Cards, b.Models)
	}
	if c := decode[cardJSON](t, f.do(t, "GET", fmt.Sprintf("/api/cards/%d", id), "")); c.SendBacks != 1 {
		t.Errorf("the card's page has %d send-backs, want 1", c.SendBacks)
	}
}

// TestBoardPaused checks that the board says why the instance is paused, and
// says nothing once the pause has ended, and that the page shows it in one
// line at the top.
func TestBoardPaused(t *testing.T) {
	f := newFixture(t)
	if b := decode[boardJSON](t, f.do(t, "GET", "/api/board", "")); b.Paused != "" {
		t.Errorf("a new instance is paused: %q", b.Paused)
	}
	const why = "the Claude credential is unavailable: the credential command printed nothing"
	if _, err := f.store.SetPaused(why); err != nil {
		t.Fatal(err)
	}
	if b := decode[boardJSON](t, f.do(t, "GET", "/api/board", "")); b.Paused != why {
		t.Errorf("the board says paused %q, want %q", b.Paused, why)
	}
	if _, err := f.store.SetPaused(""); err != nil {
		t.Fatal(err)
	}
	if b := decode[boardJSON](t, f.do(t, "GET", "/api/board", "")); b.Paused != "" {
		t.Errorf("after the pause the board says %q", b.Paused)
	}
	for _, want := range []string{`<div id="paused" class="offline" role="status" hidden></div>`, `"Paused: " + b.paused`} {
		if !strings.Contains(string(page), want) {
			t.Errorf("the page lacks %s", want)
		}
	}
}

// longTitle is a first line written as one long paragraph, and longHeadline
// what the strip shows of it.
const (
	longTitle    = "On the board, the strip at the top that lists the cards that need the developer or wait for their approval shows a card's whole first line as its title."
	longHeadline = "On the board, the strip at the top that lists the cards that need the developer or wait…"
)

func TestHeadline(t *testing.T) {
	// comma is a sentence whose 88th and 89th characters are ", ".
	comma := strings.Repeat("word ", 16) + "and too" + ", then more words that go past the end"
	url := "https://example.com/" + strings.Repeat("a", 100)
	for _, c := range []struct{ in, want string }{
		{"Write the screen", "Write the screen"},
		{longTitle, longHeadline},
		{"Fix index.html and go 1.27.4 builds. Then more.", "Fix index.html and go 1.27.4 builds."},
		{"Use e.g. the proxy. Then more.", "Use e.g. the proxy."},
		{`Say "done." Then more.`, `Say "done."`},
		{"Why? Because.", "Why?"},
		{url, string([]rune(url)[:89]) + "…"},
		{strings.Repeat("ä", 90), strings.Repeat("ä", 90)},
		{strings.Repeat("ä", 91), strings.Repeat("ä", 89) + "…"},
		{comma, strings.Repeat("word ", 16) + "and too…"},
		{"", ""},
	} {
		got := headline(c.in)
		if got != c.want {
			t.Errorf("headline(%q) = %q, want %q", c.in, got, c.want)
		}
		if n := utf8.RuneCountInString(got); n > 90 || !utf8.ValidString(got) {
			t.Errorf("headline(%q) = %q: %d runes, valid UTF-8 %v", c.in, got, n, utf8.ValidString(got))
		}
	}
	if n := utf8.RuneCountInString(longHeadline); n != 88 {
		t.Errorf("the long headline has %d runes, want 88", n)
	}
	if r := []rune(comma); string(r[87:89]) != ", " {
		t.Errorf("the comma case has %q at 88 and 89", string(r[87:89]))
	}
}

// TestBuild checks that the page carries the build it is served with, that
// the board says the same build, and that the served page differs from
// index.html in the build alone, with the policy's hashes still its own.
func TestBuild(t *testing.T) {
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(build) {
		t.Fatalf("the build %q is not 16 hex digits", build)
	}
	f := newFixture(t)
	w := f.do(t, "GET", "/", "")
	body := w.Body.String()
	attr := `data-build="` + build + `"`
	if !strings.Contains(body, `<html lang="en" `+attr+`>`) {
		t.Fatalf("GET / does not carry the build %s", build)
	}
	if b := decode[map[string]any](t, f.do(t, "GET", "/api/board", "")); b["build"] != build {
		t.Errorf("the board's build: %v, want %s", b["build"], build)
	}
	if strings.Replace(body, attr, `data-build=""`, 1) != string(page) {
		t.Error("the served page differs from index.html in more than the build")
	}
	csp := w.Header().Get("Content-Security-Policy")
	for _, tag := range []string{"script", "style"} {
		m := regexp.MustCompile(`(?s)<` + tag + `>(.*?)</` + tag + `>`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("the served page has no %s", tag)
		}
		sum := sha256.Sum256([]byte(m[1]))
		if want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(csp, want) {
			t.Errorf("the policy %q lacks the served %s's hash %s", csp, tag, want)
		}
	}
}

func ptr(s string) *store.Doc { return &store.Doc{Kind: store.KindSpec, Text: s} }

func TestCard(t *testing.T) {
	f := newFixture(t)
	// A document with a script and an escape sequence arrives unchanged, as
	// JSON text.
	doc := "# Spec\n<script>alert(1)</script>\x1b]0;title\x07\x1b[2J\n"
	id := f.card(t, "The card\n\nits text <b>", store.State{Stage: store.Spec, WaitsFor: store.Run})
	if err := f.store.AddRun(store.RunRow{ID: "r1", Card: id, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EndRun("r1", store.Finished, "exit 0", store.State{Stage: store.Spec, WaitsFor: store.Approval}, ptr(doc)); err != nil {
		t.Fatal(err)
	}

	w := f.do(t, "GET", fmt.Sprintf("/api/cards/%d", id), "")
	if w.Code != 200 {
		t.Fatalf("GET the card: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "\x1b") {
		t.Errorf("the body carries the document's markup or escape as it is: %q", w.Body)
	}
	c := decode[cardJSON](t, w)
	if c.ID != id || c.Title != "The card" || c.Text != "The card\n\nits text <b>" || c.Stage != "spec" || c.WaitsFor != "approval" ||
		c.Reason != "exit 0" || c.Run != nil || c.Note != "" || c.Dropped ||
		!reflect.DeepEqual(c.Moves, []moveJSON{{"approve", "optional"}, {"again", "required"}, {"drop", "optional"}}) {
		t.Errorf("the card: %+v", c)
	}
	if want := []documentJSON{{"spec", "r1", doc, ""}}; !reflect.DeepEqual(c.Documents, want) {
		t.Errorf("the documents: %+v, want %+v", c.Documents, want)
	}
	if len(c.History) != 3 || c.History[2].Run != "r1" || c.History[2].Cause != "run" || c.History[2].Reason != "exit 0" {
		t.Errorf("the history: %+v", c.History)
	}
	if len(c.Runs) != 1 || c.Runs[0].ID != "r1" || c.Runs[0].Ending != "finished" || c.Runs[0].Started == nil {
		t.Errorf("the runs: %+v", c.Runs)
	}
	if c.Activity == nil || len(c.Activity) != 0 {
		t.Errorf("the activity of a card without a run in flight: %+v", c.Activity)
	}

	// The activity of the run in flight, and only of it.
	if _, err := f.store.Decide(id, store.Approve, "the note"); err != nil {
		t.Fatal(err)
	}
	c = decode[cardJSON](t, f.do(t, "GET", fmt.Sprintf("/api/cards/%d", id), ""))
	if want := []documentJSON{{"spec", "r1", doc, "the note"}}; !reflect.DeepEqual(c.Documents, want) || c.Note != "" {
		t.Errorf("after an approval with a note: documents %+v (want %+v), note %q", c.Documents, want, c.Note)
	}
	if err := f.store.AddRun(store.RunRow{ID: "r2", Card: id, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}, 8); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	f.act.Add("r1", run.Activity{At: at, Kind: "text", Text: "of the ended run"})
	f.act.Add("r2", run.Activity{At: at, Kind: "tool", Tool: "Bash", Text: "go test ./..."})
	f.act.Add("r2", run.Activity{At: at, Kind: "text", Text: "<img src=x>"})
	c = decode[cardJSON](t, f.do(t, "GET", fmt.Sprintf("/api/cards/%d", id), ""))
	want := []activityJSON{{at, "tool", "Bash", "go test ./..."}, {at, "text", "", "<img src=x>"}}
	if !reflect.DeepEqual(c.Activity, want) || c.Run == nil || c.Run.ID != "r2" || len(c.Moves) != 0 {
		t.Errorf("the card with a run in flight: run %+v, activity %+v, moves %v", c.Run, c.Activity, c.Moves)
	}

	for _, path := range []string{"/api/cards/99", "/api/cards/x", "/api/cards/0", "/api/cards/-1"} {
		if w := f.do(t, "GET", path, ""); w.Code != 404 {
			t.Errorf("GET %s: %d", path, w.Code)
		}
	}
}

// TestChange checks the change in a card's answer: none for a card without a
// branch, the branch, its commits and its diff for one with a branch, and only
// the size and the command for one whose diff is over the limit.
func TestChange(t *testing.T) {
	f := newFixture(t)
	changes := map[int64]*Change{
		2: {Branch: "kiln/card-2", Repo: "/data/repo.git", Commits: []Commit{{"ab12cd3", "a save"}, {"0f0f0f0", "a first save"}},
			Diff: "diff --git a/NOTES.md b/NOTES.md\n+<script>\n", Size: 41, Command: "git --git-dir=/data/repo.git diff HEAD...kiln/card-2"},
		3: {Branch: "kiln/card-3", Repo: "/data/repo.git", Commits: []Commit{{"1234567", "a save"}}, Size: 2 << 20, TooLarge: true,
			Command: "git --git-dir=/data/repo.git diff HEAD...kiln/card-3"},
	}
	asked := []int64{}
	h, err := Handler(f.store, Info{Instance: "ab12cd34", Repo: "/data/repo.git", Model: "m",
		Change: func(_ context.Context, card int64) (*Change, error) {
			asked = append(asked, card)
			return changes[card], nil
		}}, f.act, addr)
	if err != nil {
		t.Fatal(err)
	}
	f.h = h
	for i, to := range []store.State{{Stage: store.Spec, WaitsFor: store.Approval}, {Stage: store.Review, WaitsFor: store.Approval},
		{Stage: store.Review, WaitsFor: store.Developer}} {
		if id := f.card(t, fmt.Sprintf("Card %d", i+1), to); id != int64(i+1) {
			t.Fatalf("card %d has the ID %d", i+1, id)
		}
	}
	get := func(id int64) (*changeJSON, string) {
		t.Helper()
		w := f.do(t, "GET", fmt.Sprintf("/api/cards/%d", id), "")
		if w.Code != 200 {
			t.Fatalf("GET card %d: %d %s", id, w.Code, w.Body)
		}
		return decode[cardJSON](t, w).Change, w.Body.String()
	}
	if ch, body := get(1); ch != nil || !strings.Contains(body, `"change":null`) {
		t.Errorf("a card without a branch has the change %+v", ch)
	}
	ch, body := get(2)
	want := &changeJSON{Branch: "kiln/card-2", Repo: "/data/repo.git", Commits: []commitJSON{{"ab12cd3", "a save"}, {"0f0f0f0", "a first save"}},
		Diff: changes[2].Diff, Size: 41, Command: "git --git-dir=/data/repo.git diff HEAD...kiln/card-2", Workflows: []string{}}
	if !reflect.DeepEqual(ch, want) || strings.Contains(body, "<script>") {
		t.Errorf("a card with a branch has the change %+v, want %+v", ch, want)
	}
	ch, _ = get(3)
	if ch == nil || !ch.TooLarge || ch.Diff != "" || ch.Size != 2<<20 || ch.Command != changes[3].Command || len(ch.Commits) != 1 {
		t.Errorf("a card whose diff is over the limit has the change %+v", ch)
	}
	if !reflect.DeepEqual(asked, []int64{1, 2, 3}) {
		t.Errorf("the change was asked for the cards %v", asked)
	}

	// A change that cannot be read is said, and the card is shown.
	h, _ = Handler(f.store, Info{Change: func(context.Context, int64) (*Change, error) {
		return nil, errors.New("git log: \x1b[2Jbroken")
	}}, f.act, addr)
	f.h = h
	if ch, _ := get(2); ch == nil || !strings.Contains(ch.Error, "broken") || strings.Contains(ch.Error, "\x1b") {
		t.Errorf("a change that cannot be read: %+v", ch)
	}
}

func TestCreate(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, "POST", "/api/cards", `{"text":"Add a screen\n\nwith a board"}`)
	if w.Code != 201 {
		t.Fatalf("POST /api/cards: %d %s", w.Code, w.Body)
	}
	c := decode[cardJSON](t, w)
	if c.ID != 1 || c.Title != "Add a screen" || c.Stage != "spec" || c.WaitsFor != "run" {
		t.Errorf("the new card: %+v", c)
	}
	// As `kiln card` writes it: created, then waiting in spec for a run, both
	// by the developer.
	h, err := f.store.History(1)
	if err != nil || len(h) != 2 || h[0].State != (store.State{Stage: store.Backlog, WaitsFor: store.Developer}) ||
		h[1].State != (store.State{Stage: store.Spec, WaitsFor: store.Run}) || h[1].Cause != store.CauseDeveloper {
		t.Errorf("the history: %+v, %v", h, err)
	}
	if ids, _ := f.store.Waiting(); !reflect.DeepEqual(ids, []int64{1}) {
		t.Errorf("Waiting: %v", ids)
	}
	for _, body := range []string{`{"text":""}`, `{"text":"  \n"}`, `{}`, `not json`, `{"text":1}`} {
		if w := f.do(t, "POST", "/api/cards", body); w.Code != 400 {
			t.Errorf("POST %s: %d", body, w.Code)
		}
	}
	if w := f.do(t, "POST", "/api/cards", `{"text":"`+strings.Repeat("x", maxBody)+`"}`); w.Code != 400 {
		t.Errorf("a body over the limit: %d", w.Code)
	}
	if b, _ := f.store.Board(); len(b) != 1 {
		t.Errorf("refused bodies wrote cards: %+v", b)
	}
}

// TestDecide sends every row of the table, with a note where it takes one,
// moves that are not in it, and notes that do not fit.
func TestDecide(t *testing.T) {
	f := newFixture(t)
	post := func(id int64, body string) *httptest.ResponseRecorder {
		return f.do(t, "POST", fmt.Sprintf("/api/cards/%d/decide", id), body)
	}
	spec := store.State{Stage: store.Spec, WaitsFor: store.Approval}
	passed := store.State{Stage: store.Review, WaitsFor: store.Approval}
	for _, row := range []struct {
		action, note string
		from, to     store.State
	}{
		{"approve", "", spec, store.State{Stage: store.Implement, WaitsFor: store.Run}},
		{"again", "Name the table.", spec, store.State{Stage: store.Spec, WaitsFor: store.Run}},
		{"again", "", store.State{Stage: store.Spec, WaitsFor: store.Developer}, store.State{Stage: store.Spec, WaitsFor: store.Run}},
		{"again", "", store.State{Stage: store.Implement, WaitsFor: store.Developer}, store.State{Stage: store.Implement, WaitsFor: store.Run}},
		{"again", "Try once more.", store.State{Stage: store.Review, WaitsFor: store.Developer}, store.State{Stage: store.Review, WaitsFor: store.Run}},
		{"send-back", "", store.State{Stage: store.Review, WaitsFor: store.Developer}, store.State{Stage: store.Implement, WaitsFor: store.Run}},
		{"send-back", "Keep the old name.", passed, store.State{Stage: store.Implement, WaitsFor: store.Run}},
		{"done", "", passed, store.State{Stage: store.Done, WaitsFor: store.Nothing}},
		{"to-spec", "The spec no longer fits.", store.State{Stage: store.Implement, WaitsFor: store.Developer}, store.State{Stage: store.Spec, WaitsFor: store.Run}},
		{"to-spec", "", store.State{Stage: store.Review, WaitsFor: store.Developer}, store.State{Stage: store.Spec, WaitsFor: store.Run}},
		{"drop", "", store.State{Stage: store.Backlog, WaitsFor: store.Developer}, store.State{Stage: store.Done, WaitsFor: store.Nothing}},
		{"drop", "Not needed.", store.State{Stage: store.Spec, WaitsFor: store.Run}, store.State{Stage: store.Done, WaitsFor: store.Nothing}},
	} {
		id := f.card(t, "a card", row.from)
		body, _ := json.Marshal(map[string]string{"action": row.action, "note": "  " + row.note + "\n"})
		w := post(id, string(body))
		c := decode[cardJSON](t, w)
		if w.Code != 200 || c.Stage != row.to.Stage || c.WaitsFor != row.to.WaitsFor {
			t.Errorf("%s from %v: %d %+v", row.action, row.from, w.Code, c)
		}
		if got, _ := f.store.Card(id); got.State != row.to {
			t.Errorf("%s from %v: the store has %v", row.action, row.from, got.State)
		}
		// The note is the developer's words in the history, and the card's
		// note while it waits for the run that follows.
		e := c.History[len(c.History)-1]
		if e.Note != row.note || e.Cause != "developer" || e.Dropped != (row.action == "drop") || c.Dropped != e.Dropped {
			t.Errorf("%s from %v: the last entry is %+v, dropped %v", row.action, row.from, e, c.Dropped)
		}
		if want := map[bool]string{true: row.note}[row.to.WaitsFor == store.Run]; c.Note != want {
			t.Errorf("%s from %v: the card's note is %q, want %q", row.action, row.from, c.Note, want)
		}
	}

	refused := func(id int64, body string, from store.State, code int, what string) map[string]any {
		t.Helper()
		before, _ := f.store.History(id)
		w := post(id, body)
		got := decode[map[string]any](t, w)
		if w.Code != code {
			t.Errorf("%s: %d %v; want %d", what, w.Code, got, code)
		}
		if after, _ := f.store.History(id); len(after) != len(before) {
			t.Errorf("%s was refused and wrote an entry", what)
		}
		if c, _ := f.store.Card(id); c.State != from {
			t.Errorf("%s was refused and moved the card to %v", what, c.State)
		}
		return got
	}
	for _, row := range []struct {
		action string
		from   store.State
	}{
		{"approve", store.State{Stage: store.Spec, WaitsFor: store.Run}},         // a spec not written yet
		{"send-back", store.State{Stage: store.Spec, WaitsFor: store.Developer}}, // not in review
		{"drop", store.State{Stage: store.Done, WaitsFor: store.Nothing}},        // done already
		{"shelve", store.State{Stage: store.Review, WaitsFor: store.Developer}},  // no such move
	} {
		id := f.card(t, "a card", row.from)
		got := refused(id, `{"action":"`+row.action+`"}`, row.from, 409, fmt.Sprintf("%s from %v", row.action, row.from))
		if got["stage"] != string(row.from.Stage) || got["waits_for"] != string(row.from.WaitsFor) {
			t.Errorf("%s from %v: %v; want the card's state", row.action, row.from, got)
		}
	}

	// A drop of a card with a run in flight is refused as a move not in the
	// table, and the moves it is answered with have no drop.
	inFlight := f.card(t, "a card", store.State{Stage: store.Implement, WaitsFor: store.Run})
	if err := f.store.AddRun(store.RunRow{ID: "r9", Card: inFlight, Commit: "c0ffee", Model: "claude-sonnet-5-5", Effort: "medium"}, 8); err != nil {
		t.Fatal(err)
	}
	if got := refused(inFlight, `{"action":"drop","note":"x"}`, store.State{Stage: store.Implement, WaitsFor: store.Run}, 409, "a drop in flight"); fmt.Sprint(got["moves"]) != "[]" {
		t.Errorf("the moves of a card with a run in flight: %v", got["moves"])
	}

	// A note that does not fit is refused with 400 and a sentence.
	for _, tc := range []struct {
		what, body string
		from       store.State
		want       string
	}{
		{"a spec sent back without a note", `{"action":"again"}`, spec, "this move needs a note that says what should change"},
		{"a spec sent back with white space", `{"action":"again","note":" \n\t "}`, spec, "this move needs a note that says what should change"},
		{"a passing review sent back without a note", `{"action":"send-back","note":""}`, passed, "this move needs a note that says what should change"},
		{"an approval with a note too long", `{"action":"approve","note":"` + strings.Repeat("a", 16<<10+1) + `"}`, spec, "the note is 16.1 KiB, more than 16 KiB"},
		{"done with a note", `{"action":"done","note":"fine"}`, passed, "this move takes no note"},
		{"a note too long", `{"action":"again","note":"` + strings.Repeat("a", 16<<10+1) + `"}`, spec, "the note is 16.1 KiB, more than 16 KiB"},
	} {
		id := f.card(t, "a card", tc.from)
		if got := refused(id, tc.body, tc.from, 400, tc.what); got["error"] != tc.want {
			t.Errorf("%s: the error is %q, want %q", tc.what, got["error"], tc.want)
		}
	}

	// An approval with a note, for a card that has no spec, is refused with
	// 409 and nothing changes.
	{
		id := f.card(t, "a card", spec)
		refused(id, `{"action":"approve","note":"fine"}`, spec, 409, "an approval with a note and no spec")
	}

	if w := f.do(t, "POST", "/api/cards/99/decide", `{"action":"approve"}`); w.Code != 404 {
		t.Errorf("a move on no card: %d", w.Code)
	}
	if w := f.do(t, "POST", "/api/cards/1/decide", `nope`); w.Code != 400 {
		t.Errorf("a move that is not JSON: %d", w.Code)
	}
}

// TestRefusals sends the requests that only this machine's page may send
// from somewhere else.
func TestRefusals(t *testing.T) {
	f := newFixture(t)
	f.card(t, "a card", store.State{Stage: store.Spec, WaitsFor: store.Approval})
	post := `{"text":"from elsewhere"}`

	// A Host that is not the listen address, as a name rebound to 127.0.0.1
	// gives, for a read and for a POST.
	for _, host := range []string{"evil.example:4747", "127.0.0.1:4748", "localhost", "[::1]:4747", ""} {
		set := func(r *http.Request) { r.Host = host }
		if w := f.do(t, "GET", "/api/board", "", set); w.Code != 403 {
			t.Errorf("GET with Host %q: %d", host, w.Code)
		}
		if w := f.do(t, "POST", "/api/cards", post, set); w.Code != 403 {
			t.Errorf("POST with Host %q: %d", host, w.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:4747", "localhost:4747"} {
		if w := f.do(t, "GET", "/api/board", "", func(r *http.Request) { r.Host = host }); w.Code != 200 {
			t.Errorf("GET with Host %q: %d", host, w.Code)
		}
	}

	// A POST that is not JSON, as a form sends it.
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		set := func(r *http.Request) { r.Header.Set("Content-Type", ct) }
		if w := f.do(t, "POST", "/api/cards", post, set); w.Code != 403 {
			t.Errorf("POST with Content-Type %q: %d", ct, w.Code)
		}
	}
	if w := f.do(t, "POST", "/api/cards", post, func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
	}); w.Code != 201 {
		t.Errorf("POST with JSON and a charset: %d", w.Code)
	}

	// An Origin that is not the page's.
	for _, origin := range []string{"https://evil.example", "null", "http://localhost:4747", "https://127.0.0.1:4747"} {
		set := func(r *http.Request) { r.Header.Set("Origin", origin) }
		if w := f.do(t, "POST", "/api/cards", post, set); w.Code != 403 {
			t.Errorf("POST with Origin %q: %d", origin, w.Code)
		}
		if w := f.do(t, "POST", "/api/cards/1/decide", `{"action":"approve"}`, set); w.Code != 403 {
			t.Errorf("a move with Origin %q: %d", origin, w.Code)
		}
	}
	// Without an Origin, as a tool on this machine sends it.
	if w := f.do(t, "POST", "/api/cards", post, func(r *http.Request) { r.Header.Del("Origin") }); w.Code != 201 {
		t.Errorf("POST without an Origin: %d", w.Code)
	}

	if b, _ := f.store.Board(); len(b) != 3 {
		t.Errorf("the refused requests changed the store: %d cards", len(b))
	}
	if c, _ := f.store.Card(1); c.WaitsFor != store.Approval {
		t.Errorf("a refused move moved the card: %+v", c)
	}
}

func TestCheckAddress(t *testing.T) {
	for _, a := range []string{"127.0.0.1:4747", "127.0.0.2:80", "localhost:4747", "[::1]:4747", "127.0.0.1:0"} {
		if err := CheckAddress(a); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	for _, a := range []string{"0.0.0.0:4747", ":4747", "[::]:4747", "192.168.1.2:4747", "example.com:4747", "127.0.0.1", "127.0.0.1:x", "localhost:99999"} {
		if err := CheckAddress(a); err == nil {
			t.Errorf("%s was accepted", a)
		}
	}
}

func TestActivityRing(t *testing.T) {
	a := NewActivity()
	for i := range 250 {
		a.Add("r", run.Activity{Text: fmt.Sprint(i)})
	}
	got := a.Of("r")
	if len(got) != 200 || got[0].Text != "50" || got[199].Text != "249" {
		t.Errorf("the ring holds %d, from %s to %s", len(got), got[0].Text, got[len(got)-1].Text)
	}
	if a.Of("other") != nil {
		t.Error("another run has activities")
	}
	a.Drop("r")
	if a.Of("r") != nil {
		t.Error("a dropped run has activities")
	}
}

// TestServe serves the handler on a real listener, as the core does.
func TestServe(t *testing.T) {
	f := newFixture(t)
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, err := Handler(f.store, Info{}, f.act, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/cards", "application/json", strings.NewReader(`{"text":"over the wire"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Errorf("POST over the wire: %d %s", resp.StatusCode, body)
	}
	if _, err := Listen("0.0.0.0:0"); err == nil {
		t.Error("Listen accepted an address that is not loopback")
	}
}

// TestPageFromFile serves the page from a file, as `kiln core --page` does: a
// change to the file shows at the next request, with a policy of its own, and
// a file that goes wrong gives an error, never the embedded page.
func TestPageFromFile(t *testing.T) {
	f := newFixture(t)
	get := func(h http.Handler) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://"+addr+"/", nil))
		return w
	}
	// Without a file, the embedded page and its policy, as before.
	if w := get(f.h); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), served) || w.Header().Get("Content-Security-Policy") != policy {
		t.Fatalf("the embedded page: %d %s", w.Code, w.Header())
	}
	file := filepath.Join(t.TempDir(), "page.html")
	h, err := Handler(f.store, Info{Page: file}, f.act, addr)
	if err != nil {
		t.Fatal(err)
	}
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("<title>One</title><style>p{}</style><script>let a</script>")
	first := get(h)
	if first.Code != 200 || first.Body.String() != "<title>One</title><style>p{}</style><script>let a</script>" {
		t.Fatalf("the first page: %d %q", first.Code, first.Body)
	}
	two := "<title>Two</title><style>p{}</style><script>let b</script>"
	write(two)
	second := get(h)
	want, err := policyOf([]byte(two))
	if err != nil {
		t.Fatal(err)
	}
	csp := second.Header().Get("Content-Security-Policy")
	if second.Code != 200 || second.Body.String() != two {
		t.Fatalf("the changed page: %d %q", second.Code, second.Body)
	}
	if csp != want || csp == first.Header().Get("Content-Security-Policy") {
		t.Errorf("the changed page's policy %q, want %q", csp, want)
	}
	// A file that goes wrong while the core runs.
	write("<title>Three</title><style>p{}</style>")
	for _, why := range []string{"a page without a script", "a removed page"} {
		w := get(h)
		if w.Code != 500 || w.Header().Get("Content-Security-Policy") != "" || !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("%s: %d %s %q", why, w.Code, w.Header(), w.Body)
		}
		os.Remove(file)
	}
}

// TestReadPage refuses a file that cannot be read.
func TestReadPage(t *testing.T) {
	dir := t.TempDir()
	for _, file := range []string{filepath.Join(dir, "missing.html"), dir} {
		if _, _, err := ReadPage(file); err == nil || !strings.HasPrefix(err.Error(), "the page: ") {
			t.Errorf("%s: %v", file, err)
		}
	}
}

// With --page, the build is the file's as it is at that moment: the page is
// served with it, the board says it, and both change when the file does, so
// that a page left open reloads itself. A file that cannot be read has no build.
func TestBuildFromFile(t *testing.T) {
	f := newFixture(t)
	file := filepath.Join(t.TempDir(), "page.html")
	h, err := Handler(f.store, Info{Page: file}, f.act, addr)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://"+addr+path, nil))
		return w
	}
	boardBuild := func() string {
		t.Helper()
		var b struct {
			Build string `json:"build"`
		}
		if err := json.Unmarshal(get("/api/board").Body.Bytes(), &b); err != nil {
			t.Fatal(err)
		}
		return b.Build
	}
	var builds []string
	for _, text := range []string{
		`<html data-build=""><style>p{}</style><script>let a</script>`,
		`<html data-build=""><style>p{}</style><script>let b</script>`,
	} {
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		want := buildOf([]byte(text))
		if got := boardBuild(); got != want {
			t.Errorf("the board's build: %q, want the file's, %q", got, want)
		}
		if body := get("/").Body.String(); !strings.Contains(body, `data-build="`+want+`"`) {
			t.Errorf("the served page does not carry the file's build %s: %q", want, body)
		}
		builds = append(builds, want)
	}
	if builds[0] == builds[1] {
		t.Error("a changed file has the same build")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if got := boardBuild(); got != "" {
		t.Errorf("the board's build without a file: %q, want none", got)
	}
}

// TestPublish publishes from the screen: publish is offered only by a core
// with a publish command; a change that touches workflow files is refused
// with 409 and a sentence without the developer's yes, and taken with it, the
// yes kept for the change's tip (ADR 0060); a card being published has no move
// and says so; and its pull request shows once it has one, and after the merge.
func TestPublish(t *testing.T) {
	f := newFixture(t)
	passed := store.State{Stage: store.Review, WaitsFor: store.Approval}
	atHost := store.State{Stage: store.Review, WaitsFor: store.Host}
	plain, touches := f.card(t, "Plain", passed), f.card(t, "Touches the workflows", passed)
	changes := map[int64]*Change{
		plain:   {Branch: "kiln/card-1", Tip: "c0ffee", Commits: []Commit{}, Workflows: nil},
		touches: {Branch: "kiln/card-2", Tip: "beef", Commits: []Commit{}, Workflows: []string{".github/workflows/check.yml"}},
	}
	info := Info{Instance: "ab12cd34", Model: "m", Publishing: true,
		Change: func(_ context.Context, card int64) (*Change, error) { return changes[card], nil }}
	without := f.h
	h, err := Handler(f.store, info, f.act, addr)
	if err != nil {
		t.Fatal(err)
	}
	f.h = h
	post := func(id int64, body string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		w := f.do(t, "POST", fmt.Sprintf("/api/cards/%d/decide", id), body)
		return w, decode[map[string]any](t, w)
	}
	get := func(id int64) cardJSON {
		t.Helper()
		return decode[cardJSON](t, f.do(t, "GET", fmt.Sprintf("/api/cards/%d", id), ""))
	}

	// Publish comes first among the moves of a review that passed.
	if c := get(plain); fmt.Sprint(c.Moves) != "[{publish none} {done none} {send-back required} {drop optional}]" {
		t.Errorf("the moves of a review that passed: %v", c.Moves)
	}
	if c := get(touches); fmt.Sprint(c.Change.Workflows) != "[.github/workflows/check.yml]" {
		t.Errorf("the change's workflow files: %+v", c.Change)
	}

	// A core without a publish command refuses it.
	f.h = without
	if w, got := post(plain, `{"action":"publish"}`); w.Code != 409 || fmt.Sprint(got["moves"]) != "[map[action:done note:none] map[action:send-back note:required] map[action:drop note:optional]]" {
		t.Errorf("publish without a publish command: %d %v", w.Code, got)
	}
	f.h = h

	// Without the yes, a change to the workflow files is refused, with the
	// files; with it, it is published and the yes kept for its tip.
	w, got := post(touches, `{"action":"publish"}`)
	if w.Code != 409 || !strings.Contains(fmt.Sprint(got["error"]), "touches workflow files") ||
		fmt.Sprint(got["workflows"]) != "[.github/workflows/check.yml]" || got["waits_for"] != "approval" {
		t.Errorf("publish of a workflow change without the yes: %d %v", w.Code, got)
	}
	if c, _ := f.store.Card(touches); c.State != passed {
		t.Errorf("the refused publish moved the card to %v", c.State)
	}
	if got["tip"] != "beef" {
		t.Errorf("the refusal names the tip %v", got["tip"])
	}
	// A yes for a tip the page showed earlier is refused as well.
	if w, got := post(touches, `{"action":"publish","workflow_yes":true,"tip":"c0ffee"}`); w.Code != 409 ||
		!strings.Contains(fmt.Sprint(got["error"]), "no longer the one you said yes to") {
		t.Errorf("a yes for another tip: %d %v", w.Code, got)
	}
	if w, got := post(touches, `{"action":"publish","workflow_yes":true,"tip":"beef"}`); w.Code != 200 || got["waits_for"] != "host" {
		t.Errorf("publish with the yes: %d %v", w.Code, got)
	}
	if p, _ := f.store.Publication(touches); p.WorkflowYes != "beef" {
		t.Errorf("the publication: %+v", p)
	}
	// A yes for a change without workflow files is not kept.
	if w, _ := post(plain, `{"action":"publish","workflow_yes":true}`); w.Code != 200 {
		t.Errorf("publish of a plain change: %d", w.Code)
	}
	if p, _ := f.store.Publication(plain); p.WorkflowYes != "" {
		t.Errorf("the publication of a plain change: %+v", p)
	}

	// While it is published, the card says so and has no move.
	c := get(plain)
	if !c.Publishing || c.PullRequest != nil || len(c.Moves) != 0 {
		t.Errorf("a card being published: publishing %v, %+v, moves %v", c.Publishing, c.PullRequest, c.Moves)
	}
	if w, got := post(plain, `{"action":"done"}`); w.Code != 409 || !strings.Contains(fmt.Sprint(got["error"]), "being published") {
		t.Errorf("done while the card is published: %d %v", w.Code, got)
	}

	// Once it has its address, the pull request shows, and the card can be
	// closed or sent back.
	if _, err := f.store.KeepPullRequest(plain, "f00d", "https://git.example/pull/3"); err != nil {
		t.Fatal(err)
	}
	c = get(plain)
	if c.Publishing || c.PullRequest == nil || *c.PullRequest != (pullJSON{"https://git.example/pull/3", "f00d"}) ||
		fmt.Sprint(c.Moves) != "[{done none} {send-back required} {drop optional}]" {
		t.Errorf("a card at the Git host: %v %+v %v", c.Publishing, c.PullRequest, c.Moves)
	}
	if _, err := f.store.MoveWithReason(plain, atHost, store.State{Stage: store.Done, WaitsFor: store.Nothing}, store.CauseHost, store.MergedReason); err != nil {
		t.Fatal(err)
	}
	c = get(plain)
	if !c.Merged || c.PullRequest == nil || c.Dropped {
		t.Errorf("a merged card: merged %v, %+v", c.Merged, c.PullRequest)
	}
	b := decode[boardJSON](t, f.do(t, "GET", "/api/board", ""))
	if !b.Cards[0].Merged || b.Cards[1].Merged || b.Cards[1].WaitsFor != store.Host {
		t.Errorf("the board: %+v", b.Cards)
	}
}
