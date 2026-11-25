package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

func TestCreateFixes(t *testing.T) {
	f := newFixture(t)
	merged := f.started(t)
	f.merge(t, merged)
	open := f.started(t)
	gone := f.started(t)
	if _, err := f.store.Decide(gone, store.Drop, ""); err != nil {
		t.Fatal(err)
	}

	w := f.do(t, "POST", "/api/cards", fmt.Sprintf(`{"text":"a fix","fixes":%d}`, merged))
	if w.Code != 201 {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	fix := decode[cardJSON](t, w)
	if fix.Fixes != merged || fix.FixedBy == nil || len(fix.FixedBy) != 0 {
		t.Errorf("the card's JSON: fixes %d, fixed_by %v", fix.Fixes, fix.FixedBy)
	}
	fixed := decode[cardJSON](t, f.do(t, "GET", fmt.Sprintf("/api/cards/%d", merged), ""))
	if len(fixed.FixedBy) != 1 || fixed.FixedBy[0] != fix.ID || fixed.Fixes != 0 {
		t.Errorf("the fixed card's JSON: fixes %d, fixed_by %v", fixed.Fixes, fixed.FixedBy)
	}
	if tl := f.tileOf(t, fix.ID); tl.Fixes != merged {
		t.Errorf("the board's tile has fixes %d", tl.Fixes)
	}
	body := f.do(t, "GET", fmt.Sprintf("/api/cards/%d", open), "").Body.String()
	if !strings.Contains(body, `"fixes":0`) || !strings.Contains(body, `"fixed_by":[]`) {
		t.Errorf("a card that fixes nothing:\n%s", body)
	}

	cards := len(decode[boardJSON](t, f.do(t, "GET", "/api/board", "")).Cards)
	for _, tc := range []struct{ body, want string }{
		{fmt.Sprintf(`{"text":"x","fixes":%d}`, open), fmt.Sprintf("card %d is not merged; only a merged card can be fixed", open)},
		{fmt.Sprintf(`{"text":"x","fixes":%d}`, gone), fmt.Sprintf("card %d was dropped; only a merged card can be fixed", gone)},
		{`{"text":"x","fixes":99}`, "no card 99 to fix"},
		{fmt.Sprintf(`{"text":"x","fixes":%d}`, fix.ID+1), fmt.Sprintf("card %d cannot fix itself", fix.ID+1)},
		{`{"text":"x","fixes":"7"}`, `the body must be {"text": …, "predecessors": [card, …], "fixes": card, "work_item": number}, the predecessors, fixes and work item optional`},
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

func TestMeasuresNeededFix(t *testing.T) {
	f := newFixture(t)
	a, b := f.started(t), f.started(t)
	f.merge(t, a)
	f.merge(t, b)
	for i := 0; i < 2; i++ {
		if w := f.do(t, "POST", "/api/cards", fmt.Sprintf(`{"text":"fix","fixes":%d}`, a)); w.Code != 201 {
			t.Fatalf("POST: %d %s", w.Code, w.Body)
		}
	}
	m := decode[boardJSON](t, f.do(t, "GET", "/api/board", "")).Measures
	if m.NeededFix != 1 || m.Merged != 2 {
		t.Errorf("the figures: %+v", m)
	}
	if want := "2 merged · interventions per merged card: 0 · needed a follow-up fix: 1 of 2 · 0 dropped · proposals: 0 made, 0 accepted, 0 dismissed"; m.Text != want {
		t.Errorf("the text %q, want %q", m.Text, want)
	}
	if body := f.do(t, "GET", "/api/board", "").Body.String(); !strings.Contains(body, `"needed_fix":1`) {
		t.Errorf("the board lacks needed_fix: %s", body)
	}
}

func TestFixesPage(t *testing.T) {
	f := newFixture(t)
	page := f.do(t, "GET", "/", "").Body.String()
	for _, want := range []string{
		`id="d-fixes"`, `body.fixes = fixes`, `"Fixes card "`, `"Fixed by card`, `a.href = "#card-" + id`,
		`Name the merged card it fixes by its number, such as 7.`,
		`A merged card needed a follow-up fix when a later card that you did not drop names it as the card it fixes; a fix you did not name is not counted.`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
}
