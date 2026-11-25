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
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	Repo     string // the copy of Kiln's own repo, shown in the board's header
	// Repos are the instance's repos, in order; empty, the instance has one,
	// store.KilnRepo. A new card names one of them, and the board's header
	// shows them when there is more than one.
	Repos []Repo
	// The models of the spec runs, the implement runs and the review runs the
	// core starts.
	Model, ImplementModel, ReviewModel string
	// Runs is the most runs the core has in flight at once.
	Runs int
	// Change is the change on a card's branch, or nil when the card has no
	// branch. The tick hands it in, so that this package runs no command. Nil
	// shows no change on any card.
	Change func(ctx context.Context, card int64) (*Change, error)
	// Page is a file the page is read from on every request, with its own
	// policy, instead of the embedded one, for work on the screen. Empty, the
	// embedded page is served.
	Page string
	// Publishing is true for the name of each repo that has a publish
	// command: the table of moves of its cards then has publish (card 19).
	// A nil map publishes no card.
	Publishing map[string]bool
	// Version is the commit the core's program was built from, or empty for
	// a program built by hand; the board's header shows it.
	Version string
	// Limits are the limits of cards in progress (ADR 0014), each repo's and
	// the instance's, by which a new card and the developer's start are
	// queued; the zero Limits, or a repo left out, has store.DefaultLimit.
	Limits store.Limits
	// Accepting says whether the core accepts moves: until its report, every
	// POST is refused (ADR 0036, rule 3). Nil accepts.
	Accepting func() bool
}

// Repo is one of the instance's repos: its name and the copy the core holds.
type Repo struct{ Name, Copy string }

// starting is the sentence of a move refused before the core's report.
const starting = "Kiln is starting, and accepts moves once it has compared the store, the container engine and the Git host"

// Change is where a card's change is and what it is: its branch in the
// instance's copy of the repo and the commit it is at, the branch's commits
// since it left main, newest first, the diff from where it left main to its
// tip, and the workflow files it touches (ADR 0060). A diff over the limit is
// not given: only its size, and the command that prints it.
type Change struct {
	Branch    string
	Repo      string
	Tip       string
	Commits   []Commit
	Diff      string
	Size      int64
	TooLarge  bool
	Command   string
	Workflows []string
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
	mux.HandleFunc("POST /api/instance/decide", h.decideInstance)
	mux.HandleFunc("POST /api/proposals/{id}/decide", h.decideProposal)
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
			if info.Accepting != nil && !info.Accepting() {
				fail(w, http.StatusServiceUnavailable, starting)
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
		Instance string `json:"instance"`
		Build    string `json:"build"`
		// Version is the commit the core was built from, empty for a
		// program built by hand.
		Version string `json:"version"`
		// Repo is the copy of Kiln's own repo; Repos are every repo's name
		// and copy, in the settings' order, which the header shows when
		// there is more than one.
		Repo  string     `json:"repo"`
		Repos []repoJSON `json:"repos"`
		// InstanceLimit is the instance's limit and its cards that count
		// against it, those in progress not ready to merge (ADR 0014).
		InstanceLimit instanceLimitJSON `json:"instance_limit"`
		Models        modelsJSON        `json:"models"`
		Runs          int               `json:"runs"`
		Now           time.Time         `json:"now"`
		// Paused is why the instance is paused, empty when it is not: then
		// no run starts (ADR 0016, ADR 0055).
		Paused string `json:"paused"`
		// MergingOff is set while automatic merging is off after a failed
		// update, and null while it is on (ADR 0019, rule 3).
		MergingOff *mergingJSON `json:"merging_off"`
		Cards      []tileJSON   `json:"cards"`
		// Proposals are the open proposals, oldest first, apart from the
		// cards: they raise no count of "needs you" (ADR 0033, rule 3).
		Proposals []proposalJSON `json:"proposals"`
		// Measures are the board's measures over the whole history.
		Measures measuresJSON `json:"measures"`
	}
	// repoJSON: Limit is the repo's limit of cards in progress, and
	// InProgress its cards in spec, implement or review (ADR 0014).
	repoJSON struct {
		Name       string `json:"name"`
		Copy       string `json:"copy"`
		Limit      int    `json:"limit"`
		InProgress int    `json:"in_progress"`
	}
	instanceLimitJSON struct {
		Limit      int `json:"limit"`
		InProgress int `json:"in_progress"`
	}
	// measuresJSON: PerMerged is null with no merged card.
	measuresJSON struct {
		Merged        int                `json:"merged"`
		Interventions int                `json:"interventions"`
		PerMerged     *float64           `json:"per_merged"`
		Dropped       int                `json:"dropped"`
		NeededFix     int                `json:"needed_fix"`
		Proposals     proposalCountsJSON `json:"proposals"`
		Text          string             `json:"text"`
	}
	// proposalCountsJSON are the proposals made by reviews, accepted as
	// cards and dismissed, over the whole history (ADR 0033, rule 4).
	proposalCountsJSON struct {
		Made      int `json:"made"`
		Accepted  int `json:"accepted"`
		Dismissed int `json:"dismissed"`
	}
	// proposalJSON is a proposal: the card it was noticed on and that
	// card's headline, the review run that wrote it, its title and its
	// headline, "" for none, its text, and its state, "open", "made" or
	// "dismissed", with the card made from it, 0 until then, and the note
	// it was dismissed with.
	proposalJSON struct {
		ID           int64  `json:"id"`
		Card         int64  `json:"card"`
		CardHeadline string `json:"card_headline"`
		Run          string `json:"run"`
		Title        string `json:"title"`
		Headline     string `json:"headline"`
		Text         string `json:"text"`
		State        string `json:"state"`
		Made         int64  `json:"made"`
		Note         string `json:"note"`
	}
	// originJSON is the proposal a card was made from.
	originJSON struct {
		ID   int64  `json:"id"`
		Card int64  `json:"card"`
		Run  string `json:"run"`
	}
	mergingJSON struct {
		// Reason names the failed update, why it failed, and the version
		// the core went back to.
		Reason string `json:"reason"`
		// Failed is the commit of the failed update, empty when the failed
		// program had none in its name.
		Failed string     `json:"failed"`
		Moves  []moveJSON `json:"moves"`
	}
	modelsJSON struct {
		Spec      string `json:"spec"`
		Implement string `json:"implement"`
		Review    string `json:"review"`
		// SentBack is the model of an implement run after a send-back
		// (ADR 0030, rule 3).
		SentBack string `json:"sent_back"`
	}
	tileJSON struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		// Repo is the name of the card's repo.
		Repo      string         `json:"repo"`
		Headline  string         `json:"headline"`
		Stage     store.Stage    `json:"stage"`
		WaitsFor  store.WaitsFor `json:"waits_for"`
		Reason    string         `json:"reason"`
		Dropped   bool           `json:"dropped"`
		Merged    bool           `json:"merged"`
		Run       *flightJSON    `json:"run"`
		Documents []string       `json:"documents"`
		// SendBacks is the card's number of send-backs since its spec was
		// approved (ADR 0054).
		SendBacks int `json:"send_backs"`
		// Cost is what the card's ended runs cost; null with none.
		Cost *costJSON `json:"cost"`
		// Predecessors are the cards it names as predecessors, and
		// QueuedFor those of them not done while it is queued (ADR 0010).
		Predecessors []int64 `json:"predecessors"`
		QueuedFor    []int64 `json:"queued_for"`
		// Interventions is the card's number of interventions, 0 included.
		Interventions int `json:"interventions"`
		// Fixes is the merged card it fixes, 0 for none.
		Fixes int64 `json:"fixes"`
	}
	partJSON struct {
		USD     float64 `json:"usd"`
		Runs    int     `json:"runs"`
		Unknown int     `json:"unknown"`
	}
	costJSON struct {
		Total     partJSON `json:"total"`
		Spec      partJSON `json:"spec"`
		Implement partJSON `json:"implement"`
		Review    partJSON `json:"review"`
		Text      string   `json:"text"`
		Label     string   `json:"label"`
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
		// NamedBy are the cards that name it as a predecessor and are not
		// in done (ADR 0011).
		NamedBy []namedJSON `json:"named_by"`
		// FixedBy are the cards that name it as the card they fix.
		FixedBy []int64 `json:"fixed_by"`
		// The card's pull request, once it has an address, and whether the
		// core is publishing it, from when the developer publishes until it
		// has one (card 19).
		PullRequest *pullJSON `json:"pull_request"`
		Publishing  bool      `json:"publishing"`
		// FromProposal is the proposal the card was made from, null when
		// it was not.
		FromProposal *originJSON `json:"from_proposal"`
		Now          time.Time   `json:"now"`
	}
	pullJSON struct {
		Address string `json:"address"`
		Commit  string `json:"commit"`
	}
	moveJSON struct {
		Action store.Action `json:"action"`
		Note   store.Note   `json:"note"`
	}
	changeJSON struct {
		Branch    string       `json:"branch"`
		Repo      string       `json:"repo"`
		Tip       string       `json:"tip"`
		Commits   []commitJSON `json:"commits"`
		Diff      string       `json:"diff"`
		Size      int64        `json:"size"`
		TooLarge  bool         `json:"too_large"`
		Command   string       `json:"command"`
		Workflows []string     `json:"workflows"`
		Error     string       `json:"error,omitempty"`
	}
	namedJSON struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
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
		// Proposal is the proposal the entry is about, 0 on every other
		// entry: such an entry changes no card.
		Proposal int64 `json:"proposal"`
		// Did is what a run's entry did, when the rule's entry follows it in the
		// same step: "failed", "passed", "sent back" or "finished". Empty on
		// every other entry.
		Did string `json:"did"`
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
		Note string `json:"note"`
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

// headlineMax is the most characters a headline has, its ellipsis included.
const headlineMax = 90

// headline is what the strip shows of a card's title: its first sentence,
// cut to at most headlineMax characters with an ellipsis. The cut steps back
// to the last white space when that lies past half of it, and drops trailing
// white space and punctuation before the ellipsis.
func headline(title string) string {
	r := []rune(title)
	r = r[:sentenceEnd(r)]
	if len(r) <= headlineMax {
		return string(r)
	}
	cut := headlineMax - 1
	for i := cut - 1; i >= headlineMax/2; i-- {
		if unicode.IsSpace(r[i]) {
			cut = i
			break
		}
	}
	return strings.TrimRightFunc(string(r[:cut]), func(c rune) bool {
		return unicode.IsSpace(c) || strings.ContainsRune(",;:-–—", c)
	}) + "…"
}

// sentenceEnd is the length of the first sentence of r, a simplified form of
// the sentence rules of Unicode Standard Annex #29: a sentence ends after
// ., ! or ?, any closing brackets and quotes, and white space (SB11), but not
// a full stop whose white space is followed by a lowercase letter (SB8).
func sentenceEnd(r []rune) int {
	for i, c := range r {
		if c != '.' && c != '!' && c != '?' {
			continue
		}
		end := i + 1
		for end < len(r) && (unicode.In(r[end], unicode.Pe, unicode.Pf) || r[end] == '"' || r[end] == '\'') {
			end++
		}
		next := end
		for next < len(r) && unicode.IsSpace(r[next]) {
			next++
		}
		if next == end || next == len(r) || c == '.' && unicode.IsLower(r[next]) {
			continue
		}
		return end
	}
	return len(r)
}

func started(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// costText is a part of a card's cost as the tile shows it: dollars with two
// decimals, "+?" after it when some of its runs have no cost, and "?" when
// none has.
func costText(p store.Part) string {
	switch {
	case p.Runs > 0 && p.Unknown >= p.Runs:
		return "?"
	case p.Unknown > 0:
		return fmt.Sprintf("$%.2f+?", p.USD)
	}
	return fmt.Sprintf("$%.2f", p.USD)
}

// costLabel says whose figure the cost is, and its stages with runs.
func costLabel(c store.CardCost) string {
	var stages []string
	for _, s := range []struct {
		name string
		p    store.Part
	}{{"spec", c.Spec}, {"implement", c.Implement}, {"review", c.Review}} {
		if s.p.Runs > 0 {
			stages = append(stages, s.name+" "+costText(s.p))
		}
	}
	label := "Claude Code's estimate: " + costText(c.Total)
	if len(stages) > 0 {
		label += " (" + strings.Join(stages, ", ") + ")"
	}
	return label
}

func costOf(c *store.CardCost) *costJSON {
	if c == nil {
		return nil
	}
	part := func(p store.Part) partJSON { return partJSON{USD: p.USD, Runs: p.Runs, Unknown: p.Unknown} }
	return &costJSON{Total: part(c.Total), Spec: part(c.Spec), Implement: part(c.Implement), Review: part(c.Review),
		Text: costText(c.Total), Label: costLabel(*c)}
}

func tile(s store.Summary) tileJSON {
	t := tileJSON{ID: s.ID, Title: title(s.Text), Repo: s.Repo, Headline: headline(title(s.Text)), Stage: s.Stage, WaitsFor: s.WaitsFor, Reason: s.Reason,
		Dropped: s.Dropped, Merged: s.Merged, Documents: s.Kinds, SendBacks: s.SendBacks, Cost: costOf(s.Cost), Interventions: s.Interventions, Fixes: s.Fixes}
	if t.Documents == nil {
		t.Documents = []string{}
	}
	t.Predecessors = append([]int64{}, s.Predecessors...)
	t.QueuedFor = append([]int64{}, s.QueuedFor...)
	if r := s.InFlight; r != nil {
		t.Run = &flightJSON{ID: r.ID, Model: r.Model, Since: started(r.Started)}
	}
	return t
}

// measures is the board's strip: the figures and the line that tells them.
func measures(m store.Measures, p store.ProposalCounts) measuresJSON {
	out := measuresJSON{Merged: m.Merged, Interventions: m.Interventions, Dropped: m.Dropped, NeededFix: m.NeededFix,
		Proposals: proposalCountsJSON{Made: p.Made, Accepted: p.Accepted, Dismissed: p.Dismissed}}
	per := "–"
	if m.Merged > 0 {
		v := float64(m.Interventions) / float64(m.Merged)
		out.PerMerged = &v
		per = figure(v)
	}
	out.Text = fmt.Sprintf("%d merged · interventions per merged card: %s · needed a follow-up fix: %d of %d · %d dropped · proposals: %d made, %d accepted, %d dismissed",
		m.Merged, per, m.NeededFix, m.Merged, m.Dropped, p.Made, p.Accepted, p.Dismissed)
	return out
}

// proposal is a proposal's JSON, with the headline of the card it was
// noticed on.
func proposal(p store.Proposal, cardHeadline string) proposalJSON {
	out := proposalJSON{ID: p.ID, Card: p.Card, CardHeadline: cardHeadline, Run: p.Run, Title: p.Title, Headline: headline(p.Title),
		Text: p.Text, State: "open", Made: p.Made, Note: p.Note}
	switch {
	case p.Made != 0:
		out.State = "made"
	case p.Dismissed:
		out.State = "dismissed"
	}
	return out
}

// figure is v rounded to two decimals, without trailing zeros or a dot.
func figure(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(strconv.FormatFloat(v, 'f', 2, 64), "0"), ".")
}

func (h *handler) board(w http.ResponseWriter, r *http.Request) {
	cards, err := h.store.Board()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	paused, err := h.store.Paused()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	merging, err := h.mergingOff()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	open, counts, err := h.store.OpenProposals()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	b := boardJSON{Instance: h.info.Instance, Build: h.buildNow(), Version: h.info.Version, Repo: h.info.Repo, Models: h.models(), Runs: h.info.Runs, Now: time.Now().UTC(),
		Repos: []repoJSON{}, Paused: paused, MergingOff: merging, Cards: []tileJSON{}, Proposals: []proposalJSON{}}
	busy := map[string]int{}
	b.InstanceLimit.Limit = h.info.Limits.OfInstance()
	for _, c := range cards {
		if c.Stage == store.Spec || c.Stage == store.Implement || c.Stage == store.Review {
			busy[c.Repo]++
		}
		if store.CountsForInstance(c.State) {
			b.InstanceLimit.InProgress++
		}
	}
	for _, r := range h.info.Repos {
		b.Repos = append(b.Repos, repoJSON{Name: r.Name, Copy: r.Copy, Limit: h.info.Limits.Of(r.Name), InProgress: busy[r.Name]})
	}
	headlines := map[int64]string{}
	for _, c := range cards {
		t := tile(c)
		b.Cards = append(b.Cards, t)
		headlines[c.ID] = t.Headline
	}
	for _, p := range open {
		b.Proposals = append(b.Proposals, proposal(p, headlines[p.Card]))
	}
	b.Measures = measures(store.MeasuresOf(cards), counts)
	reply(w, http.StatusOK, b)
}

// models are the models of the three stages; an empty one is the spec's.
func (h *handler) models() modelsJSON {
	m := modelsJSON{Spec: h.info.Model, Implement: h.info.ImplementModel, Review: h.info.ReviewModel, SentBack: run.SentBackModel}
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
		return &changeJSON{Commits: []commitJSON{}, Workflows: []string{}, Error: run.Printable(err.Error())}
	}
	if ch == nil {
		return nil
	}
	out := &changeJSON{Branch: ch.Branch, Repo: ch.Repo, Tip: ch.Tip, Commits: []commitJSON{}, Diff: ch.Diff, Size: ch.Size,
		TooLarge: ch.TooLarge, Command: ch.Command, Workflows: append([]string{}, ch.Workflows...)}
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
	pub, err := h.store.Publication(c.ID)
	if err != nil {
		return cardJSON{}, err
	}
	moves, err := h.store.Moves(c, h.info.Publishing[c.Repo])
	if err != nil {
		return cardJSON{}, err
	}
	order, err := h.store.Order(c.ID)
	if err != nil {
		return cardJSON{}, err
	}
	fix, err := h.store.Fix(c.ID)
	if err != nil {
		return cardJSON{}, err
	}
	sum := store.Summary{Card: c, Predecessors: order.Predecessors, QueuedFor: order.QueuedFor, Fixes: fix.Fixes}
	out := cardJSON{Text: c.Text, History: []entryJSON{}, Runs: []runJSON{},
		Documents: []documentJSON{}, Activity: []activityJSON{}, Now: time.Now().UTC(), Moves: movesJSON(moves), NamedBy: []namedJSON{},
		FixedBy: append([]int64{}, fix.FixedBy...)}
	for _, n := range order.NamedBy {
		out.NamedBy = append(out.NamedBy, namedJSON{ID: n.ID, Title: title(n.Text)})
	}
	// The pull request shows while the card is at the Git host, and on a card
	// the Git host merged.
	atHost := c.State == store.State{Stage: store.Review, WaitsFor: store.Host}
	// The card's last entry, past those about a proposal, which change no
	// card.
	var changes []store.Entry
	for _, e := range history {
		if e.Proposal == 0 {
			changes = append(changes, e)
		}
	}
	if len(changes) > 0 {
		last := changes[len(changes)-1]
		sum.Reason, sum.Dropped = last.Reason, store.Dropped(last.State, last.Cause, last.Reason)
		sum.Merged = store.Merged(last.State, last.Cause, last.Reason)
		// The note that the run the card waits for is told, as the tick
		// reads it (store.Note).
		if c.WaitsFor == store.Run {
			out.Note = last.Note()
		}
	}
	endings := make(map[string]store.Ending, len(runs))
	for _, r := range runs {
		endings[r.ID] = r.Ending
	}
	for i, e := range history {
		out.History = append(out.History, entryJSON{Number: e.Number, At: e.At, Cause: e.Cause, Stage: e.Stage,
			WaitsFor: e.WaitsFor, Run: e.Run, Reason: e.Reason, Note: e.Note(),
			Dropped: store.Dropped(e.State, e.Cause, e.Reason), Proposal: e.Proposal, Did: did(history, i, endings)})
	}
	for _, r := range runs {
		out.Runs = append(out.Runs, runJSON{ID: r.ID, Commit: r.Commit, Model: r.Model, Effort: r.Effort,
			Ending: r.Ending, Started: started(r.Started)})
		if r.Ending == "" {
			sum.InFlight = &r
		}
	}
	if pub.Address != "" && (atHost || sum.Merged) {
		out.PullRequest = &pullJSON{Address: pub.Address, Commit: pub.Commit}
	}
	out.Publishing = atHost && pub.Address == "" && h.info.Publishing[c.Repo]
	seen := map[string]bool{}
	for _, d := range docs {
		out.Documents = append(out.Documents, documentJSON{Kind: d.Kind, Run: d.Run, Text: d.Text, Note: d.Note})
		if !seen[d.Kind] {
			seen[d.Kind] = true
			sum.Kinds = append(sum.Kinds, d.Kind)
		}
	}
	from, err := h.store.ProposalOf(c.ID)
	if err != nil {
		return cardJSON{}, err
	}
	if from != nil {
		out.FromProposal = &originJSON{ID: from.ID, Card: from.Card, Run: from.Run}
	}
	sum.Cost = store.CostOf(runs)
	sum.Interventions = store.Interventions(history)
	out.tileJSON = tile(sum)
	if sum.InFlight != nil && h.act != nil {
		for _, a := range h.act.Of(sum.InFlight.ID) {
			out.Activity = append(out.Activity, activityJSON{At: a.At, Kind: a.Kind, Tool: a.Tool, Text: a.Text})
		}
	}
	return out, nil
}

// create writes a new card of its repo, with its predecessors and the card it
// fixes, if any, and starts it: to spec, waiting for a run, or queued while a
// predecessor is not done or its repo or the instance has no room under its
// limit. A repo the instance does not have, none with more than one, and a
// predecessor the store refuses are said with the reason, and no card is
// written.
func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text         *string `json:"text"`
		Repo         string  `json:"repo"`
		Predecessors []int64 `json:"predecessors"`
		Fixes        int64   `json:"fixes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text == nil {
		fail(w, http.StatusBadRequest, `the body must be {"text": …, "predecessors": [card, …], "fixes": card}, the predecessors and fixes optional`)
		return
	}
	if strings.TrimSpace(*body.Text) == "" {
		fail(w, http.StatusBadRequest, "a card needs a text")
		return
	}
	repo, err := h.repoOf(body.Repo)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := h.store.CreateIn(repo, *body.Text, body.Fixes, body.Predecessors...)
	if errors.Is(err, store.ErrPredecessor) || errors.Is(err, store.ErrFix) {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err == nil {
		c, err = h.store.StartWith(c.ID, h.info.Limits)
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

// repoNames are the instance's repos' names: Info.Repos, or kiln when it is
// empty.
func (h *handler) repoNames() []string {
	if len(h.info.Repos) == 0 {
		return []string{store.KilnRepo}
	}
	names := make([]string, 0, len(h.info.Repos))
	for _, r := range h.info.Repos {
		names = append(names, r.Name)
	}
	return names
}

// repoOf is the repo a new card names, by its name: one of the instance's
// repos, or, left out or empty, the instance's one repo. A name the instance
// does not have, and none with more than one, are refused with the reason.
func (h *handler) repoOf(name string) (string, error) {
	repos := h.repoNames()
	if name == "" {
		if len(repos) == 1 {
			return repos[0], nil
		}
		return "", fmt.Errorf(`the instance has more than one repo; name one in "repo": %s`, strings.Join(repos, ", "))
	}
	if !slices.Contains(repos, name) {
		return "", fmt.Errorf("the instance has no repo %q; its repos: %s", name, strings.Join(repos, ", "))
	}
	return name, nil
}

func (h *handler) decide(w http.ResponseWriter, r *http.Request) {
	id := cardID(r)
	if id == 0 {
		fail(w, http.StatusNotFound, fmt.Sprintf("no card %s", r.PathValue("id")))
		return
	}
	var body struct {
		Action      store.Action `json:"action"`
		Note        string       `json:"note"`
		WorkflowYes bool         `json:"workflow_yes"`
		// The tip of the change the page showed when the developer said yes.
		Tip string `json:"tip"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, `the body must be {"action": …, "note": …}, the note optional`)
		return
	}
	// Whether the card can be published is a fact of its repo. A card's repo
	// is written once, when the card is, so it is read here, before the
	// move's transaction.
	card, err := h.store.Card(id)
	switch {
	case errors.Is(err, store.ErrNoCard):
		fail(w, http.StatusNotFound, fmt.Sprintf("no card %d", id))
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	opts := store.Options{Publishing: h.info.Publishing[card.Repo], Limits: h.info.Limits}
	if body.Action == store.Publish {
		// A change to the workflow files is published only after the
		// developer has seen them and said yes, for this very change, which
		// the yes is kept with (ADR 0060).
		var ch *Change
		var err error
		if h.info.Change != nil {
			ch, err = h.info.Change(r.Context(), id)
		}
		if err != nil {
			fail(w, http.StatusInternalServerError, "the change cannot be read, so it is not published: "+err.Error())
			return
		}
		if ch != nil && len(ch.Workflows) > 0 {
			switch {
			case !body.WorkflowYes:
				h.refuse(w, id, "this change touches workflow files, which the Git host runs as soon as it is published; "+
					`publish it with {"action": "publish", "workflow_yes": true, "tip": …} once you have read them`, ch.Workflows, ch.Tip)
				return
			case body.Tip != ch.Tip:
				h.refuse(w, id, "the change is no longer the one you said yes to; read its workflow files again", ch.Workflows, ch.Tip)
				return
			}
			opts.WorkflowYes = ch.Tip
		}
	}
	c, err := h.store.DecideWith(id, body.Action, body.Note, opts)
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
		h.refuse(w, c.ID, err.Error(), nil, "")
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

// The developer's moves on a proposal (ADR 0033).
const (
	makeCard = "make-card"
	dismiss  = "dismiss"
)

// decideProposal makes a move of the developer's on a proposal: make a card
// from it, which answers 201 with the proposal and the card, or dismiss it,
// with a note that may be left out. A proposal acted on already is refused
// with 409, the sentence and the proposal.
func (h *handler) decideProposal(w http.ResponseWriter, r *http.Request) {
	id := cardID(r)
	if id == 0 {
		fail(w, http.StatusNotFound, fmt.Sprintf("no proposal %s", r.PathValue("id")))
		return
	}
	var body struct {
		Action string  `json:"action"`
		Note   *string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		!(body.Action == makeCard && body.Note == nil || body.Action == dismiss) {
		fail(w, http.StatusBadRequest, `the body must be {"action": "make-card"} or {"action": "dismiss", "note": …}, the note optional`)
		return
	}
	var c store.Card
	var p store.Proposal
	var err error
	if body.Action == makeCard {
		// A card of a repo that is not in the settings must not get an open
		// card: the next start would be refused for it.
		if on, perr := h.store.Proposal(id); perr == nil {
			if from, cerr := h.store.Card(on.Card); cerr == nil && !slices.Contains(h.repoNames(), from.Repo) {
				out, jerr := h.proposalJSON(on)
				if jerr != nil {
					fail(w, http.StatusInternalServerError, jerr.Error())
					return
				}
				msg := fmt.Sprintf("proposal %d was noticed on a card of the repo %q, which is not in the settings, so no card is made from it; dismiss it instead", id, from.Repo)
				reply(w, http.StatusConflict, map[string]any{"error": run.Printable(msg), "proposal": out})
				return
			}
		}
		c, p, err = h.store.MakeCard(id)
	} else {
		var note string
		if body.Note != nil {
			note = *body.Note
		}
		p, err = h.store.Dismiss(id, note)
	}
	switch {
	case errors.Is(err, store.ErrNoProposal):
		fail(w, http.StatusNotFound, fmt.Sprintf("no proposal %d", id))
		return
	case errors.Is(err, store.ErrNote):
		fail(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), store.ErrNote.Error()+": "))
		return
	case errors.Is(err, store.ErrRefused):
		out, perr := h.proposalJSON(p)
		if perr != nil {
			fail(w, http.StatusInternalServerError, perr.Error())
			return
		}
		// The sentence of the proposal, such as "proposal 7 was dismissed
		// already", without the words of the table of a card's moves.
		msg := strings.TrimPrefix(err.Error(), store.ErrRefused.Error()+": ")
		reply(w, http.StatusConflict, map[string]any{"error": run.Printable(msg), "proposal": out})
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := h.proposalJSON(p)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if body.Action == dismiss {
		reply(w, http.StatusOK, map[string]any{"proposal": out})
		return
	}
	card, err := h.cardJSON(c)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, http.StatusCreated, map[string]any{"proposal": out, "card": card})
}

// proposalJSON is a proposal's JSON, with the headline of the card it was
// noticed on read from the store.
func (h *handler) proposalJSON(p store.Proposal) (proposalJSON, error) {
	c, err := h.store.Card(p.Card)
	if err != nil {
		return proposalJSON{}, err
	}
	return proposal(p, headline(title(c.Text))), nil
}

// mergingOn is the developer's move that turns automatic merging on again.
const mergingOn store.Action = "merging-on"

// mergingOff is the board's state of automatic merging: nil while it is on.
func (h *handler) mergingOff() (*mergingJSON, error) {
	m, err := h.store.Merging()
	if err != nil || m.Off == "" {
		return nil, err
	}
	return &mergingJSON{Reason: m.Off, Failed: m.Failed, Moves: []moveJSON{{Action: mergingOn, Note: store.NoNote}}}, nil
}

// decideInstance makes a move of the developer's on the instance: turning
// automatic merging on again after a failed update (ADR 0019, rule 3).
func (h *handler) decideInstance(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action store.Action `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Action != mergingOn {
		fail(w, http.StatusBadRequest, `the body must be {"action": "merging-on"}`)
		return
	}
	err := h.store.TurnMergingOn()
	if err != nil && !errors.Is(err, store.ErrRefused) {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	merging, merr := h.mergingOff()
	if merr != nil {
		fail(w, http.StatusInternalServerError, merr.Error())
		return
	}
	if err != nil {
		reply(w, http.StatusConflict, map[string]any{"error": "automatic merging is on", "merging_off": merging})
		return
	}
	reply(w, http.StatusOK, map[string]any{"merging_off": merging})
}

// refuse answers a move that is refused with 409: the sentence, the card's
// state and the moves it allows, and the workflow files that need the
// developer's yes and the tip of the change they are in, if that is why.
func (h *handler) refuse(w http.ResponseWriter, id int64, msg string, workflows []string, tip string) {
	c, err := h.store.Card(id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	moves, err := h.store.Moves(c, h.info.Publishing[c.Repo])
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{"error": run.Printable(msg), "stage": c.Stage, "waits_for": c.WaitsFor, "moves": movesJSON(moves)}
	if workflows != nil {
		out["workflows"], out["tip"] = workflows, tip
	}
	reply(w, http.StatusConflict, out)
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

// did says what the run's entry history[i] did, when the rule's entry follows
// it in the same step, and is "" for every other entry. It tells the step by
// the time: EndRunByRule writes the run's entry and the rule's entry in one
// transaction of change, with one time (store.go, "The card's row and its
// entries carry the same time"), and every other change takes its own time.
// Consecutive numbers do not tell it.
func did(history []store.Entry, i int, endings map[string]store.Ending) string {
	e := history[i]
	if e.Cause != store.CauseRun || e.Run == "" || i+1 >= len(history) {
		return ""
	}
	next := history[i+1]
	if next.Cause != store.CauseRule || next.Run != "" || !next.At.Equal(e.At) {
		return ""
	}
	switch {
	case endings[e.Run] == store.Failed:
		return "failed"
	case e.Stage == store.Review && e.WaitsFor == store.Approval:
		return "passed"
	// The prefix is the one laterDestination in internal/tick writes.
	case e.Stage == store.Review && e.WaitsFor == store.Developer && strings.HasPrefix(e.Reason, "the review sends the change back"):
		return "sent back"
	}
	return "finished"
}
