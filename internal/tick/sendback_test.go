package tick

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

// approvedCard writes a card whose spec names claude-haiku-5-5 and which the
// developer approved, so that it waits for its first implement run, with its
// branch put in place once, as a save would. It returns the card's ID.
func (p *publishing) approvedCard(t *testing.T, s *store.Store) int64 {
	t.Helper()
	c, err := s.Create("A card whose review goes back.")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(c.ID, waiting, store.CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("%s-%d-1", p.instance, c.ID)
	if err := s.AddRun(store.RunRow{ID: id, Card: c.ID, Commit: p.commit, Model: "claude-opus-5-5", Effort: "high"}, MaxRuns); err != nil {
		t.Fatal(err)
	}
	spec := "# Spec\n\nModel for implement: `claude-haiku-5-5`\n\nPull request title: `" + publishTitle + "`\n\n## 1. What\n"
	if _, err := s.EndRun(id, store.Finished, "staged", finished, store.Cost{}, &store.Doc{Kind: store.KindSpec, Text: spec}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(c.ID, store.Approve, ""); err != nil {
		t.Fatal(err)
	}
	p.save(t, c.ID, "refs/heads/main", map[string]string{"NOTES.md": "# Notes\nA line.\n"}, id)
	return c.ID
}

// entryLine is an entry as the tests of send-backs compare it.
func entryLine(e store.Entry) string {
	return fmt.Sprintf("%s/%s by %s", e.Stage, e.WaitsFor, e.Cause)
}

// TestSentBackByTheRule has a card's reviews go back twice and then pass, on
// one core, with no move of the developer's after the approval: the rule sends
// the change back to implement twice, the implement runs after a send-back are
// told the last review and run on claude-opus-5-5, and the first on the
// spec's model.
func TestSentBackByTheRule(t *testing.T) {
	p := newPublishing(t)
	p.cfg.PublishCommand = ""
	var card int64
	p.with(t, func(s *store.Store) { card = p.approvedCard(t, s) })
	e := newFakeEngine()
	e.reviews[card] = []string{goesBack, goesBack, passes}
	e.saves[card] = true
	e.open(card, "exit")
	core := p.start(t, e, 3)
	p.waitFor(t, card, reviewPassed, "")
	core.stop(t)

	p.look(t, func(s *store.Store) {
		h, err := s.History(card)
		if err != nil {
			t.Fatal(err)
		}
		// From the approval on.
		at := slices.IndexFunc(h, func(e store.Entry) bool { return e.State == implementWaits })
		var got []string
		for _, e := range h[at:] {
			got = append(got, entryLine(e))
			if e.Cause == store.CauseDeveloper && e.Number != h[at].Number {
				t.Errorf("an entry after the approval is the developer's: %+v", e)
			}
		}
		want := []string{"implement/run by developer",
			"review/run by run", "review/developer by run", "implement/run by rule",
			"review/run by run", "review/developer by run", "implement/run by rule",
			"review/run by run", "review/approval by run"}
		if !slices.Equal(got, want) {
			t.Fatalf("the history from the approval is\n%q\nwant\n%q", got, want)
		}
		for i, n := range map[int]int{3: 1, 6: 2} {
			want := fmt.Sprintf("the review sent the change back; implement runs again by the rule, send-back %d of 2", n)
			if r := h[at+i].Reason; r != want {
				t.Errorf("the rule's entry %d says %q, want %q", n, r, want)
			}
			if r := h[at+i-1].Reason; !strings.HasPrefix(r, "the review sends the change back; ") || h[at+i-1].Run == "" {
				t.Errorf("the run's entry before send-back %d is %+v", n, h[at+i-1])
			}
		}
		if c, _ := s.Card(card); c.SendBacks != 2 {
			t.Errorf("the card has %d send-backs, want 2", c.SendBacks)
		}

		// The model: the spec's before a send-back, the stronger one after.
		for n, want := range map[int]string{2: "claude-haiku-5-5", 4: "claude-opus-5-5", 6: "claude-opus-5-5"} {
			id := fmt.Sprintf("%s-%d-%d", p.instance, card, n)
			if r, err := s.Run(id); err != nil || r.Model != want || r.Effort != "medium" || e.models[id] != want {
				t.Errorf("the implement run %s is %+v (%v) with the inputs' model %s, want it on %s", id, r, err, e.models[id], want)
			}
		}
		// The implement runs after a send-back are told the last review.
		for n, review := range map[int]int{4: 3, 6: 5} {
			id := fmt.Sprintf("%s-%d-%d", p.instance, card, n)
			told := string(readFile(t, filepath.Join(p.cfg.Data, "instructions", id+".md")))
			last := e.reviewed[fmt.Sprintf("%s-%d-%d", p.instance, card, review)] + fmt.Sprintf("%s-%d-%d\n", p.instance, card, review)
			if !strings.HasPrefix(last, goesBack) || !strings.HasSuffix(told, "## The last review\n\n"+fenced(last, "markdown")) {
				t.Errorf("the implement run %s is told\n%s\nwant it to end with the review\n%s", id, told, last)
			}
		}
	})
}

// TestSentBackAThirdTime has a card's reviews go back three times: after two
// send-backs by the rule the card stops under "needs you", with the reason
// that says so and the third review, and no fourth implement run starts. The
// developer's moves are as before.
func TestSentBackAThirdTime(t *testing.T) {
	p := newPublishing(t)
	p.cfg.PublishCommand = ""
	var card int64
	p.with(t, func(s *store.Store) { card = p.approvedCard(t, s) })
	e := newFakeEngine()
	e.reviews[card] = []string{goesBack, goesBack, goesBack}
	e.saves[card] = true
	e.open(card, "exit")
	core := p.start(t, e, 3)
	const third = "the review sent the change back a third time; the rule sends a change back twice at most, so it waits for you"
	last := p.waitFor(t, card, reviewNeedsYou, third)
	e.waitTicks(t, 3)
	core.stop(t)

	if last.Reason != third || last.Cause != store.CauseRule || last.Run != "" {
		t.Errorf("the last entry is %+v", last)
	}
	p.look(t, func(s *store.Store) {
		c, err := s.Card(card)
		if err != nil || c.SendBacks != 3 {
			t.Errorf("the card is %+v (%v), want 3 send-backs", c, err)
		}
		if n, _ := s.RunCount(card); n != 7 {
			t.Errorf("the card has %d runs, want 7: no fourth implement run", n)
		}
		third := fmt.Sprintf("%s-%d-7", p.instance, card)
		d, err := s.Newest(card, store.KindReport, store.KindReview)
		if err != nil || d.Run != third || !strings.HasPrefix(d.Text, goesBack+"\n\n- A finding of "+third) {
			t.Errorf("the newest document is %+v (%v), want the third review with its finding", d, err)
		}
		moves, err := s.Moves(c, false)
		if err != nil {
			t.Fatal(err)
		}
		var names []store.Action
		for _, m := range moves {
			names = append(names, m.Action)
		}
		if want := []store.Action{store.Again, store.SendBack, store.ToSpec, store.Drop}; !slices.Equal(names, want) {
			t.Errorf("the moves are %v, want %v", names, want)
		}
		// The developer's send-back still sends it back, and leaves the number.
		c, err = s.Decide(card, store.SendBack, "")
		if err != nil || c.State != implementWaits || c.SendBacks != 3 {
			t.Errorf("the developer's send-back: %+v, %v", c, err)
		}
	})
}

// TestSentBackKilled kills a core at the end of a review that goes back:
// nothing of the end is written, and the next core writes it once.
func TestSentBackKilled(t *testing.T) {
	p := newPublishing(t)
	p.cfg.PublishCommand = ""
	var card int64
	p.with(t, func(s *store.Store) { card = p.reviewing(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"}) })
	run := fmt.Sprintf("%s-%d-3", p.instance, card)
	cmd, done := startChild(t, child{Command: "core", Config: p.cfg, ExitAt: "review/before-end",
		Reviews: map[int64][]string{card: {goesBack}}})
	if code := <-done; code != exitAtPoint {
		t.Fatalf("the core exited with %d, want %d\n%s%s", code, exitAtPoint, cmd.Stdout, cmd.Stderr)
	}
	p.look(t, func(s *store.Store) {
		c, _ := s.Card(card)
		r, err := s.Run(run)
		if err != nil || r.Ending != "" || c.State != reviewWaits || c.SendBacks != 0 {
			t.Errorf("after the kill: the card %+v, the run %+v (%v), want the run in flight and nothing of its end", c, r, err)
		}
	})

	e := newFakeEngine()
	e.reviews[card] = []string{goesBack}
	core := p.start(t, e, 3)
	p.waitFor(t, card, implementWaits, "the review sent the change back; implement runs again by the rule, send-back 1 of 2")
	core.stop(t)
	p.look(t, func(s *store.Store) {
		h, err := s.History(card)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range h {
			if e.Cause == store.CauseRule && e.State == implementWaits {
				n++
			}
		}
		if c, _ := s.Card(card); n != 1 || c.SendBacks != 1 {
			t.Errorf("%d entries of the rule's send-back, and the card is %+v; want 1 and 1", n, c)
		}
		if r, _ := s.Run(run); r.Ending != store.Finished {
			t.Errorf("the review run is %+v", r)
		}
	})
}

// TestSentBackRule is the rule's step after a review that goes back, by the
// number of send-backs the card had.
func TestSentBackRule(t *testing.T) {
	for _, tc := range []struct {
		n      int
		to     store.State
		reason string
	}{
		{0, implementWaits, "the review sent the change back; implement runs again by the rule, send-back 1 of 2"},
		{1, implementWaits, "the review sent the change back; implement runs again by the rule, send-back 2 of 2"},
		{2, reviewNeedsYou, "the review sent the change back a third time; the rule sends a change back twice at most, so it waits for you"},
		{3, reviewNeedsYou, "the review sent the change back again, send-back 4; the rule sends a change back twice at most, so it waits for you"},
	} {
		r := sentBackRule(tc.n)
		if r.To != tc.to || r.Reason != tc.reason || !r.SendBack || r.Publish {
			t.Errorf("at %d: %+v", tc.n, r)
		}
	}
}
