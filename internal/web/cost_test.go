package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

func TestBoardCost(t *testing.T) {
	f := newFixture(t)
	usd := func(v float64) *float64 { return &v }
	// ended adds a run in the card's stage, which ends the card at to.
	ended := func(card int64, id string, cost store.Cost, to store.State) {
		t.Helper()
		if err := f.store.AddRun(store.RunRow{ID: id, Card: card, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}, 8); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.EndRun(id, store.Finished, "exit 0", to, cost); err != nil {
			t.Fatal(err)
		}
	}
	run := func(stage store.Stage) store.State { return store.State{Stage: stage, WaitsFor: store.Run} }

	full := f.card(t, "Everything", run(store.Spec))
	ended(full, "r1", store.Cost{USD: usd(0.4012), Session: "s1"}, run(store.Implement))
	ended(full, "r2", store.Cost{USD: usd(0.3), Session: "s2"}, run(store.Implement))
	ended(full, "r3", store.Cost{USD: usd(0.4), Session: "s3"}, run(store.Review))
	ended(full, "r4", store.Cost{USD: usd(0.1333), Session: "s4"}, run(store.Review))
	ended(full, "r5", store.Cost{}, store.State{Stage: store.Review, WaitsFor: store.Developer})

	unknown := f.card(t, "Unknown", run(store.Spec))
	ended(unknown, "u1", store.Cost{}, store.State{Stage: store.Spec, WaitsFor: store.Developer})

	flying := f.card(t, "In flight", run(store.Spec))
	if err := f.store.AddRun(store.RunRow{ID: "f1", Card: flying, Commit: "c0ffee", Model: "claude-opus-5-5", Effort: "high"}, 8); err != nil {
		t.Fatal(err)
	}

	const text = "$1.23+?"
	const label = "Claude Code's estimate: $1.23+? (spec $0.40, implement $0.70, review $0.13+?)"
	body := f.do(t, "GET", "/api/board", "").Body.String()
	for _, want := range []string{
		`"cost":{"total":{"usd":1.2345,"runs":5,"unknown":1},"spec":{"usd":0.4012,"runs":1,"unknown":0},`,
		`"review":{"usd":0.1333,"runs":2,"unknown":1},"text":"` + text + `","label":"` + label + `"}`,
		`"text":"?","label":"Claude Code's estimate: ? (spec ?)"`,
		`"cost":null`,
	} {
		if !strings.Contains(strings.ReplaceAll(body, `\u0027`, "'"), want) {
			t.Errorf("the board lacks %s:\n%s", want, body)
		}
	}
	board := decode[boardJSON](t, f.do(t, "GET", "/api/board", ""))
	byID := map[int64]tileJSON{}
	for _, c := range board.Cards {
		byID[c.ID] = c
	}
	if c := byID[full].Cost; c == nil || c.Text != text || c.Label != label {
		t.Errorf("the full card's cost: %+v", c)
	}
	if c := byID[unknown].Cost; c == nil || c.Text != "?" {
		t.Errorf("the unknown card's cost: %+v", c)
	}
	if c := byID[flying].Cost; c != nil {
		t.Errorf("a card with a run in flight has the cost %+v", c)
	}
	if c := decode[cardJSON](t, f.do(t, "GET", fmt.Sprintf("/api/cards/%d", full), "")); c.Cost == nil || c.Cost.Text != text || c.Cost.Label != label {
		t.Errorf("the card's page cost: %+v", c.Cost)
	}
	for _, want := range []string{`"cost", c.cost.text`, `c.cost.label`, `id="p-cost"`} {
		if !strings.Contains(string(page), want) {
			t.Errorf("the page lacks %s", want)
		}
	}
}
