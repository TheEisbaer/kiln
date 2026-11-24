package store

import (
	"errors"
	"testing"
)

// The failed run's reasons of the tests of the retry.
const (
	firstFailed  = "exit 1, no result, not saved"
	secondFailed = "exit 137, killed for memory, no result, not saved"
	refusedRun   = "the upstream refused the Claude credential (401), 23 turns, $1.5000, not saved"
)

// TestRetrying reads whether the run a card waits for is the retry, and the
// first run's reason, from the card's last entry alone; and the note the next
// run is told, past the rule's entries after a failed or refused run.
func TestRetrying(t *testing.T) {
	const note = "Name the table."
	// A spec card sent back with a note, waiting for its run r1.
	setup := func(t *testing.T) (*Store, int64) {
		s := open(t)
		card := mustCreate(t, s, "a card")
		if _, err := s.Move(card.ID, State{Spec, Approval}, CauseRun); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Decide(card.ID, Again, note); err != nil {
			t.Fatal(err)
		}
		return s, card.ID
	}
	needsYou, waits := State{Spec, Developer}, State{Spec, Run}
	end := func(t *testing.T, s *Store, card int64, id, reason, rule string) {
		t.Helper()
		mustAddRun(t, s, theRun(card, id))
		if _, err := s.EndRunByRule(id, Failed, reason, needsYou, Rule{To: waits, Reason: rule}, Cost{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name     string
		steps    func(t *testing.T, s *Store, card int64)
		retrying bool
		first    string
		note     string
	}{
		{"after a failed run", func(t *testing.T, s *Store, card int64) {
			end(t, s, card, "r1", firstFailed, RetryPrefix+firstFailed)
		}, true, firstFailed, note},
		{"after a refused retry", func(t *testing.T, s *Store, card int64) {
			end(t, s, card, "r1", firstFailed, RetryPrefix+firstFailed)
			end(t, s, card, "r2", refusedRun, RefusedRetryPrefix+firstFailed)
		}, true, firstFailed, note},
		{"after a refused first run", func(t *testing.T, s *Store, card int64) {
			end(t, s, card, "r1", refusedRun, RefusedReason)
		}, false, "", note},
		{"after two refusals in a row", func(t *testing.T, s *Store, card int64) {
			end(t, s, card, "r1", refusedRun, RefusedReason)
			end(t, s, card, "r2", refusedRun, RefusedReason)
		}, false, "", note},
		{"after the developer's again", func(t *testing.T, s *Store, card int64) {
			end(t, s, card, "r1", firstFailed, RetryPrefix+firstFailed)
			mustAddRun(t, s, theRun(card, "r2"))
			if _, err := s.EndRun("r2", Failed, secondFailed, needsYou, Cost{}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Decide(card, Again, "Try again."); err != nil {
				t.Fatal(err)
			}
		}, false, "", "Try again."},
		{"after a developer's note in the retry's words", func(t *testing.T, s *Store, card int64) {
			mustAddRun(t, s, theRun(card, "r1"))
			if _, err := s.EndRun("r1", Failed, firstFailed, needsYou, Cost{}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Decide(card, Again, RetryPrefix+firstFailed); err != nil {
				t.Fatal(err)
			}
		}, false, "", RetryPrefix + firstFailed},
		{"after a run's entry", func(t *testing.T, s *Store, card int64) {
			mustAddRun(t, s, theRun(card, "r1"))
			if _, err := s.EndRun("r1", Failed, firstFailed, needsYou, Cost{}); err != nil {
				t.Fatal(err)
			}
		}, false, "", ""},
		{"on a card that does not wait for a run", func(t *testing.T, s *Store, card int64) {
			if _, err := s.MoveWithReason(card, waits, needsYou, CauseRule, RetryPrefix+firstFailed); err != nil {
				t.Fatal(err)
			}
		}, false, "", ""},
		{"after a send-back by the rule", func(t *testing.T, s *Store, card int64) {
			if _, err := s.Move(card, State{Review, Run}, CauseRule); err != nil {
				t.Fatal(err)
			}
			mustAddRun(t, s, theRun(card, "r1"))
			if _, err := s.EndRunByRule("r1", Finished, "exit 0", State{Review, Developer}, Rule{To: State{Implement, Run},
				Reason: "the review sent the change back; implement runs again by the rule, send-back 1 of 2", SendBack: true}, Cost{}, reviewDoc()); err != nil {
				t.Fatal(err)
			}
		}, false, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, card := setup(t)
			c.steps(t, s, card)
			first, retrying, err := s.Retrying(card)
			if err != nil || retrying != c.retrying || first != c.first {
				t.Errorf("Retrying = %q, %v, %v; want %q, %v", first, retrying, err, c.first, c.retrying)
			}
			if got, err := s.Note(card); err != nil || got != c.note {
				t.Errorf("Note = %q, %v; want %q", got, err, c.note)
			}
		})
	}
	t.Run("no card", func(t *testing.T) {
		if _, _, err := open(t).Retrying(999); !errors.Is(err, ErrNoCard) {
			t.Errorf("Retrying of no card: %v", err)
		}
	})
}
