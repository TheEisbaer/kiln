package tick

import (
	"fmt"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/instructions"
	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/store"
)

// TestReviewProposals ends two review runs: one whose proposals.md has no
// title line keeps it as one proposal, as written, and one that wrote no
// proposals.md keeps none; both cards go where their verdicts send them.
func TestReviewProposals(t *testing.T) {
	b := newBench(t, 2)
	var malformed, none int64
	b.with(t, func(s *store.Store) {
		malformed = b.card(t, s, store.Review)
		none = b.card(t, s, store.Review)
	})
	const text = "Something is wrong in foo.go:12\nit panics on nil"
	e := newFakeEngine()
	e.proposals[malformed] = text
	core := b.start(t, e, 2)
	e.open(malformed, "exit")
	e.open(none, "exit")
	b.waitEnded(t, e, malformed)
	b.waitEnded(t, e, none)
	core.stop(t)
	b.assertEnded(t, map[int64]store.Ending{malformed: store.Finished, none: store.Finished})
	b.with(t, func(s *store.Store) {
		open, n, err := s.OpenProposals()
		if err != nil {
			t.Fatal(err)
		}
		if len(open) != 1 || n.Made != 1 {
			t.Fatalf("the proposals kept: %+v, %+v; want one", open, n)
		}
		if p := open[0]; p.Card != malformed || p.Run != b.runID(malformed, 3) || p.Number != 1 || p.Title != "" || p.Text != text {
			t.Errorf("the proposal kept is %+v, want the text as written, with no title", p)
		}
		for _, card := range []int64{malformed, none} {
			if c, _ := s.Card(card); c.State != reviewPassed {
				t.Errorf("card %d is %+v, want its review passed", card, c)
			}
		}
	})
}

// TestProposalsKilled kills a core at the end of a review run with two
// proposals, before the end is written and after: the next core keeps them
// once.
func TestProposalsKilled(t *testing.T) {
	const two = "Proposal: One\nfoo.go:1 fails.\n\nProposal: Two\nbar.go:2 fails.\n"
	for _, point := range []string{"before-end", "after-end"} {
		t.Run(point, func(t *testing.T) {
			b := newBench(t, 1)
			var card int64
			b.with(t, func(s *store.Store) { card = b.card(t, s, store.Review) })
			cmd, done := startChild(t, child{Command: "core", Config: b.cfg, ExitAt: "review/" + point,
				Reviews: map[int64][]string{card: {passes}}, Proposals: map[int64]string{card: two}})
			if code := <-done; code != exitAtPoint {
				t.Fatalf("the core exited with %d, want %d\n%s%s", code, exitAtPoint, cmd.Stdout, cmd.Stderr)
			}
			e := newFakeEngine()
			e.proposals[card] = two
			e.open(card, "exit")
			core := b.start(t, e, 1)
			b.waitEnded(t, e, card)
			e.waitTicks(t, 5)
			core.stop(t)
			b.with(t, func(s *store.Store) {
				open, _, err := s.OpenProposals()
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, p := range open {
					got = append(got, fmt.Sprintf("%d %s %s", p.Number, p.Run, p.Title))
				}
				run := b.runID(card, 3)
				if want := []string{"1 " + run + " One", "2 " + run + " Two"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("the proposals kept are %q, want %q", got, want)
				}
			})
		})
	}
}

// TestWithProposals is what a review run's proposals.md does to its end.
func TestWithProposals(t *testing.T) {
	const reason = "exit 0, result success"
	back := end{store.Finished, reviewNeedsYou, "the review sends the change back; " + reason, nil}
	for _, tc := range []struct {
		name   string
		file   run.Document
		reason string
		kept   int
	}{
		{"missing", run.Document{Problem: "the run wrote no proposals", Missing: true}, back.reason, 0},
		{"blank", run.Document{Problem: "the proposals is empty", Empty: true}, back.reason, 0},
		{"too large", run.Document{Problem: "the proposals is 300000 bytes, more than 262144"},
			"the review sends the change back; the proposals is 300000 bytes, more than 262144; " + reason, 0},
		{"two", run.Document{Text: []byte("Proposal: A\na\nProposal: B\nb\n")}, back.reason, 2},
		{"none said", run.Document{Text: []byte("Proposal: none\n")}, back.reason, 0},
	} {
		d, doc := back.withProposals(tc.file, reason)
		kept := 0
		if doc != nil {
			kept = len(doc.Proposals)
			if doc.Kind != store.KindProposals {
				t.Errorf("%s: the doc is of the kind %q", tc.name, doc.Kind)
			}
		}
		if d.reason != tc.reason || kept != tc.kept || d.to != back.to || d.ending != back.ending {
			t.Errorf("%s: %+v with %d proposals, want the reason %q and %d", tc.name, d, kept, tc.reason, tc.kept)
		}
	}
}

// TestReviewInstructionProposals asks a review run for its proposals in the
// shape the core reads, apart from its review.
func TestReviewInstructionProposals(t *testing.T) {
	for _, want := range []string{"/kiln/out/proposals.md", "Proposal: "} {
		if !strings.Contains(instructions.Review, want) {
			t.Errorf("the review instruction lacks %q", want)
		}
	}
	if h := strings.TrimPrefix(proposalsHeading, "## "); !strings.Contains(instructions.Review, h) {
		t.Errorf("the review instruction does not name the part %q", h)
	}
	if strings.Contains(instructions.Review, "Proposals, if any.") {
		t.Error("the review instruction still asks for proposals in review.md")
	}
}
