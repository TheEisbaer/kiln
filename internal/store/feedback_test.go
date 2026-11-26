package store

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The commits of the feedback tests.
const (
	feedA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	feedB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// feedbackReason is a reason of the Git host's feedback, with a fenced block.
const feedbackReason = ReviewReason + "\n\n```text\nann NOTES.md:2: say why\n```\n"

// atHostWith writes a card whose review passed, published as commit with a
// pull request's address, waiting for the Git host.
func atHostWith(t *testing.T, s *Store, commit string) int64 {
	t.Helper()
	card := mustCreate(t, s, "a card")
	if _, err := s.Move(card.ID, passed, CauseRun); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideWith(card.ID, Publish, "", Options{Publishing: true, WorkflowYes: "c0ffee"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.KeepPullRequest(card.ID, commit, "https://git.example/pull/1"); err != nil {
		t.Fatal(err)
	}
	return card.ID
}

// TestReviewFeedback moves a card at the Git host to "needs you" for a
// reviewer's feedback on its published commit, as no send-back, keeps its
// publication and sets its feedback mark; it refuses feedback on another
// commit, on a card without an address and on one no longer at the Git host,
// writing nothing.
func TestReviewFeedback(t *testing.T) {
	s := open(t)
	read := time.Date(2026, 11, 26, 10, 0, 0, 500, time.UTC)
	card := mustCreate(t, s, "a card")
	if _, err := s.Move(card.ID, passed, CauseRun); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideWith(card.ID, Publish, "", Options{Publishing: true, WorkflowYes: "c0ffee"}); err != nil {
		t.Fatal(err)
	}
	unchanged := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v, want ErrRefused", what, err)
		}
		h, _ := s.History(card.ID)
		p, _ := s.Publication(card.ID)
		if strings.HasPrefix(h[len(h)-1].Reason, ReviewReason) || !p.FeedbackSince.IsZero() {
			t.Errorf("%s: the last entry is %+v, the publication %+v", what, h[len(h)-1], p)
		}
	}
	_, err := s.ReviewFeedback(card.ID, feedB, read, feedbackReason)
	unchanged("a card without an address", err)
	if _, err := s.KeepPullRequest(card.ID, feedB, "https://git.example/pull/1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReviewFeedback(card.ID, feedA, read, feedbackReason)
	unchanged("another commit", err)
	if got.ID != card.ID || got.State != atTheHost {
		t.Errorf("another commit returns %+v", got)
	}
	if _, err := s.ReviewFeedback(card.ID, feedB, read, "something else"); err == nil || errors.Is(err, ErrRefused) {
		t.Errorf("a reason that is not feedback: %v", err)
	}
	before, _ := s.Publication(card.ID)

	got, err = s.ReviewFeedback(card.ID, feedB, read, feedbackReason)
	if err != nil || got.State != needsYou || got.SendBacks != 0 {
		t.Fatalf("the published commit: %+v, %v", got, err)
	}
	h, _ := s.History(card.ID)
	if last := h[len(h)-1]; last.Cause != CauseHost || last.Reason != feedbackReason || last.Run != "" || last.State != needsYou {
		t.Errorf("the entry is %+v", last)
	}
	after, err := s.Publication(card.ID)
	want := before
	want.FeedbackSince = read
	if err != nil || after != want {
		t.Errorf("the publication is %+v (%v), want %+v", after, err, want)
	}
	// A repeat is refused: the card is no longer at the Git host.
	_, err = s.ReviewFeedback(card.ID, feedB, read.Add(time.Hour), feedbackReason)
	if p, _ := s.Publication(card.ID); !errors.Is(err, ErrRefused) || !p.FeedbackSince.Equal(read) {
		t.Errorf("a repeat: %v, the mark %v", err, p.FeedbackSince)
	}
}

// TestFeedbackMoves offers send-back, to-spec, to-host and drop, and no again
// and no publish, from "needs you" after feedback; to-host only there. It puts
// the card back to the Git host with the same publication and the move's time
// as its mark.
func TestFeedbackMoves(t *testing.T) {
	s := open(t)
	card := atHostWith(t, s, feedB)
	if _, err := s.ReviewFeedback(card, feedB, time.Now(), feedbackReason); err != nil {
		t.Fatal(err)
	}
	c, _ := s.Card(card)
	m, err := s.Moves(c, true)
	if want := []Allowed{{SendBack, OptionalNote}, {ToSpec, OptionalNote}, {ToHost, NoNote}, {Drop, OptionalNote}}; err != nil || !slices.Equal(m, want) {
		t.Errorf("the moves after feedback are %v (%v), want %v", m, err, want)
	}
	// Feedback with no text is feedback too.
	if f := (Facts{Feedback: Feedback(needsYou, CauseHost, ReviewReason)}); !f.Feedback || Feedback(needsYou, CauseRule, ReviewReason) {
		t.Error("Feedback is not told by the cause host and the reason's start")
	}

	before, _ := s.Publication(card)
	if _, err := s.Decide(card, ToHost, "a note"); !errors.Is(err, ErrNote) {
		t.Errorf("to-host with a note: %v", err)
	}
	got, err := s.Decide(card, ToHost, "")
	if err != nil || got.State != atTheHost || got.SendBacks != 0 {
		t.Fatalf("to-host: %+v, %v", got, err)
	}
	h, _ := s.History(card)
	last := h[len(h)-1]
	after, _ := s.Publication(card)
	want := before
	want.FeedbackSince = last.At
	if last.Cause != CauseDeveloper || last.Reason != "" || after != want || !after.FeedbackSince.After(before.FeedbackSince) {
		t.Errorf("after to-host the last entry is %+v, the publication %+v, want %+v", last, after, want)
	}
	if ids, _ := s.AtHost(KilnRepo); ids[card] != feedB {
		t.Errorf("the cards at the Git host: %v", ids)
	}
	if _, err := s.Decide(card, ToHost, ""); !errors.Is(err, ErrRefused) {
		t.Errorf("a repeat of to-host: %v", err)
	}

	// After a closed pull request: again, and no to-host.
	closed := atHostWith(t, s, feedA)
	if _, err := s.PullRequestClosed(closed, feedA); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Card(closed)
	if m, _ := s.Moves(c, true); slices.Contains(m, Allowed{ToHost, NoNote}) || !slices.Contains(m, Allowed{Again, OptionalNote}) {
		t.Errorf("the moves after a closed pull request are %v", m)
	}
	if _, err := s.Decide(closed, ToHost, ""); !errors.Is(err, ErrRefused) {
		t.Errorf("to-host after a closed pull request: %v", err)
	}
	// After a failed publishing: no to-host.
	for _, f := range []Facts{{PublishFailed: true, Publishing: true}, {}} {
		if m := Moves(needsYou, f); slices.Contains(m, Allowed{ToHost, NoNote}) || !slices.Contains(m, Allowed{Again, OptionalNote}) {
			t.Errorf("the moves with %+v are %v", f, m)
		}
	}
}

// TestFromHost finds the newer of a failed check and the Git host's feedback,
// and neither once a newer review exists; FailedCheck finds only a failed
// check.
func TestFromHost(t *testing.T) {
	s := open(t)
	card := atHostWith(t, s, feedB)
	failed := CheckFailedReason + " for " + feedB + "; implement runs again by the rule, send-back 1 of 2"
	if _, err := s.CheckFailed(card, feedB, State{Implement, Run}, failed); err != nil {
		t.Fatal(err)
	}
	if e, ok, err := s.FromHost(card); err != nil || !ok || e.Reason != failed {
		t.Errorf("after a failed check: %+v, %v, %v", e, ok, err)
	}
	back := func() {
		t.Helper()
		if _, err := s.Move(card, atTheHost, CauseRule); err != nil {
			t.Fatal(err)
		}
	}
	back()
	if _, err := s.ReviewFeedback(card, feedB, time.Now(), feedbackReason); err != nil {
		t.Fatal(err)
	}
	if e, ok, err := s.FromHost(card); err != nil || !ok || e.Reason != feedbackReason || e.Cause != CauseHost {
		t.Errorf("after feedback: %+v, %v, %v", e, ok, err)
	}
	if _, ok, err := s.FailedCheck(card); err != nil || ok {
		t.Errorf("FailedCheck after feedback: %v, %v", ok, err)
	}
	// The developer's send-back: the feedback is still the newest.
	if _, err := s.Decide(card, SendBack, "a note"); err != nil {
		t.Fatal(err)
	}
	if e, ok, _ := s.FromHost(card); !ok || e.Reason != feedbackReason {
		t.Errorf("after the send-back: %+v, %v", e, ok)
	}
	back()
	if _, err := s.CheckFailed(card, feedB, State{Implement, Run}, failed); err != nil {
		t.Fatal(err)
	}
	if e, ok, _ := s.FromHost(card); !ok || e.Reason != failed {
		t.Errorf("after a newer failed check: %+v, %v", e, ok)
	}
	if err := s.AddRun(RunRow{ID: "r1", Card: card, Commit: "c", Model: "m", Effort: "high"}, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndRun("r1", Finished, "exit 0", State{Review, Approval}, Cost{}, &Doc{Kind: KindReview, Text: "Verdict: passes"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.FromHost(card); err != nil || ok {
		t.Errorf("after a review: %v, %v", ok, err)
	}
}

// TestMigrateV17 sets the feedback mark on every publication with an address,
// and on no other.
func TestMigrateV17(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	withAddress := atHostWith(t, s, feedB)
	without := mustCreate(t, s, "being published").ID
	if _, err := s.Move(without, passed, CauseRun); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideWith(without, Publish, "", Options{Publishing: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`ALTER TABLE publishes DROP COLUMN feedback_since; DROP TABLE corrections; PRAGMA user_version = 17`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	start := time.Now()
	s = reopen(t, path)
	if v := userVersion(t, s); v != version {
		t.Fatalf("the store is version %d, want %d", v, version)
	}
	if p, err := s.Publication(withAddress); err != nil || p.FeedbackSince.Before(start) || p.FeedbackSince.After(time.Now()) || p.Commit != feedB {
		t.Errorf("the publication with an address: %+v, %v", p, err)
	}
	if p, err := s.Publication(without); err != nil || !p.FeedbackSince.IsZero() {
		t.Errorf("the publication without one: %+v, %v", p, err)
	}
	// A new publishing keeps the mark.
	if _, err := s.MoveWithReason(withAddress, atTheHost, needsYou, CauseHost, ClosedReason); err != nil {
		t.Fatal(err)
	}
	was, _ := s.Publication(withAddress)
	if _, err := s.DecideWith(withAddress, Publish, "", Options{Publishing: true}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Publication(withAddress); !p.FeedbackSince.Equal(was.FeedbackSince) || p.Address != "" {
		t.Errorf("publishing again: %+v, the mark was %v", p, was.FeedbackSince)
	}
}

// TestFromHostAfterProposal still finds the feedback when the developer moved
// one of the passed review's proposals between the feedback and the
// send-back: a proposal's entry does not hide it, not even one that reads as
// the developer's to-host.
func TestFromHostAfterProposal(t *testing.T) {
	s := open(t)
	c := mustCreate(t, s, "a card")
	if _, err := s.Move(c.ID, State{Review, Run}, CauseRule); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(c.ID, "r1"))
	if _, err := s.EndRun("r1", Finished, "exit 0", passed, Cost{}, &Doc{Kind: KindProposals, Proposals: twoProposals}); err != nil {
		t.Fatal(err)
	}
	card := c.ID
	if _, err := s.DecideWith(card, Publish, "", Options{Publishing: true, WorkflowYes: "c0ffee"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.KeepPullRequest(card, feedB, "https://git.example/pull/1"); err != nil {
		t.Fatal(err)
	}
	ps, _, err := s.OpenProposals()
	if err != nil || len(ps) == 0 {
		t.Fatalf("the card's proposals: %v, %v", ps, err)
	}
	id := ps[0].ID
	if _, err := s.ReviewFeedback(card, feedB, time.Now(), feedbackReason); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Dismiss(id, ""); err != nil {
		t.Fatal(err)
	}
	// A move on a proposal is never the developer's to-host, even with the
	// state of the Git host on its entry.
	if _, err := s.db.Exec(`UPDATE entries SET stage = ?, waits_for = ? WHERE card = ? AND proposal IS NOT NULL`,
		atHost.Stage, atHost.WaitsFor, card); err != nil {
		t.Fatal(err)
	}
	if e, ok, err := s.FromHost(card); err != nil || !ok || e.Reason != feedbackReason {
		t.Errorf("after a dismissal: %+v, %v, %v", e, ok, err)
	}
	if _, err := s.Decide(card, SendBack, "fix it"); err != nil {
		t.Fatal(err)
	}
	if e, ok, err := s.FromHost(card); err != nil || !ok || e.Reason != feedbackReason {
		t.Errorf("after a dismissal and a send-back: %+v, %v, %v", e, ok, err)
	}
}

// TestFromHostAfterToHost passes over feedback that the developer's "Back to
// the Git host" follows: neither a send-back from the Git host nor one after a
// closed pull request finds it, and feedback newer than the move counts again.
func TestFromHostAfterToHost(t *testing.T) {
	s := open(t)
	card := atHostWith(t, s, feedB)
	if _, err := s.ReviewFeedback(card, feedB, time.Now(), feedbackReason); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(card, ToHost, ""); err != nil {
		t.Fatal(err)
	}
	if e, ok, err := s.FromHost(card); err != nil || ok {
		t.Errorf("after to-host: %+v, %v, %v", e, ok, err)
	}
	// The developer's send-back from the Git host.
	if _, err := s.DecideWith(card, SendBack, "rename the flag", Options{Publishing: true}); err != nil {
		t.Fatal(err)
	}
	if e, ok, err := s.FromHost(card); err != nil || ok {
		t.Errorf("after a send-back from the Git host: %+v, %v, %v", e, ok, err)
	}

	// Feedback, to-host, a closed pull request, then the send-back from it.
	other := atHostWith(t, s, feedA)
	if _, err := s.ReviewFeedback(other, feedA, time.Now(), feedbackReason); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(other, ToHost, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PullRequestClosed(other, feedA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(other, SendBack, "start again"); err != nil {
		t.Fatal(err)
	}
	if e, ok, err := s.FromHost(other); err != nil || ok {
		t.Errorf("after to-host, a closed pull request and a send-back: %+v, %v, %v", e, ok, err)
	}

	// Feedback, to-host, newer feedback, then the send-back: the newer is told.
	third := atHostWith(t, s, feedA)
	if _, err := s.ReviewFeedback(third, feedA, time.Now(), feedbackReason); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(third, ToHost, ""); err != nil {
		t.Fatal(err)
	}
	newer := ReviewReason + "\n\n```text\ncarol: a newer comment\n```\n"
	if _, err := s.ReviewFeedback(third, feedA, time.Now(), newer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(third, SendBack, ""); err != nil {
		t.Fatal(err)
	}
	if e, ok, err := s.FromHost(third); err != nil || !ok || e.Reason != newer {
		t.Errorf("after newer feedback and a send-back: %+v, %v, %v", e, ok, err)
	}
}
