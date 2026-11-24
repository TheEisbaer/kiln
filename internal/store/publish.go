package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Publication is what the store keeps of a card's publishing (card 19): the
// commit of the card's branch whose workflow files the developer said yes to
// with the move publish, if any; once the core has published it, the commit it
// published and the pull request's address; and the branch on the Git host it
// is published on, named at its first publishing and kept for every later
// one, so that one card has one pull request. A card never published has none
// of them.
type Publication struct {
	WorkflowYes string
	Commit      string
	Address     string
	Branch      string
}

// The reasons on the entries of publishing that the store tells apart.
const (
	// PublishFailedReason begins the reason of a card that publishing moved
	// to "needs you", from which the developer can publish again.
	PublishFailedReason = "publishing failed"
	// MergedReason is the reason of a card the Git host merged.
	MergedReason = "merged"
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
	return f, nil
}

// Publication reads a card's publication; a card never published has an
// empty one.
func (s *Store) Publication(card int64) (Publication, error) {
	return readPublication(s.db, card)
}

func readPublication(q querier, card int64) (Publication, error) {
	var yes, commit, address, branch sql.NullString
	err := q.QueryRow(`SELECT workflow_yes, commit_id, address, branch FROM publishes WHERE card = ?`, card).
		Scan(&yes, &commit, &address, &branch)
	if errors.Is(err, sql.ErrNoRows) {
		return Publication{}, nil
	}
	return Publication{WorkflowYes: yes.String, Commit: commit.String, Address: address.String, Branch: branch.String}, err
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

// Publishing lists the cards that wait for the Git host and have no pull
// request's address yet, which the core publishes, by ID.
func (s *Store) Publishing() ([]int64, error) {
	return s.atHost(`address IS NULL`)
}

// AtHost lists the cards that wait for the Git host with a pull request's
// address, by ID, with the commits they published.
func (s *Store) AtHost() (map[int64]string, error) {
	rows, err := s.db.Query(`SELECT cards.id, publishes.commit_id FROM cards JOIN publishes ON publishes.card = cards.id
		WHERE cards.stage = ? AND cards.waits_for = ? AND publishes.address IS NOT NULL`, Review, Host)
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

// atHost lists the cards that wait for the Git host whose publication fits
// the condition, a card without a publication counting as one without an
// address.
func (s *Store) atHost(cond string) ([]int64, error) {
	rows, err := s.db.Query(`SELECT cards.id FROM cards LEFT JOIN publishes ON publishes.card = cards.id
		WHERE cards.stage = ? AND cards.waits_for = ? AND (`+cond+`) ORDER BY cards.id`, Review, Host)
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
			p, err := readPublication(tx, card)
			if err != nil {
				return err
			}
			if p.Commit != commit || p.Address == "" {
				return fmt.Errorf("%w: card %d is published as %q, not as %s with an address", ErrRefused, card, p.Commit, commit)
			}
			return nil
		}})
	if errors.Is(err, ErrRefused) && c.ID == 0 {
		// The refusal came from the write, which rolled everything back.
		if cur, cerr := s.Card(card); cerr == nil {
			c = cur
		}
	}
	return c, err
}

// FailedCheck finds the card's newest entry of the rule whose reason begins
// with CheckFailedReason, when it is newer than every entry that ended a run
// which wrote a spec or a review: the failed check that the card's implement
// runs are told until a newer spec or review exists. A retry, or a run after
// the developer's again, still finds it.
func (s *Store) FailedCheck(card int64) (Entry, bool, error) {
	e := Entry{Card: card}
	var at string
	var run sql.NullString
	err := s.db.QueryRow(`SELECT number, at, cause, stage, waits_for, run, reason FROM entries
		WHERE card = ? AND cause = ? AND substr(reason, 1, ?) = ?
		AND number > (SELECT COALESCE(MAX(entries.number), 0) FROM entries JOIN specs ON specs.run = entries.run
			WHERE entries.card = ? AND specs.kind IN (?, ?))
		ORDER BY number DESC LIMIT 1`,
		card, CauseRule, len(CheckFailedReason), CheckFailedReason, card, KindSpec, KindReview).
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
			p, err := readPublication(tx, card)
			if err != nil {
				return err
			}
			if p.Commit != commit || p.Address == "" {
				return fmt.Errorf("%w: card %d is published as %q, not as %s with an address", ErrRefused, card, p.Commit, commit)
			}
			return writePublication(tx, card, p.WorkflowYes)
		}})
	if errors.Is(err, ErrRefused) && c.ID == 0 {
		// The refusal came from the write, which rolled everything back.
		if cur, cerr := s.Card(card); cerr == nil {
			c = cur
		}
	}
	return c, err
}
