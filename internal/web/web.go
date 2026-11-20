// Package web is the core's screen (ADR 0023, ADR 0044): one embedded page,
// index.html, with its CSS and JavaScript inside, and the JSON API it asks.
// The core serves it on a loopback address while it ticks.
//
// Only this machine's browser may ask, and only the page may act. A card's
// text becomes a run's instruction, so a page on the web that could add a
// card could instruct a run: every request must name the listen address as its
// Host, which a page on the web reached by a rebound name does not; and a
// POST must be JSON, which a form cannot send, with an Origin, when it has
// one, of the page's own.
//
// Nothing of a run is trusted by the page: every text of a card, a run or a
// document reaches it as JSON, and the page puts it in as text, never as HTML.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/store"
)

//go:embed index.html
var page []byte

// policy is the embedded page's Content-Security-Policy, computed once.
var policy = func() string {
	p, err := policyOf(page)
	if err != nil {
		panic("index.html: " + err.Error())
	}
	return p
}()

// policyOf is a page's Content-Security-Policy: its one script and its one
// style, by their hashes, requests to its own origin only, and nothing else.
// A page without a script or a style has none.
func policyOf(page []byte) (string, error) {
	hash := func(tag string) (string, error) {
		m := regexp.MustCompile(`(?s)<` + tag + `>(.*?)</` + tag + `>`).FindSubmatch(page)
		if m == nil {
			return "", fmt.Errorf("the page has no <%s>", tag)
		}
		sum := sha256.Sum256(m[1])
		return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'", nil
	}
	script, err := hash("script")
	if err != nil {
		return "", err
	}
	style, err := hash("style")
	if err != nil {
		return "", err
	}
	return "default-src 'none'; script-src " + script + "; style-src " + style +
		"; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'", nil
}

// ReadPage reads a page from a file, for work on the screen (kiln core
// --page), and gives it with its own policy, so that it runs as it will once
// embedded.
func ReadPage(file string) ([]byte, string, error) {
	body, err := os.ReadFile(file)
	if err != nil {
		return nil, "", fmt.Errorf("the page: %w", err)
	}
	p, err := policyOf(body)
	if err != nil {
		return nil, "", fmt.Errorf("the page %s: %w", file, err)
	}
	return body, p, nil
}

// buildOf is a page's build: the first 16 hex digits of its SHA-256.
func buildOf(page []byte) string {
	sum := sha256.Sum256(page)
	return hex.EncodeToString(sum[:])[:16]
}

// withBuild is a page as it is served: the page with its build in its one
// data-build attribute, which the page compares with the board's. The
// attribute is outside the script and the style, so their hashes in the policy
// hold for the served page too. A page that does not have exactly one empty
// attribute is given as it is, and withBuild says so.
func withBuild(page []byte) ([]byte, bool) {
	empty := []byte(`data-build=""`)
	if bytes.Count(page, empty) != 1 {
		return page, false
	}
	return bytes.Replace(page, empty, []byte(`data-build="`+buildOf(page)+`"`), 1), true
}

// build is the embedded page's build. Only the page goes into it, so a core
// rebuilt with the same page has the same build, and a page left open on it
// does not reload.
var build = buildOf(page)

// served is the embedded page as it is served.
var served = func() []byte {
	s, ok := withBuild(page)
	if !ok {
		panic(`index.html does not have exactly one data-build=""`)
	}
	return s
}()

// maxBody is the largest body of a request that is read.
const maxBody = 1 << 20

// Info is what the board says about the core, and where the card's page gets
// a card's change from.
type Info struct {
	Instance string
	Repo     string
	// The models of the spec runs, the implement runs and the review runs the
	// core starts.
	Model, ImplementModel, ReviewModel string
	// Change is the change on a card's branch, or nil when the card has no
	// branch. The tick hands it in, so that this package runs no command. Nil
	// shows no change on any card.
	Change func(ctx context.Context, card int64) (*Change, error)
	// Page is a file the page is read from on every request, with its own
	// policy, instead of the embedded one, for work on the screen. Empty, the
	// embedded page is served.
	Page string
}

// Change is where a card's change is and what it is: its branch in the
// instance's copy of the repo, the branch's commits since it left main, newest
// first, and the diff from where it left main to its tip. A diff over the
// limit is not given: only its size, and the command that prints it.
type Change struct {
	Branch   string
	Repo     string
	Commits  []Commit
	Diff     string
	Size     int64
	TooLarge bool
	Command  string
}

// Commit is one commit of a card's branch: its short hash and its title.
type Commit struct {
	Hash  string
	Title string
}

// CheckAddress refuses a listen address whose host is not loopback: an IP
// address of the loopback, or localhost.
func CheckAddress(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("the address %q: %w", addr, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("the address %q has no port", addr)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("the address %q is not loopback; the screen listens only on this machine (ADR 0044)", addr)
	}
	return nil
}

// Listen checks the address and listens on it.
func Listen(addr string) (net.Listener, error) {
	if err := CheckAddress(addr); err != nil {
		return nil, err
	}
	return net.Listen("tcp", addr)
}

// Handler is the screen and its API on the store, for a core that listens on
// addr, the address it actually listens on, with its port.
func Handler(s *store.Store, info Info, act *Activity, addr string) (http.Handler, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	hosts := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true, net.JoinHostPort(host, port): true}
	h := &handler{store: s, info: info, act: act}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.page)
	mux.HandleFunc("GET /api/board", h.board)
	mux.HandleFunc("GET /api/cards/{id}", h.card)
	mux.HandleFunc("POST /api/cards", h.create)
	mux.HandleFunc("POST /api/cards/{id}/decide", h.decide)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] {
			fail(w, http.StatusForbidden, "the request's Host is not the core's address")
			return
		}
		if r.Method == http.MethodPost {
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
				fail(w, http.StatusForbidden, "a POST must have the Content-Type application/json")
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
				fail(w, http.StatusForbidden, "the request's Origin is not the page's")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	}), nil
}

type handler struct {
	store *store.Store
	info  Info
	act   *Activity
}

func (h *handler) page(w http.ResponseWriter, r *http.Request) {
	body, csp := served, policy
	if h.info.Page != "" {
		// A file that went wrong is said, never covered by the embedded page.
		file, filePolicy, err := ReadPage(h.info.Page)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		body, _ = withBuild(file)
		csp = filePolicy
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Write(body)
}

// buildNow is the build of the page the core serves at this moment: the
// embedded page's, or with --page the file's as it is now, so that a page left
// open on an earlier state of the file reloads itself. A file that cannot be
// read has no build, and a page left open then stays as it is.
func (h *handler) buildNow() string {
	if h.info.Page == "" {
		return build
	}
	file, _, err := ReadPage(h.info.Page)
	if err != nil {
		return ""
	}
	return buildOf(file)
}

// The API's JSON.
type (
	boardJSON struct {
		Instance string     `json:"instance"`
		Build    string     `json:"build"`
		Repo     string     `json:"repo"`
		Models   modelsJSON `json:"models"`
		Now      time.Time  `json:"now"`
		Cards    []tileJSON `json:"cards"`
	}
	modelsJSON struct {
		Spec      string `json:"spec"`
		Implement string `json:"implement"`
		Review    string `json:"review"`
	}
	tileJSON struct {
		ID        int64          `json:"id"`
		Title     string         `json:"title"`
		Stage     store.Stage    `json:"stage"`
		WaitsFor  store.WaitsFor `json:"waits_for"`
		Reason    string         `json:"reason"`
		Dropped   bool           `json:"dropped"`
		Run       *flightJSON    `json:"run"`
		Documents []string       `json:"documents"`
	}
	flightJSON struct {
		ID    string     `json:"id"`
		Model string     `json:"model"`
		Since *time.Time `json:"since"`
	}
	cardJSON struct {
		tileJSON
		Text      string         `json:"text"`
		Moves     []moveJSON     `json:"moves"`
		Note      string         `json:"note"`
		History   []entryJSON    `json:"history"`
		Runs      []runJSON      `json:"runs"`
		Documents []documentJSON `json:"documents"`
		Activity  []activityJSON `json:"activity"`
		Change    *changeJSON    `json:"change"`
		Now       time.Time      `json:"now"`
	}
	moveJSON struct {
		Action store.Action `json:"action"`
		Note   store.Note   `json:"note"`
	}
	changeJSON struct {
		Branch   string       `json:"branch"`
		Repo     string       `json:"repo"`
		Commits  []commitJSON `json:"commits"`
		Diff     string       `json:"diff"`
		Size     int64        `json:"size"`
		TooLarge bool         `json:"too_large"`
		Command  string       `json:"command"`
		Error    string       `json:"error,omitempty"`
	}
	commitJSON struct {
		Hash  string `json:"hash"`
		Title string `json:"title"`
	}
	entryJSON struct {
		Number   int64          `json:"number"`
		At       time.Time      `json:"at"`
		Cause    store.Cause    `json:"cause"`
		Stage    store.Stage    `json:"stage"`
		WaitsFor store.WaitsFor `json:"waits_for"`
		Run      string         `json:"run"`
		Reason   string         `json:"reason"`
		Note     string         `json:"note"`
		Dropped  bool           `json:"dropped"`
	}
	runJSON struct {
		ID      string       `json:"id"`
		Commit  string       `json:"commit"`
		Model   string       `json:"model"`
		Effort  string       `json:"effort"`
		Ending  store.Ending `json:"ending"`
		Started *time.Time   `json:"started"`
	}
	documentJSON struct {
		Kind string `json:"kind"`
		Run  string `json:"run"`
		Text string `json:"text"`
	}
	activityJSON struct {
		At   time.Time `json:"at"`
		Kind string    `json:"kind"`
		Tool string    `json:"tool"`
		Text string    `json:"text"`
	}
)

// title is a card's first line that is not empty.
func title(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func started(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func tile(s store.Summary) tileJSON {
	t := tileJSON{ID: s.ID, Title: title(s.Text), Stage: s.Stage, WaitsFor: s.WaitsFor, Reason: s.Reason,
		Dropped: s.Dropped, Documents: s.Kinds}
	if t.Documents == nil {
		t.Documents = []string{}
	}
	if r := s.InFlight; r != nil {
		t.Run = &flightJSON{ID: r.ID, Model: r.Model, Since: started(r.Started)}
	}
	return t
}

func (h *handler) board(w http.ResponseWriter, r *http.Request) {
	cards, err := h.store.Board()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	b := boardJSON{Instance: h.info.Instance, Build: h.buildNow(), Repo: h.info.Repo, Models: h.models(), Now: time.Now().UTC(),
		Cards: []tileJSON{}}
	for _, c := range cards {
		b.Cards = append(b.Cards, tile(c))
	}
	reply(w, http.StatusOK, b)
}

// models are the models of the three stages; an empty one is the spec's.
func (h *handler) models() modelsJSON {
	m := modelsJSON{Spec: h.info.Model, Implement: h.info.ImplementModel, Review: h.info.ReviewModel}
	if m.Implement == "" {
		m.Implement = m.Spec
	}
	if m.Review == "" {
		m.Review = m.Spec
	}
	return m
}

// cardID is the card's ID in the request's path, or 0 when it is none.
func cardID(r *http.Request) int64 {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

func (h *handler) card(w http.ResponseWriter, r *http.Request) {
	id := cardID(r)
	c, err := h.store.Card(id)
	if id == 0 || errors.Is(err, store.ErrNoCard) {
		fail(w, http.StatusNotFound, fmt.Sprintf("no card %s", r.PathValue("id")))
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := h.cardJSON(c)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out.Change = h.change(r.Context(), c.ID)
	reply(w, http.StatusOK, out)
}

// change is the change of a card's branch, nil when it has none. A change that
// cannot be read is said on the card's page, which shows the rest.
func (h *handler) change(ctx context.Context, card int64) *changeJSON {
	if h.info.Change == nil {
		return nil
	}
	ch, err := h.info.Change(ctx, card)
	if err != nil {
		return &changeJSON{Commits: []commitJSON{}, Error: run.Printable(err.Error())}
	}
	if ch == nil {
		return nil
	}
	out := &changeJSON{Branch: ch.Branch, Repo: ch.Repo, Commits: []commitJSON{}, Diff: ch.Diff, Size: ch.Size,
		TooLarge: ch.TooLarge, Command: ch.Command}
	for _, c := range ch.Commits {
		out.Commits = append(out.Commits, commitJSON{Hash: c.Hash, Title: c.Title})
	}
	return out
}

// cardJSON is everything the card's page shows.
func (h *handler) cardJSON(c store.Card) (cardJSON, error) {
	history, err := h.store.History(c.ID)
	if err != nil {
		return cardJSON{}, err
	}
	runs, err := h.store.Runs(c.ID)
	if err != nil {
		return cardJSON{}, err
	}
	docs, err := h.store.Documents(c.ID)
	if err != nil {
		return cardJSON{}, err
	}
	sum := store.Summary{Card: c}
	out := cardJSON{Text: c.Text, History: []entryJSON{}, Runs: []runJSON{},
		Documents: []documentJSON{}, Activity: []activityJSON{}, Now: time.Now().UTC()}
	if len(history) > 0 {
		last := history[len(history)-1]
		sum.Reason, sum.Dropped = last.Reason, store.Dropped(last.State, last.Cause, last.Reason)
		// The note that the run the card waits for is told, as the tick
		// reads it (store.Note).
		if c.WaitsFor == store.Run {
			out.Note = last.Note()
		}
	}
	for _, e := range history {
		out.History = append(out.History, entryJSON{Number: e.Number, At: e.At, Cause: e.Cause, Stage: e.Stage,
			WaitsFor: e.WaitsFor, Run: e.Run, Reason: e.Reason, Note: e.Note(),
			Dropped: store.Dropped(e.State, e.Cause, e.Reason)})
	}
	for _, r := range runs {
		out.Runs = append(out.Runs, runJSON{ID: r.ID, Commit: r.Commit, Model: r.Model, Effort: r.Effort,
			Ending: r.Ending, Started: started(r.Started)})
		if r.Ending == "" {
			sum.InFlight = &r
		}
	}
	out.Moves = movesJSON(store.Moves(c.State, sum.InFlight != nil))
	seen := map[string]bool{}
	for _, d := range docs {
		out.Documents = append(out.Documents, documentJSON{Kind: d.Kind, Run: d.Run, Text: d.Text})
		if !seen[d.Kind] {
			seen[d.Kind] = true
			sum.Kinds = append(sum.Kinds, d.Kind)
		}
	}
	out.tileJSON = tile(sum)
	if sum.InFlight != nil && h.act != nil {
		for _, a := range h.act.Of(sum.InFlight.ID) {
			out.Activity = append(out.Activity, activityJSON{At: a.At, Kind: a.Kind, Tool: a.Tool, Text: a.Text})
		}
	}
	return out, nil
}

// waiting is where a new card goes, as `kiln card` puts it (tick.AddCard):
// in spec, waiting for a run.
var waiting = store.State{Stage: store.Spec, WaitsFor: store.Run}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text *string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text == nil {
		fail(w, http.StatusBadRequest, `the body must be {"text": …}`)
		return
	}
	if strings.TrimSpace(*body.Text) == "" {
		fail(w, http.StatusBadRequest, "a card needs a text")
		return
	}
	c, err := h.store.Create(*body.Text)
	if err == nil {
		c, err = h.store.Move(c.ID, waiting, store.CauseDeveloper)
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := h.cardJSON(c)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, http.StatusCreated, out)
}

func (h *handler) decide(w http.ResponseWriter, r *http.Request) {
	id := cardID(r)
	if id == 0 {
		fail(w, http.StatusNotFound, fmt.Sprintf("no card %s", r.PathValue("id")))
		return
	}
	var body struct {
		Action store.Action `json:"action"`
		Note   string       `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, `the body must be {"action": …, "note": …}, the note optional`)
		return
	}
	c, err := h.store.Decide(id, body.Action, body.Note)
	switch {
	case errors.Is(err, store.ErrNoCard):
		fail(w, http.StatusNotFound, fmt.Sprintf("no card %d", id))
		return
	case errors.Is(err, store.ErrNote):
		// The sentence of what does not fit, such as "this move needs a note
		// that says what should change".
		fail(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), store.ErrNote.Error()+": "))
		return
	case errors.Is(err, store.ErrRefused):
		inFlight, ferr := h.store.InFlight(c.ID)
		if ferr != nil {
			fail(w, http.StatusInternalServerError, ferr.Error())
			return
		}
		reply(w, http.StatusConflict, map[string]any{"error": err.Error(), "stage": c.Stage, "waits_for": c.WaitsFor,
			"moves": movesJSON(store.Moves(c.State, inFlight != nil))})
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := h.cardJSON(c)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out.Change = h.change(r.Context(), c.ID)
	reply(w, http.StatusOK, out)
}

// movesJSON are the moves a card's state allows, each with what it takes of a
// note, so that the page asks for a note where one is needed.
func movesJSON(moves []store.Allowed) []moveJSON {
	out := []moveJSON{}
	for _, m := range moves {
		out = append(out, moveJSON{Action: m.Action, Note: m.Note})
	}
	return out
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]string{"error": run.Printable(msg)})
}
