package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

// needsYou is where a review that sends the change back leaves a card.
var needsYou = store.State{Stage: store.Review, WaitsFor: store.Developer}

// reviewed writes a card whose review run r<card> sent the change back, so
// that it needs you, and returns the card's ID and the run.
func (f *fixture) reviewed(t *testing.T, text string, docs ...*store.Doc) (int64, string) {
	t.Helper()
	id := f.card(t, text, store.State{Stage: store.Review, WaitsFor: store.Run})
	run := fmt.Sprintf("ab12cd34-%d-1", id)
	if err := f.store.AddRun(store.RunRow{ID: run, Card: id, Commit: "c0ffee", Model: "m", Effort: "high"}, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EndRun(run, store.Finished, "the review sends the change back; exit 0", needsYou, store.Cost{}, docs...); err != nil {
		t.Fatal(err)
	}
	return id, run
}

// propose writes proposals of a card's review run as that run's end writes
// them, through a connection of its own, so that the board can be read before
// and after.
func (f *fixture) propose(t *testing.T, card int64, run string, titles ...string) {
	t.Helper()
	db, err := sql.Open("sqlite3", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, title := range titles {
		if _, err := db.Exec(`INSERT INTO proposals (card, run, number, title, text) VALUES (?, ?, ?, ?, ?)`,
			card, run, i+1, title, "The text of "+title+"."); err != nil {
			t.Fatal(err)
		}
	}
}

// decideProposal posts a move on a proposal.
func (f *fixture) decideProposal(t *testing.T, id int64, body string) map[string]any {
	t.Helper()
	w := f.do(t, "POST", fmt.Sprintf("/api/proposals/%d/decide", id), body)
	out := decode[map[string]any](t, w)
	out["code"] = float64(w.Code)
	return out
}

// TestBoardProposals shows a card's two proposals apart from "needs you": the
// cards are as they were before the proposals, and a dismiss leaves the tile's
// reason.
func TestBoardProposals(t *testing.T) {
	f := newFixture(t)
	card, run := f.reviewed(t, "The noticed card. With more.")
	before := decode[boardJSON](t, f.do(t, "GET", "/api/board", ""))
	if len(before.Proposals) != 0 || !strings.Contains(f.do(t, "GET", "/api/board", "").Body.String(), `"proposals":[]`) {
		t.Errorf("a board without proposals has %+v", before.Proposals)
	}
	f.propose(t, card, run, "A leak in bar.go. And more.", "")

	b := decode[boardJSON](t, f.do(t, "GET", "/api/board", ""))
	want := []proposalJSON{
		{ID: 1, Card: card, CardHeadline: "The noticed card.", Run: run, Title: "A leak in bar.go. And more.", Headline: "A leak in bar.go.",
			Text: "The text of A leak in bar.go. And more..", State: "open"},
		{ID: 2, Card: card, CardHeadline: "The noticed card.", Run: run, Text: "The text of .", State: "open"},
	}
	if !reflect.DeepEqual(b.Proposals, want) {
		t.Errorf("the board's proposals are %+v, want %+v", b.Proposals, want)
	}
	if !reflect.DeepEqual(b.Cards, before.Cards) {
		t.Errorf("the cards with proposals are %+v, were %+v", b.Cards, before.Cards)
	}
	waiting := 0
	for _, c := range b.Cards {
		if c.WaitsFor == store.Developer {
			waiting++
		}
	}
	if c := b.Cards[0]; waiting != 1 || c.State() != needsYou || c.Reason != "the review sends the change back; exit 0" {
		t.Errorf("%d cards wait for you; the tile is %+v", waiting, c)
	}

	if out := f.decideProposal(t, 1, `{"action":"dismiss"}`); out["code"] != 200.0 {
		t.Fatalf("the dismiss: %v", out)
	}
	b = decode[boardJSON](t, f.do(t, "GET", "/api/board", ""))
	if !reflect.DeepEqual(b.Cards, before.Cards) || len(b.Proposals) != 1 || b.Proposals[0].ID != 2 {
		t.Errorf("after a dismiss: the cards %+v, the proposals %+v", b.Cards, b.Proposals)
	}
	c := decode[cardJSON](t, f.do(t, "GET", fmt.Sprintf("/api/cards/%d", card), ""))
	if c.Reason != "the review sends the change back; exit 0" || c.Interventions != 0 {
		t.Errorf("the card after a dismiss: the reason %q, %d interventions", c.Reason, c.Interventions)
	}
	if last := c.History[len(c.History)-1]; last.Proposal != 1 || last.Reason != "dismissed proposal 1" || last.Note != "" {
		t.Errorf("the entry of the dismiss is %+v", last)
	}
}

// State is a tile's stage and wait.
func (t tileJSON) State() store.State { return store.State{Stage: t.Stage, WaitsFor: t.WaitsFor} }

// TestMakeCardFromProposal makes a card from a proposal: a backlog card, not
// started, that remembers where it came from; a second time is refused.
func TestMakeCardFromProposal(t *testing.T) {
	f := newFixture(t)
	card, run := f.reviewed(t, "The noticed card.")
	f.propose(t, card, run, "A leak")

	w := f.do(t, "POST", "/api/proposals/1/decide", `{"action":"make-card"}`)
	if w.Code != 201 {
		t.Fatalf("make-card: %d %s", w.Code, w.Body)
	}
	out := decode[struct {
		Proposal proposalJSON `json:"proposal"`
		Card     cardJSON     `json:"card"`
	}](t, w)
	made := out.Card
	if made.Stage != store.Backlog || made.WaitsFor != store.Developer || made.Text != "A leak\n\nThe text of A leak." ||
		len(made.History) != 1 || made.History[0].Cause != store.CauseDeveloper || made.Interventions != 0 {
		t.Errorf("the card made is %+v", made)
	}
	if made.FromProposal == nil || *made.FromProposal != (originJSON{ID: 1, Card: card, Run: run}) {
		t.Errorf("the card made comes from %+v", made.FromProposal)
	}
	if p := out.Proposal; p.State != "made" || p.Made != made.ID {
		t.Errorf("the proposal after make-card is %+v", p)
	}
	c := decode[cardJSON](t, f.do(t, "GET", fmt.Sprintf("/api/cards/%d", made.ID), ""))
	if c.FromProposal == nil || c.FromProposal.Card != card {
		t.Errorf("GET of the card made: from %+v", c.FromProposal)
	}
	noticed := decode[cardJSON](t, f.do(t, "GET", fmt.Sprintf("/api/cards/%d", card), ""))
	if noticed.FromProposal != nil || noticed.Interventions != 0 || noticed.State() != needsYou {
		t.Errorf("the noticed card: %+v", noticed)
	}
	if last := noticed.History[len(noticed.History)-1]; last.Proposal != 1 || last.Reason != fmt.Sprintf("made card #%d from proposal 1", made.ID) {
		t.Errorf("the noticed card's last entry is %+v", last)
	}
	if b := decode[boardJSON](t, f.do(t, "GET", "/api/board", "")); len(b.Proposals) != 0 || len(b.Cards) != 2 {
		t.Errorf("the board after make-card: %d proposals, %d cards", len(b.Proposals), len(b.Cards))
	}

	again := f.decideProposal(t, 1, `{"action":"make-card"}`)
	if again["code"] != 409.0 || again["proposal"].(map[string]any)["state"] != "made" ||
		again["error"] != fmt.Sprintf("proposal 1 was made into card #%d already", made.ID) {
		t.Errorf("a second make-card: %v", again)
	}
	if b, _ := f.store.Board(); len(b) != 2 {
		t.Errorf("a second make-card wrote a card: %d cards", len(b))
	}
}

// TestDismissProposal dismisses a proposal without a note and one with, and
// refuses every move after.
func TestDismissProposal(t *testing.T) {
	f := newFixture(t)
	card, run := f.reviewed(t, "The noticed card.")
	f.propose(t, card, run, "One", "Two")
	was, _ := f.store.Card(card)

	if out := f.decideProposal(t, 1, fmt.Sprintf(`{"action":"dismiss","note":%q}`, strings.Repeat("x", 16<<10+1))); out["code"] != 400.0 {
		t.Errorf("a note over 16 KiB: %v", out)
	}
	out := f.decideProposal(t, 1, `{"action":"dismiss"}`)
	if p := out["proposal"].(map[string]any); out["code"] != 200.0 || p["state"] != "dismissed" || p["note"] != "" {
		t.Errorf("a dismiss without a note: %v", out)
	}
	out = f.decideProposal(t, 2, `{"action":"dismiss","note":"not a bug"}`)
	if p := out["proposal"].(map[string]any); out["code"] != 200.0 || p["state"] != "dismissed" || p["note"] != "not a bug" {
		t.Errorf("a dismiss with a note: %v", out)
	}
	c := decode[cardJSON](t, f.do(t, "GET", fmt.Sprintf("/api/cards/%d", card), ""))
	h := c.History[len(c.History)-2:]
	if h[0].Reason != "dismissed proposal 1" || h[0].Note != "" || h[1].Reason != "dismissed proposal 2: not a bug" || h[1].Note != "not a bug" {
		t.Errorf("the entries of the dismisses: %+v", h)
	}
	for _, id := range []int64{1, 2} {
		for _, body := range []string{`{"action":"make-card"}`, `{"action":"dismiss"}`} {
			if out := f.decideProposal(t, id, body); out["code"] != 409.0 {
				t.Errorf("%s on proposal %d after a dismiss: %v", body, id, out)
			}
		}
	}
	if now, _ := f.store.Card(card); now != was {
		t.Errorf("the noticed card is %+v, was %+v", now, was)
	}
}

// TestMeasuresProposals counts the proposals made, accepted and dismissed.
func TestMeasuresProposals(t *testing.T) {
	f := newFixture(t)
	card, run := f.reviewed(t, "The noticed card.")
	f.propose(t, card, run, "One", "Two", "Three")
	f.decideProposal(t, 1, `{"action":"make-card"}`)
	f.decideProposal(t, 2, `{"action":"dismiss","note":"no"}`)
	m := decode[boardJSON](t, f.do(t, "GET", "/api/board", "")).Measures
	if m.Proposals != (proposalCountsJSON{Made: 3, Accepted: 1, Dismissed: 1}) {
		t.Errorf("the counts are %+v", m.Proposals)
	}
	if !strings.HasSuffix(m.Text, " · proposals: 3 made, 1 accepted, 1 dismissed") {
		t.Errorf("the text is %q", m.Text)
	}
	if body := f.do(t, "GET", "/api/board", "").Body.String(); !strings.Contains(body, `"proposals":{"made":3,"accepted":1,"dismissed":1}`) {
		t.Errorf("the board's JSON: %s", body)
	}
}

// TestProposalChecks holds a move on a proposal to the checks of every POST,
// and refuses what is not a move.
func TestProposalChecks(t *testing.T) {
	f := newFixture(t)
	card, run := f.reviewed(t, "The noticed card.")
	f.propose(t, card, run, "One")
	path := "/api/proposals/1/decide"
	for name, edit := range map[string]func(*http.Request){
		"no JSON":          func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"a foreign Origin": func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"a foreign Host":   func(r *http.Request) { r.Host = "evil.example:4747" },
	} {
		if w := f.do(t, "POST", path, `{"action":"dismiss"}`, edit); w.Code != 403 {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	for body, code := range map[string]int{
		`{"action":"make-card","note":"x"}`: 400,
		`{"action":"approve"}`:              400,
		`not JSON`:                          400,
	} {
		if w := f.do(t, "POST", path, body); w.Code != code {
			t.Errorf("%s: %d, want %d", body, w.Code, code)
		}
	}
	for _, p := range []string{"/api/proposals/9/decide", "/api/proposals/x/decide"} {
		if w := f.do(t, "POST", p, `{"action":"dismiss"}`); w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}

	accepting := false
	h, err := Handler(f.store, Info{Accepting: func() bool { return accepting }}, f.act, addr)
	if err != nil {
		t.Fatal(err)
	}
	f.h = h
	if w := f.do(t, "POST", path, `{"action":"dismiss"}`); w.Code != 503 {
		t.Errorf("before the report: %d", w.Code)
	}
	if open, _, _ := f.store.OpenProposals(); len(open) != 1 {
		t.Errorf("a refused move acted: %d open", len(open))
	}
	accepting = true
	if w := f.do(t, "POST", path, `{"action":"dismiss"}`); w.Code != 200 {
		t.Errorf("after the report: %d %s", w.Code, w.Body)
	}
}

// TestProposalsPage holds the page to its part for the proposals, apart from
// the strip.
func TestProposalsPage(t *testing.T) {
	page := newFixture(t).do(t, "GET", "/", "").Body.String()
	for _, want := range []string{
		`id="proposals"`, `aria-label="Proposals"`, `renderProposals(b.proposals)`, `renderStrip(b.cards)`,
		`"Make a card"`, `"Dismiss the proposal?"`, `"Why? Optional; the history keeps it."`, `"(no title)"`,
		`/api/proposals/`, `Proposals: made by reviews, accepted as cards, dismissed by you.`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	strip, proposals, measures := strings.Index(page, `id="strip"`), strings.Index(page, `id="proposals"`), strings.Index(page, `id="measures"`)
	if !(strip < proposals && proposals < measures) {
		t.Errorf("the proposals' part is at %d, not between the strip at %d and the measures at %d", proposals, strip, measures)
	}
}
