package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

// started is a card written and started, as the page writes it.
func (f *fixture) started(t *testing.T) int64 {
	t.Helper()
	return f.card(t, "A card", store.State{Stage: store.Spec, WaitsFor: store.Run})
}

// intervene leaves a card waiting for the developer in implement and makes n
// "again"s on it, the rule returning it each time; then it drops it when drop
// is set.
func (f *fixture) intervene(t *testing.T, id int64, n int, drop bool) {
	t.Helper()
	back := store.State{Stage: store.Implement, WaitsFor: store.Developer}
	if _, err := f.store.Move(id, back, store.CauseRule); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := f.store.Decide(id, store.Again, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.Move(id, back, store.CauseRule); err != nil {
			t.Fatal(err)
		}
	}
	if drop {
		if _, err := f.store.Decide(id, store.Drop, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fixture) merge(t *testing.T, id int64) {
	t.Helper()
	at := store.State{Stage: store.Review, WaitsFor: store.Host}
	if _, err := f.store.Move(id, at, store.CauseRule); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.MoveWithReason(id, at, store.State{Stage: store.Done, WaitsFor: store.Nothing}, store.CauseHost, store.MergedReason); err != nil {
		t.Fatal(err)
	}
}

func TestInterventionsJSON(t *testing.T) {
	f := newFixture(t)
	two := f.started(t)
	f.intervene(t, two, 1, true) // one "again" and one drop
	none := f.started(t)
	for _, path := range []string{"/api/board", fmt.Sprintf("/api/cards/%d", two)} {
		if body := f.do(t, "GET", path, "").Body.String(); !strings.Contains(body, `"interventions":2`) {
			t.Errorf("%s lacks interventions 2:\n%s", path, body)
		}
	}
	board := decode[boardJSON](t, f.do(t, "GET", "/api/board", ""))
	got := map[int64]int{}
	for _, c := range board.Cards {
		got[c.ID] = c.Interventions
	}
	if got[two] != 2 || got[none] != 0 {
		t.Errorf("the board's counts: %v", got)
	}
	body := f.do(t, "GET", fmt.Sprintf("/api/cards/%d", none), "").Body.String()
	if !strings.Contains(body, `"interventions":0`) {
		t.Errorf("a card only started lacks interventions 0:\n%s", body)
	}
}

func TestMeasures(t *testing.T) {
	f := newFixture(t)
	measures := func() string {
		b := f.do(t, "GET", "/api/board", "")
		if b.Code != 200 {
			t.Fatalf("the board answers %d: %s", b.Code, b.Body)
		}
		s := b.Body.String()
		i := strings.Index(s, `"measures":`)
		if i < 0 {
			t.Fatalf("no measures: %s", s)
		}
		return s[i:]
	}
	if got, want := measures(), `"measures":{"merged":0,"interventions":0,"per_merged":null,"dropped":0,"text":"0 merged · interventions per merged card: – · 0 dropped"}`; !strings.HasPrefix(got, want) {
		t.Errorf("an empty board: %s", got)
	}

	for _, n := range []int{0, 1, 1} {
		id := f.started(t)
		if n > 0 {
			f.intervene(t, id, n, false)
		}
		f.merge(t, id)
	}
	drop := f.started(t)
	f.intervene(t, drop, 0, true)
	open := f.started(t)
	f.intervene(t, open, 3, false)

	got := measures()
	for _, want := range []string{`"merged":3,"interventions":`, `"dropped":1`, `"text":"3 merged · interventions per merged card:`} {
		if !strings.Contains(got, want) {
			t.Errorf("the measures lack %s: %s", want, got)
		}
	}
	board := decode[boardJSON](t, f.do(t, "GET", "/api/board", ""))
	m := board.Measures
	if m.Merged != 3 || m.Dropped != 1 || m.PerMerged == nil || *m.PerMerged != float64(m.Interventions)/3 {
		t.Errorf("the figures: %+v", m)
	}
	if m.Interventions != 2 {
		t.Errorf("merged cards carry %d interventions, want 2 (the dropped and the open card count in none)", m.Interventions)
	}
	if want := "3 merged · interventions per merged card: 0.67 · 1 dropped"; m.Text != want {
		t.Errorf("the text %q, want %q", m.Text, want)
	}
}

func TestFigure(t *testing.T) {
	for v, want := range map[float64]string{0: "0", 1: "1", 0.5: "0.5", 2.0 / 3: "0.67", 10: "10", 1.25: "1.25", 1.999: "2"} {
		if got := figure(v); got != want {
			t.Errorf("figure(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestMeasuresPage(t *testing.T) {
	f := newFixture(t)
	page := f.do(t, "GET", "/", "").Body.String()
	for _, want := range []string{
		`id="measures"`, `renderMeasures(b.measures)`, `title="Over the whole history.`, `protected paths on the Git host`,
		`function interventionsText(n)`, `"1 intervention"`, `c.interventions > 0`, `id="p-interventions"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
}
