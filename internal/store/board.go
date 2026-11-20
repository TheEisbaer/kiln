package store

import (
	"database/sql"
	"fmt"
	"time"
)

// The kinds of a card's documents, one per stage that runs: the file a run of
// the stage writes to /kiln/out (ADR 0038).
const (
	KindSpec        = "spec"         // spec.md of a spec run
	KindReport      = "report"       // report.md of an implement run
	KindPullRequest = "pull-request" // pull-request.md of an implement run, the pull request's text (card 19)
	KindReview      = "review"       // review.md of a review run
)

// kinds are the kinds of documents the store keeps.
var kinds = map[string]bool{KindSpec: true, KindReport: true, KindPullRequest: true, KindReview: true}

// Document is one document of a card: its kind, the run that wrote it, and
// its text, exactly as the run wrote it.
type Document struct {
	Kind string
	Run  string
	Text string
}

// Documents reads a card's documents, newest last, each under its kind: the
// specs, reports and reviews, from the table specs.
func (s *Store) Documents(card int64) ([]Document, error) {
	rows, err := s.db.Query(`SELECT kind, run, text FROM specs WHERE card = ? ORDER BY rowid`, card)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []Document
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.Kind, &d.Run, &d.Text); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// RunAt is a run with the time it was written in flight, which is zero for a
// run written before the store kept it (version 4).
type RunAt struct {
	RunRow
	Started time.Time
}

const runAtSelect = `SELECT id, card, commit_id, model, effort, ending, started FROM runs`

func scanRunAt(rows *sql.Rows) (RunAt, error) {
	var r RunAt
	var ending, started sql.NullString
	if err := rows.Scan(&r.ID, &r.Card, &r.Commit, &r.Model, &r.Effort, &ending, &started); err != nil {
		return RunAt{}, err
	}
	r.Ending = Ending(ending.String)
	if started.Valid {
		t, err := time.Parse(time.RFC3339Nano, started.String)
		if err != nil {
			return RunAt{}, fmt.Errorf("run %s: %w", r.ID, err)
		}
		r.Started = t
	}
	return r, nil
}

// Runs reads a card's runs, in the order they were written.
func (s *Store) Runs(card int64) ([]RunAt, error) {
	rows, err := s.db.Query(runAtSelect+` WHERE card = ? ORDER BY rowid`, card)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []RunAt
	for rows.Next() {
		r, err := scanRunAt(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// Summary is what the board shows of a card: the card, the reason of its last
// entry and whether that entry dropped it or the Git host merged it, its run
// in flight, if any, and the kinds of the documents it has.
type Summary struct {
	Card
	Reason   string
	Dropped  bool
	Merged   bool
	InFlight *RunAt
	Kinds    []string
}

// Board reads every card's summary, by ID, in one transaction, so that the
// summaries agree with each other.
func (s *Store) Board() ([]Summary, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var out []Summary
	at := map[int64]int{} // a card's index in out
	rows, err := tx.Query(`SELECT id, text, stage, waits_for FROM cards ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c Card
		if err := rows.Scan(&c.ID, &c.Text, &c.Stage, &c.WaitsFor); err != nil {
			rows.Close()
			return nil, err
		}
		at[c.ID] = len(out)
		out = append(out, Summary{Card: c})
	}
	if err := errorsOf(rows); err != nil {
		return nil, err
	}

	rows, err = tx.Query(`SELECT card, cause, reason FROM entries
		WHERE number IN (SELECT max(number) FROM entries GROUP BY card)`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var card int64
		var cause Cause
		var reason sql.NullString
		if err := rows.Scan(&card, &cause, &reason); err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := at[card]; ok {
			out[i].Reason = reason.String
			out[i].Dropped = Dropped(out[i].State, cause, reason.String)
			out[i].Merged = Merged(out[i].State, cause, reason.String)
		}
	}
	if err := errorsOf(rows); err != nil {
		return nil, err
	}

	rows, err = tx.Query(runAtSelect + ` WHERE ending IS NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		r, err := scanRunAt(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := at[r.Card]; ok {
			out[i].InFlight = &r
		}
	}
	if err := errorsOf(rows); err != nil {
		return nil, err
	}

	// Each kind once, in the order of its first document.
	rows, err = tx.Query(`SELECT card, kind FROM specs GROUP BY card, kind ORDER BY min(rowid)`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var card int64
		var kind string
		if err := rows.Scan(&card, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := at[card]; ok {
			out[i].Kinds = append(out[i].Kinds, kind)
		}
	}
	if err := errorsOf(rows); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// errorsOf closes rows that have been read to their end and returns their
// error.
func errorsOf(rows *sql.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}
