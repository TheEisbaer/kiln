package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Publication is what the store keeps of a card's publishing: the
// commit of the card's branch whose workflow files the developer said yes to
// with the move publish, if any; once the core has published it, the commit it
// published and the pull request's address; and the branch on the Git host it
// is published on, named at its first publishing and kept for every later
// one, so that one card has one pull request. A card never published has none
// of them. FeedbackSince is the card's feedback mark (ADR 0028): the Git
// host's feedback on its pull request counts from then on; the zero time
// before any.
type Publication struct {
	WorkflowYes   string
	Commit        string
	Address       string
	Branch        string
	FeedbackSince time.Time
}

// The reasons on the entries of publishing that the store tells apart.
const (
	// PublishFailedReason begins the reason of a card that publishing moved
	// to "needs you", from which the developer can publish again.
	PublishFailedReason = "publishing failed"
	// MergedReason is the reason of a card the Git host merged.
	MergedReason = "merged"
	// ClosedReason is the reason of a card whose pull request the Git host
	// closed without a merge, which then needs you.
	ClosedReason = "its pull request was closed without a merge"
	// ReviewReason begins the reason of a card that a reviewer's feedback on
	// its pull request moved from the Git host to "needs you", followed, when
	// the check result command printed any, by an empty line and the feedback
	// as a fenced block (ADR 0028).
	ReviewReason = "a reviewer commented on its pull request"
	// PublishedReason begins the reason of the entry that keeps a card's pull
	// request's address, followed by the address.
	PublishedReason = "published as "
)

// PublishFailed says whether a card's last entry, of the state, the cause and
// the reason, left it under "needs you" because its publishing failed. As for
// Dropped, the card carries no mark of it; its last entry says how it got
// there.
func PublishFailed(s State, cause Cause, reason string) bool {
	return s == (State{Review, Developer}) && cause == CauseRule && strings.HasPrefix(reason, PublishFailedReason)
}

// Merged says whether a card's last entry left it done because the Git host
// merged its change.
func Merged(s State, cause Cause, reason string) bool {
	return s == (State{Done, Nothing}) && cause == CauseHost && reason == MergedReason
}

// PullClosed says whether a card's last entry, of the state, the cause and
// the reason, left it under "needs you" because its pull request was closed
// without a merge. Like Merged, it is told by the cause host and the exact
// reason, which tells it from a failed publishing.
func PullClosed(s State, cause Cause, reason string) bool {
	return s == (State{Review, Developer}) && cause == CauseHost && reason == ClosedReason
}

// Feedback says whether a card's last entry, of the state, the cause and the
// reason, left it under "needs you" because a reviewer commented on its pull
// request. It is told by the cause host and the reason's start, which the
// feedback follows.
func Feedback(s State, cause Cause, reason string) bool {
	return s == (State{Review, Developer}) && cause == CauseHost && strings.HasPrefix(reason, ReviewReason)
}

// UnchangedReason begins the reason of a card whose implement run finished and
// changed nothing, followed by "; " and the run's own words.
const UnchangedReason = "the run changed nothing"

// Unchanged says whether a card's last entry, of the state, the cause and the
// reason, left it under "needs you" in implement because its run changed
// nothing. It is told by the cause run and the reason's start.
func Unchanged(s State, cause Cause, reason string) bool {
	return s == (State{Implement, Developer}) && cause == CauseRun && strings.HasPrefix(reason, UnchangedReason+"; ")
}

// readFacts reads the facts of a card that the store keeps (see Facts).
func readFacts(q querier, card int64) (Facts, error) {
	var f Facts
	if err := q.QueryRow(`SELECT EXISTS (SELECT 1 FROM runs WHERE card = ? AND ending IS NULL)`, card).Scan(&f.InFlight); err != nil {
		return Facts{}, err
	}
	var e Entry
	var reason sql.NullString
	err := q.QueryRow(`SELECT cause, stage, waits_for, reason FROM entries
		WHERE card = ? AND proposal IS NULL ORDER BY number DESC LIMIT 1`, card).
		Scan(&e.Cause, &e.Stage, &e.WaitsFor, &reason)
	switch {
	case err == nil:
		f.PublishFailed = PublishFailed(e.State, e.Cause, reason.String)
		f.PullClosed = PullClosed(e.State, e.Cause, reason.String)
		f.Feedback = Feedback(e.State, e.Cause, reason.String)
		f.Unchanged = Unchanged(e.State, e.Cause, reason.String)
	case !errors.Is(err, sql.ErrNoRows):
		return Facts{}, err
	}
	p, err := readPublication(q, card)
	if err != nil {
		return Facts{}, err
	}
	f.Published = p.Address != ""
	if f.all, f.Open, f.Dropped, err = readPredecessors(q, card); err != nil {
		return Facts{}, err
	}
	if err := readRoom(q, card, reason.String, &f); err != nil {
		return Facts{}, err
	}
	return f, nil
}

// Publication reads a card's publication; a card never published has an
// empty one.
func (s *Store) Publication(card int64) (Publication, error) {
	return readPublication(s.db, card)
}

func readPublication(q querier, card int64) (Publication, error) {
	var yes, commit, address, branch, since sql.NullString
	err := q.QueryRow(`SELECT workflow_yes, commit_id, address, branch, feedback_since FROM publishes WHERE card = ?`, card).
		Scan(&yes, &commit, &address, &branch, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return Publication{}, nil
	}
	if err != nil {
		return Publication{}, err
	}
	p := Publication{WorkflowYes: yes.String, Commit: commit.String, Address: address.String, Branch: branch.String}
	if since.Valid {
		if p.FeedbackSince, err = time.Parse(time.RFC3339Nano, since.String); err != nil {
			return Publication{}, fmt.Errorf("card %d's feedback mark: %w", card, err)
		}
	}
	return p, nil
}

// PublishBranch returns the branch a card is published on: the one kept, or
// else name, which is kept from now on. A card the developer never published
// has none, and is refused.
func (s *Store) PublishBranch(card int64, name string) (string, error) {
	if name == "" {
		return "", errors.New("a branch needs a name")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE publishes SET branch = ? WHERE card = ? AND branch IS NULL`, name, card)
	if err != nil {
		return "", err
	}
	if _, err := res.RowsAffected(); err != nil {
		return "", err
	}
	p, err := readPublication(tx, card)
	if err != nil {
		return "", err
	}
	if p.Branch == "" {
		return "", fmt.Errorf("card %d has no publication", card)
	}
	return p.Branch, tx.Commit()
}

// Publishing lists the cards of the named repo that wait for the Git host and
// have no pull request's address yet, which the core publishes, by ID.
func (s *Store) Publishing(repo string) ([]int64, error) {
	return s.atHost(repo, `address IS NULL`)
}

// AtHost lists the cards of the named repo that wait for the Git host with a
// pull request's address, by ID, with the commits they published.
func (s *Store) AtHost(repo string) (map[int64]string, error) {
	rows, err := s.db.Query(`SELECT cards.id, publishes.commit_id FROM cards JOIN publishes ON publishes.card = cards.id
		WHERE cards.stage = ? AND cards.waits_for = ? AND publishes.address IS NOT NULL AND cards.repo = ?`, Review, Host, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var commit sql.NullString
		if err := rows.Scan(&id, &commit); err != nil {
			return nil, err
		}
		out[id] = commit.String
	}
	return out, rows.Err()
}

// atHost lists the cards of the named repo that wait for the Git host whose
// publication fits the condition, a card without a publication counting as
// one without an address.
func (s *Store) atHost(repo, cond string) ([]int64, error) {
	rows, err := s.db.Query(`SELECT cards.id FROM cards LEFT JOIN publishes ON publishes.card = cards.id
		WHERE cards.stage = ? AND cards.waits_for = ? AND (`+cond+`) AND cards.repo = ? ORDER BY cards.id`, Review, Host, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// KeepPullRequest keeps the commit a card's publishing published and its pull
// request's address, for a card that waits for the Git host without one, with
// an entry of the rule whose reason is PublishedReason and the address; the
// card stays where it is. A card that is no longer there, or that has an
// address already, is refused with ErrRefused and returned as it is.
func (s *Store) KeepPullRequest(card int64, commit, address string) (Card, error) {
	if commit == "" || address == "" {
		return Card{}, errors.New("a pull request needs a commit and an address")
	}
	return s.change(edit{id: card, cause: CauseRule, reason: PublishedReason + address, always: true,
		next: func(from State, f Facts) (State, error) {
			if from != atHost || f.Published {
				return State{}, fmt.Errorf("%w: card %d is in %s waiting for %s, with an address: %v",
					ErrRefused, card, from.Stage, from.WaitsFor, f.Published)
			}
			return atHost, nil
		},
		write: func(tx *sql.Tx, card int64) error {
			_, err := tx.Exec(`INSERT INTO publishes (card, commit_id, address) VALUES (?, ?, ?)
				ON CONFLICT (card) DO UPDATE SET commit_id = excluded.commit_id, address = excluded.address`, card, commit, address)
			return err
		}})
}

// CheckFailedReason begins the reason of the rule's entry that moves a card at
// the Git host to implement because the check failed for its published commit,
// followed by " for " and the commit, the rest of the sentence and, after an
// empty line, the check's output as a fenced block, if it printed any.
const CheckFailedReason = "the check failed on the Git host"

// CheckFailed moves a card that waits for the Git host to the state to, by the
// rule, because the check failed for the commit, with the reason: as
// MoveSentBack, its number of send-backs goes up by one in the same
// transaction, and a move to implement, waiting for a run, at MaxSendBacks or
// more is refused. In the same transaction the card's publication must still
// have the commit and an address; a card that is no longer at the Git host, or
// whose publication has another commit or none, is refused with ErrRefused,
// nothing is written, and the card is returned as it is.
func (s *Store) CheckFailed(card int64, commit string, to State, reason string) (Card, error) {
	if commit == "" || !strings.HasPrefix(reason, CheckFailedReason) {
		return Card{}, errors.New("a failed check needs a commit and a reason of a failed check")
	}
	c, err := s.change(edit{id: card, next: atHost.only(to), cause: CauseRule, reason: reason, sendBack: true,
		write: func(tx *sql.Tx, card int64) error {
			_, err := publishedAs(tx, card, commit)
			return err
		}})
	return s.keepIfRefused(card, c, err)
}

// publishedAs reads a card's publication in a transaction and refuses it with
// ErrRefused unless it has the commit and an address.
func publishedAs(tx *sql.Tx, card int64, commit string) (Publication, error) {
	p, err := readPublication(tx, card)
	if err != nil {
		return Publication{}, err
	}
	if p.Commit != commit || p.Address == "" {
		return Publication{}, fmt.Errorf("%w: card %d is published as %q, not as %s with an address", ErrRefused, card, p.Commit, commit)
	}
	return p, nil
}

// keepIfRefused returns the card as it is now when a change was refused by its
// write, which rolled everything back and returned no card.
func (s *Store) keepIfRefused(card int64, c Card, err error) (Card, error) {
	if errors.Is(err, ErrRefused) && c.ID == 0 {
		if cur, cerr := s.Card(card); cerr == nil {
			c = cur
		}
	}
	return c, err
}

// PullRequestClosed moves a card that waits for the Git host to review,
// waiting for the developer, by the Git host, with the reason ClosedReason,
// because the card's pull request was closed without a merge for the commit.
// It is no send-back: the number of send-backs stays as it is. In the same
// transaction the card's publication must still have the commit and an
// address; a card that is no longer at the Git host, or whose publication has
// another commit or none, is refused with ErrRefused, nothing is written, and
// the card is returned as it is. The publication is not changed: the next
// publish move writes it anew.
func (s *Store) PullRequestClosed(card int64, commit string) (Card, error) {
	if commit == "" {
		return Card{}, errors.New("a closed pull request needs a commit")
	}
	c, err := s.change(edit{id: card, next: atHost.only(State{Review, Developer}), cause: CauseHost, reason: ClosedReason, always: true,
		write: func(tx *sql.Tx, card int64) error {
			_, err := publishedAs(tx, card, commit)
			return err
		}})
	return s.keepIfRefused(card, c, err)
}

// ReviewFeedback moves a card that waits for the Git host to review, waiting
// for the developer, by the Git host, with the reason, which begins with
// ReviewReason, because a reviewer commented on the commit's pull request
// (ADR 0028). It is no send-back: the number of send-backs stays as it is. In
// the same transaction the card's publication must still have the commit and
// an address, and its feedback_since is set to read, when the feedback was
// read; its commit, address, branch and yes stay. A card that is no longer at
// the Git host, or whose publication has another commit or none, is refused
// with ErrRefused, nothing is written, and the card is returned as it is.
func (s *Store) ReviewFeedback(card int64, commit string, read time.Time, reason string) (Card, error) {
	if commit == "" || read.IsZero() || !strings.HasPrefix(reason, ReviewReason) {
		return Card{}, errors.New("a reviewer's feedback needs a commit, the time it was read and a reason of feedback")
	}
	c, err := s.change(edit{id: card, next: atHost.only(State{Review, Developer}), cause: CauseHost, reason: reason, always: true,
		write: func(tx *sql.Tx, card int64) error {
			if _, err := publishedAs(tx, card, commit); err != nil {
				return err
			}
			_, err := tx.Exec(`UPDATE publishes SET feedback_since = ? WHERE card = ?`, read.UTC().Format(time.RFC3339Nano), card)
			return err
		}})
	return s.keepIfRefused(card, c, err)
}

// FailedCheck is FromHost for a failed check only: the card's newest entry of
// the rule whose reason begins with CheckFailedReason, when it is newer than
// every entry that ended a run which wrote a spec or a review.
func (s *Store) FailedCheck(card int64) (Entry, bool, error) {
	e, ok, err := s.FromHost(card)
	if !ok || err != nil || e.Cause != CauseRule {
		return Entry{}, false, err
	}
	return e, true, nil
}

// FromHost finds the card's newest entry that is either the rule's failed
// check, of the cause rule with a reason that begins with CheckFailedReason,
// or the Git host's feedback, of the cause host with a reason that begins
// with ReviewReason, when it is newer than every entry that ended a run which
// wrote a spec or a review: what the card's implement runs are told until a
// newer spec or review exists. A retry, or a run after the developer's again,
// still finds it.
//
// A feedback entry that a later entry of the developer's to-host follows is
// passed over: "Back to the Git host" says the developer answered it, so no
// later implement run is told it (ADR 0028). That entry is the developer's,
// to review, waiting for the Git host, and about no proposal. After feedback
// and before a run writes a spec or a review, only to-host writes such an
// entry: publish from "needs you" follows only a failed publishing or a
// closed pull request, and a move on a proposal is never it. Feedback newer
// than the to-host counts again.
func (s *Store) FromHost(card int64) (Entry, bool, error) {
	e := Entry{Card: card}
	var at string
	var run sql.NullString
	err := s.db.QueryRow(`SELECT number, at, cause, stage, waits_for, run, reason FROM entries AS f
		WHERE card = ? AND ((cause = ? AND substr(reason, 1, ?) = ?)
			OR (cause = ? AND substr(reason, 1, ?) = ? AND NOT EXISTS (SELECT 1 FROM entries AS h
				WHERE h.card = f.card AND h.number > f.number AND h.cause = ? AND h.stage = ? AND h.waits_for = ?
				AND h.proposal IS NULL)))
		AND number > (SELECT COALESCE(MAX(entries.number), 0) FROM entries JOIN specs ON specs.run = entries.run
			WHERE entries.card = ? AND specs.kind IN (?, ?))
		ORDER BY number DESC LIMIT 1`,
		card, CauseRule, len(CheckFailedReason), CheckFailedReason, CauseHost, len(ReviewReason), ReviewReason,
		CauseDeveloper, atHost.Stage, atHost.WaitsFor,
		card, KindSpec, KindReview).
		Scan(&e.Number, &at, &e.Cause, &e.Stage, &e.WaitsFor, &run, &e.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	e.Run = run.String
	if e.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
		return Entry{}, false, fmt.Errorf("entry %d: %w", e.Number, err)
	}
	return e, true, nil
}

// PublishAgain makes a card at the Git host whose published commit no longer
// merges onto main one the core publishes again (Publishing lists it), by the
// rule, with the reason, the card staying where it is and counting no
// send-back: the publisher that publishes it sends it back, when its branch
// collides with main. In the same transaction the card's publication must
// still have the commit and an address; its commit and address are cleared,
// and its branch on the Git host and the developer's yes are kept, so that it
// is published again onto the same pull request. A card that is no longer at
// the Git host, or whose publication has another commit or no address, is
// refused with ErrRefused, nothing is written, and the card is returned as it
// is. No store version is needed: the row is the one a review that passes
// writes.
func (s *Store) PublishAgain(card int64, commit, reason string) (Card, error) {
	if commit == "" || reason == "" {
		return Card{}, errors.New("publishing again needs a commit and a reason")
	}
	c, err := s.change(edit{id: card, next: atHost.only(atHost), cause: CauseRule, reason: reason, always: true,
		write: func(tx *sql.Tx, card int64) error {
			p, err := publishedAs(tx, card, commit)
			if err != nil {
				return err
			}
			return writePublication(tx, card, p.WorkflowYes)
		}})
	return s.keepIfRefused(card, c, err)
}
