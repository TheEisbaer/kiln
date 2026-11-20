package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
	err := q.QueryRow(`SELECT cause, stage, waits_for, reason FROM entries WHERE card = ? ORDER BY number DESC LIMIT 1`, card).
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
