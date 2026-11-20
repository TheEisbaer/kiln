package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
	h, err := Handler(s, Info{Instance: "ab12cd34", Repo: "/data/repo.git", Model: "claude-opus-5-5"}, act, addr)
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
		b.Models != (modelsJSON{"claude-opus-5-5", "claude-opus-5-5", "claude-opus-5-5"}) || b.Cards == nil || len(b.Cards) != 0 {
		t.Fatalf("an empty board: %+v", b)
	}
	spec := f.card(t, "\n  Write the screen  \nwith more", store.State{Stage: store.Spec, WaitsFor: store.Run})
	if err := f.store.AddRun(store.RunRow{ID: "r1", Card: spec, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EndRun("r1", store.Finished, "exit 0, not saved", store.State{Stage: store.Spec, WaitsFor: store.Approval}, ptr("# Spec")); err != nil {
		t.Fatal(err)
	}
	running := f.card(t, "Runs", store.State{Stage: store.Spec, WaitsFor: store.Run})
	if err := f.store.AddRun(store.RunRow{ID: "r2", Card: running, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}); err != nil {
		t.Fatal(err)
	}

	w := f.do(t, "GET", "/api/board", "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("GET /api/board: %d %s", w.Code, w.Header())
	}
	b := decode[boardJSON](t, w)
	if len(b.Cards) != 2 {
		t.Fatalf("the cards: %+v", b.Cards)
	}
	if c := b.Cards[0]; c.ID != spec || c.Title != "Write the screen" || c.Stage != "spec" || c.WaitsFor != "approval" ||
		c.Reason != "exit 0, not saved" || c.Run != nil || !reflect.DeepEqual(c.Documents, []string{"spec"}) {
		t.Errorf("the card with a spec: %+v", c)
	}
	if c := b.Cards[1]; c.Run == nil || c.Run.ID != "r2" || c.Run.Model != "claude-opus-5-5" || c.Run.Since == nil ||
		time.Since(*c.Run.Since) > time.Minute || len(c.Documents) != 0 || c.Documents == nil {
		t.Errorf("the card with a run in flight: %+v, run %+v", c, c.Run)
	}
}

func ptr(s string) *store.Doc { return &store.Doc{Kind: store.KindSpec, Text: s} }

func TestCard(t *testing.T) {
	f := newFixture(t)
	// A document with a script and an escape sequence arrives unchanged, as
	// JSON text.
	doc := "# Spec\n<script>alert(1)</script>\x1b]0;title\x07\x1b[2J\n"
	id := f.card(t, "The card\n\nits text <b>", store.State{Stage: store.Spec, WaitsFor: store.Run})
	if err := f.store.AddRun(store.RunRow{ID: "r1", Card: id, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}); err != nil {
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
		c.Reason != "exit 0" || !reflect.DeepEqual(c.Moves, []store.Action{"approve"}) || c.Run != nil {
		t.Errorf("the card: %+v", c)
	}
	if want := []documentJSON{{"spec", "r1", doc}}; !reflect.DeepEqual(c.Documents, want) {
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
	if _, err := f.store.Decide(id, store.Approve); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AddRun(store.RunRow{ID: "r2", Card: id, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}); err != nil {
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
		Diff: changes[2].Diff, Size: 41, Command: "git --git-dir=/data/repo.git diff HEAD...kiln/card-2"}
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

// TestDecide sends every row of the table, and three moves that are not in
// it.
func TestDecide(t *testing.T) {
	f := newFixture(t)
	for _, row := range []struct {
		action   string
		from, to store.State
	}{
		{"approve", store.State{Stage: store.Spec, WaitsFor: store.Approval}, store.State{Stage: store.Implement, WaitsFor: store.Run}},
		{"again", store.State{Stage: store.Spec, WaitsFor: store.Developer}, store.State{Stage: store.Spec, WaitsFor: store.Run}},
		{"again", store.State{Stage: store.Implement, WaitsFor: store.Developer}, store.State{Stage: store.Implement, WaitsFor: store.Run}},
		{"again", store.State{Stage: store.Review, WaitsFor: store.Developer}, store.State{Stage: store.Review, WaitsFor: store.Run}},
		{"send-back", store.State{Stage: store.Review, WaitsFor: store.Developer}, store.State{Stage: store.Implement, WaitsFor: store.Run}},
		{"done", store.State{Stage: store.Review, WaitsFor: store.Approval}, store.State{Stage: store.Done, WaitsFor: store.Nothing}},
	} {
		id := f.card(t, "a card", row.from)
		w := f.do(t, "POST", fmt.Sprintf("/api/cards/%d/decide", id), `{"action":"`+row.action+`"}`)
		c := decode[cardJSON](t, w)
		if w.Code != 200 || c.Stage != row.to.Stage || c.WaitsFor != row.to.WaitsFor {
			t.Errorf("%s from %v: %d %+v", row.action, row.from, w.Code, c)
		}
		if got, _ := f.store.Card(id); got.State != row.to {
			t.Errorf("%s from %v: the store has %v", row.action, row.from, got.State)
		}
	}

	for _, row := range []struct {
		action string
		from   store.State
	}{
		{"approve", store.State{Stage: store.Spec, WaitsFor: store.Run}},         // a spec not written yet
		{"send-back", store.State{Stage: store.Spec, WaitsFor: store.Developer}}, // not in review
		{"drop", store.State{Stage: store.Review, WaitsFor: store.Developer}},    // no such move
	} {
		id := f.card(t, "a card", row.from)
		before, _ := f.store.History(id)
		w := f.do(t, "POST", fmt.Sprintf("/api/cards/%d/decide", id), `{"action":"`+row.action+`"}`)
		got := decode[map[string]any](t, w)
		if w.Code != 409 || got["stage"] != string(row.from.Stage) || got["waits_for"] != string(row.from.WaitsFor) {
			t.Errorf("%s from %v: %d %v; want 409 with the card's state", row.action, row.from, w.Code, got)
		}
		if after, _ := f.store.History(id); len(after) != len(before) {
			t.Errorf("%s from %v was refused and wrote an entry", row.action, row.from)
		}
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
