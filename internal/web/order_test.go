package web

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

var done = store.State{Stage: store.Done, WaitsFor: store.Nothing}

// tileOf is a card's tile on the board.
func (f *fixture) tileOf(t *testing.T, id int64) tileJSON {
	t.Helper()
	for _, c := range decode[boardJSON](t, f.do(t, "GET", "/api/board", "")).Cards {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("card %d is not on the board", id)
	return tileJSON{}
}

// TestCreatePredecessors writes a card after two others, one done: it is
// queued for the open one, on the board and on its page. A card with none
// has empty lists. Refused predecessors and a body whose list does not decode
// get 400, with the store's reason, and write no card.
func TestCreatePredecessors(t *testing.T) {
	f := newFixture(t)
	a := f.card(t, "A", done)
	b := f.card(t, "B", store.State{Stage: store.Spec, WaitsFor: store.Approval})
	w := f.do(t, "POST", "/api/cards", fmt.Sprintf(`{"text":"C","predecessors":[%d,%d]}`, a, b))
	if w.Code != 201 {
		t.Fatalf("POST /api/cards: %d %s", w.Code, w.Body)
	}
	c := decode[cardJSON](t, w)
	wantReason := fmt.Sprintf("%s#%d", store.QueuedPrefix, b)
	if c.Stage != store.Backlog || c.WaitsFor != store.Room || !reflect.DeepEqual(c.Predecessors, []int64{a, b}) ||
		!reflect.DeepEqual(c.QueuedFor, []int64{b}) || c.Reason != wantReason {
		t.Errorf("the new card: %+v", c.tileJSON)
	}
	if tl := f.tileOf(t, c.ID); !reflect.DeepEqual(tl.Predecessors, []int64{a, b}) || !reflect.DeepEqual(tl.QueuedFor, []int64{b}) || tl.Reason != wantReason {
		t.Errorf("the tile: %+v", tl)
	}
	if tl := f.tileOf(t, a); tl.Predecessors == nil || len(tl.Predecessors) != 0 || tl.QueuedFor == nil || len(tl.QueuedFor) != 0 {
		t.Errorf("a card with no predecessor: %+v", tl)
	}
	if strings.Contains(f.do(t, "GET", "/api/board", "").Body.String(), `"predecessors":null`) {
		t.Error("the board has a null list of predecessors")
	}

	// Once released, it waits for nothing on the board.
	if _, err := f.store.Move(b, done, store.CauseHost); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Release(); err != nil {
		t.Fatal(err)
	}
	if tl := f.tileOf(t, c.ID); tl.Stage != store.Spec || len(tl.QueuedFor) != 0 || len(tl.Predecessors) != 2 {
		t.Errorf("the released card's tile: %+v", tl)
	}

	// A card not started, with an open predecessor, is not queued and waits
	// for nothing.
	open := f.card(t, "Open", store.State{Stage: store.Spec, WaitsFor: store.Approval})
	held, err := f.store.Create("Not started", open)
	if err != nil {
		t.Fatal(err)
	}
	if tl := f.tileOf(t, held.ID); len(tl.QueuedFor) != 0 || !reflect.DeepEqual(tl.Predecessors, []int64{open}) {
		t.Errorf("a card that is not queued: %+v", tl)
	}
	if c := decode[cardJSON](t, f.do(t, "GET", fmt.Sprintf("/api/cards/%d", held.ID), "")); len(c.QueuedFor) != 0 {
		t.Errorf("the card's JSON of a card that is not queued waits for %v", c.QueuedFor)
	}

	dropped := f.card(t, "D", store.State{Stage: store.Spec, WaitsFor: store.Approval})
	if _, err := f.store.Decide(dropped, store.Drop, ""); err != nil {
		t.Fatal(err)
	}
	cards := len(decode[boardJSON](t, f.do(t, "GET", "/api/board", "")).Cards)
	next := int64(cards + 1)
	for _, tc := range []struct{ body, want string }{
		{fmt.Sprintf(`{"text":"x","predecessors":[%d]}`, next), "a card cannot be its own predecessor"},
		{`{"text":"x","predecessors":[99]}`, "no card 99 to wait for"},
		{fmt.Sprintf(`{"text":"x","predecessors":[%d]}`, dropped), fmt.Sprintf("card %d was dropped, so no card can wait for it", dropped)},
		{`{"text":"x","predecessors":"3"}`, `the body must be {"text": …, "predecessors": [card, …]}, the predecessors optional`},
	} {
		w := f.do(t, "POST", "/api/cards", tc.body)
		if got := decode[map[string]string](t, w)["error"]; w.Code != 400 || got != tc.want {
			t.Errorf("POST %s: %d %q, want 400 %q", tc.body, w.Code, got, tc.want)
		}
	}
	if b, _ := f.store.Board(); len(b) != cards {
		t.Errorf("refused bodies wrote cards: %d, want %d", len(b), cards)
	}
}

// TestNamedBy has a card's JSON name the cards that wait for it while they are
// not done.
func TestNamedBy(t *testing.T) {
	f := newFixture(t)
	p := f.card(t, "P", store.State{Stage: store.Spec, WaitsFor: store.Approval})
	named := func() []namedJSON {
		t.Helper()
		return decode[cardJSON](t, f.do(t, "GET", fmt.Sprintf("/api/cards/%d", p), "")).NamedBy
	}
	if got := named(); got == nil || len(got) != 0 {
		t.Errorf("a card no other names: %v", got)
	}
	c := decode[cardJSON](t, f.do(t, "POST", "/api/cards", fmt.Sprintf(`{"text":"The card after\n\nmore","predecessors":[%d]}`, p))).ID
	if got, want := named(), []namedJSON{{c, "The card after"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("named by %v, want %v", got, want)
	}
	if _, err := f.store.Decide(c, store.Drop, ""); err != nil {
		t.Fatal(err)
	}
	if got := named(); len(got) != 0 {
		t.Errorf("named by %v once the card is dropped", got)
	}
	d := decode[cardJSON](t, f.do(t, "POST", "/api/cards", fmt.Sprintf(`{"text":"D","predecessors":[%d]}`, p))).ID
	if _, err := f.store.Move(d, done, store.CauseRule); err != nil {
		t.Fatal(err)
	}
	if got := named(); len(got) != 0 {
		t.Errorf("named by %v once the card is done", got)
	}
}

// TestPagePredecessorDropped holds the page's words for a dropped predecessor
// to the store's.
func TestPagePredecessorDropped(t *testing.T) {
	f := newFixture(t)
	if w := f.do(t, "GET", "/", ""); !strings.Contains(w.Body.String(), `const PREDECESSOR_DROPPED = "`+store.PredecessorDroppedPrefix+`";`) {
		t.Errorf("the page does not hold %q", store.PredecessorDroppedPrefix)
	}
}
