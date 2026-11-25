package web

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

// workItemFixture is a fixture whose one repo has a publish command, and needs
// a work item number when needs is set.
func workItemFixture(t *testing.T, needs bool) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	act := NewActivity()
	h, err := Handler(s, Info{Instance: "ab12cd34", Repo: "/data/repo.git", Model: "m", Runs: 3,
		Publishing:    map[string]bool{store.KilnRepo: true},
		NeedsWorkItem: map[string]bool{store.KilnRepo: needs}}, act, addr)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{store: s, act: act, h: h, path: path}
}

func TestCreateWorkItem(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, "POST", "/api/cards", `{"text":"numbered","work_item":12}`)
	if w.Code != 201 {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	card := decode[cardJSON](t, w)
	if card.WorkItem != 12 || f.tileOf(t, card.ID).WorkItem != 12 {
		t.Errorf("the card's JSON has %d, the tile %d", card.WorkItem, f.tileOf(t, card.ID).WorkItem)
	}
	for _, body := range []string{`{"text":"x"}`, `{"text":"x","work_item":null}`} {
		w := f.do(t, "POST", "/api/cards", body)
		if w.Code != 201 || decode[cardJSON](t, w).WorkItem != 0 {
			t.Errorf("POST %s: %d %s", body, w.Code, w.Body)
		}
	}
	if got := f.do(t, "GET", fmt.Sprintf("/api/cards/%d", card.ID), "").Body.String(); !strings.Contains(got, `"work_item":12`) ||
		!strings.Contains(got, `"work_item_needed":false`) {
		t.Errorf("the card:\n%s", got)
	}

	cards := len(decode[boardJSON](t, f.do(t, "GET", "/api/board", "")).Cards)
	for _, tc := range []struct{ value, got string }{
		{`0`, `0`}, {`-3`, `-3`}, {`1.5`, `1.5`}, {`1e3`, `1e3`}, {`"12"`, `"12"`}, {`true`, `true`},
		{`2147483648`, `2147483648`}, {`{}`, `{}`}, {`99999999999999999999999999999999999999999999`, `9999999999999999999999999999999999999999`},
	} {
		body := `{"text":"x","work_item":` + tc.value + `}`
		w := f.do(t, "POST", "/api/cards", body)
		want := `"work_item" is a whole number from 1 to 2147483647, or left out; got ` + tc.got
		if got := decode[map[string]string](t, w)["error"]; w.Code != 400 || got != want {
			t.Errorf("POST %s: %d %q, want 400 %q", body, w.Code, got, want)
		}
	}
	if b, _ := f.store.Board(); len(b) != cards {
		t.Errorf("refused bodies wrote cards: %d, want %d", len(b), cards)
	}
}

func TestDecideWorkItem(t *testing.T) {
	f := workItemFixture(t, false)
	id := f.card(t, "a card", store.State{Stage: store.Spec, WaitsFor: store.Approval})
	w := f.do(t, "POST", fmt.Sprintf("/api/cards/%d/decide", id), `{"action":"approve","work_item":"8"}`)
	if got := decode[map[string]string](t, w)["error"]; w.Code != 400 || !strings.HasPrefix(got, `"work_item" is a whole number from 1`) {
		t.Fatalf("approve with \"8\": %d %q", w.Code, got)
	}
	w = f.do(t, "POST", fmt.Sprintf("/api/cards/%d/decide", id), `{"action":"approve","work_item":8}`)
	if w.Code != 200 || decode[cardJSON](t, w).WorkItem != 8 {
		t.Fatalf("approve with 8: %d %s", w.Code, w.Body)
	}
	if f.tileOf(t, id).WorkItem != 8 {
		t.Errorf("the tile has %d", f.tileOf(t, id).WorkItem)
	}

	again := f.card(t, "needs you", store.State{Stage: store.Implement, WaitsFor: store.Developer})
	w = f.do(t, "POST", fmt.Sprintf("/api/cards/%d/decide", again), `{"action":"again","work_item":3}`)
	if got := decode[map[string]string](t, w)["error"]; w.Code != 400 || got != "only approve and publish take a work item" {
		t.Errorf("again with a number: %d %q", w.Code, got)
	}
}

func TestPublishNeedsWorkItem(t *testing.T) {
	f := workItemFixture(t, true)
	id := f.card(t, "passed", store.State{Stage: store.Review, WaitsFor: store.Approval})
	card := decode[cardJSON](t, f.do(t, "GET", fmt.Sprintf("/api/cards/%d", id), ""))
	if !card.WorkItemNeeded {
		t.Error("work_item_needed is false for a card without a number")
	}
	path := fmt.Sprintf("/api/cards/%d/decide", id)
	w := f.do(t, "POST", path, `{"action":"publish"}`)
	want := "the repo kiln needs a work item number before publishing; give it with the approval or when you publish"
	if got := decode[map[string]string](t, w)["error"]; w.Code != 400 || got != want {
		t.Fatalf("publish without a number: %d %q", w.Code, got)
	}
	w = f.do(t, "POST", path, `{"action":"publish","work_item":4711}`)
	if w.Code != 200 {
		t.Fatalf("publish with a number: %d %s", w.Code, w.Body)
	}
	card = decode[cardJSON](t, w)
	if card.WorkItem != 4711 || card.WorkItemNeeded {
		t.Errorf("after publishing: %d, needed %v", card.WorkItem, card.WorkItemNeeded)
	}
}
