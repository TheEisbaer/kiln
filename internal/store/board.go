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
	// KindProposals is proposals.md of a review run, kept as rows of the
	// table proposals, never in specs (ADR 0033).
	KindProposals = "proposals"
)

// kinds are the kinds of documents the store keeps.
var kinds = map[string]bool{KindSpec: true, KindReport: true, KindPullRequest: true, KindReview: true}

// Document is one document of a card: its kind, the run that wrote it, and
// its text, exactly as the run wrote it.
type Document struct {
	Kind string
	Run  string
	Text string
	Note string // the developer's note on approving a spec; "" on every other row
}

// Documents reads a card's documents, newest last, each under its kind: the
// specs, reports and reviews, from the table specs.
func (s *Store) Documents(card int64) ([]Document, error) {
	rows, err := s.db.Query(`SELECT kind, run, text, note FROM specs WHERE card = ? ORDER BY rowid`, card)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []Document
	for rows.Next() {
		var d Document
		var note sql.NullString
		if err := rows.Scan(&d.Kind, &d.Run, &d.Text, &note); err != nil {
			return nil, err
		}
		d.Note = note.String
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// RunAt is a run with the time it was written in flight, which is zero for a
// run written before the store kept it (version 4).
type RunAt struct {
	RunRow
	Started time.Time
	Stage   Stage // the stage its card waited in for it; "" when unknown
	Cost    Cost
}

const runAtSelect = `SELECT id, card, commit_id, model, effort, ending, started, stage, cost, session FROM runs`

func scanRunAt(rows *sql.Rows) (RunAt, error) {
	var r RunAt
	var ending, started, stage, session sql.NullString
	var cost sql.NullFloat64
	if err := rows.Scan(&r.ID, &r.Card, &r.Commit, &r.Model, &r.Effort, &ending, &started, &stage, &cost, &session); err != nil {
		return RunAt{}, err
	}
	r.Ending = Ending(ending.String)
	r.Stage = Stage(stage.String)
	r.Cost.Session = session.String
	if cost.Valid {
		r.Cost.USD = &cost.Float64
	}
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
// in flight, if any, the kinds of the documents it has, its predecessors, and
// those of them that are not done while it is queued for them.
type Summary struct {
	Card
	Reason       string
	Dropped      bool
	Merged       bool
	InFlight     *RunAt
	Kinds        []string
	Cost         *CardCost // nil when no run of the card has ended
	Predecessors []int64
	QueuedFor    []int64
	// Interventions is the card's number of interventions (Interventions).
	Interventions int
}

// Part is what some of a card's runs cost.
type Part struct {
	USD     float64 // the sum of what its runs of known cost added
	Runs    int     // its ended runs
	Unknown int     // of them, those whose cost is unknown
}

// CardCost is what a card cost, in all and per stage.
type CardCost struct{ Total, Spec, Implement, Review Part }

// CostOf sums what a card's runs cost, from its runs in the order they were
// written. A run counts what it added: its reported cost less that of the
// card's latest earlier run of known cost with the same session, since a
// resumed run reports the whole conversation (ADR 0053). A difference below 0
// is unknown. Runs in flight are skipped; it is nil when no run has ended.
func CostOf(runs []RunAt) *CardCost {
	var c CardCost
	last := map[string]float64{} // the latest known reported cost of a session
	for _, r := range runs {
		if r.Ending == "" {
			continue
		}
		var counted *float64
		if r.Cost.USD != nil {
			v := *r.Cost.USD
			if r.Cost.Session != "" {
				if prev, ok := last[r.Cost.Session]; ok {
					v -= prev
				}
				last[r.Cost.Session] = *r.Cost.USD
			}
			if v >= 0 {
				counted = &v
			}
		}
		parts := []*Part{&c.Total}
		switch r.Stage {
		case Spec:
			parts = append(parts, &c.Spec)
		case Implement:
			parts = append(parts, &c.Implement)
		case Review:
			parts = append(parts, &c.Review)
		}
		for _, p := range parts {
			p.Runs++
			if counted == nil {
				p.Unknown++
			} else {
				p.USD += *counted
			}
		}
	}
	if c.Total.Runs == 0 {
		return nil
	}
	return &c
}

// UnreadCosts lists the IDs of the runs whose result.json no core has read.
func (s *Store) UnreadCosts() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM runs WHERE unread = 1 ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FillCosts writes the cost of runs that are still unread and marks them
// read, in one transaction. A run that is no longer unread is left as it is.
func (s *Store) FillCosts(costs map[string]Cost) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, c := range costs {
		var session any
		if c.Session != "" {
			session = c.Session
		}
		if _, err := tx.Exec(`UPDATE runs SET cost = ?, session = ?, unread = 0 WHERE id = ? AND unread = 1`,
			c.USD, session, id); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	rows, err := tx.Query(`SELECT id, text, stage, waits_for, send_backs FROM cards ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c Card
		if err := rows.Scan(&c.ID, &c.Text, &c.Stage, &c.WaitsFor, &c.SendBacks); err != nil {
			rows.Close()
			return nil, err
		}
		at[c.ID] = len(out)
		out = append(out, Summary{Card: c})
	}
	if err := errorsOf(rows); err != nil {
		return nil, err
	}

	// An entry about a proposal changes no card, so it is passed over.
	rows, err = tx.Query(`SELECT card, cause, reason FROM entries
		WHERE number IN (SELECT max(number) FROM entries WHERE proposal IS NULL GROUP BY card)`)
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

	// The entries' causes and states, by number, for the interventions.
	rows, err = tx.Query(`SELECT card, cause, stage, waits_for, proposal FROM entries ORDER BY number`)
	if err != nil {
		return nil, err
	}
	entries := map[int64][]Entry{}
	for rows.Next() {
		var e Entry
		var proposal sql.NullInt64
		if err := rows.Scan(&e.Card, &e.Cause, &e.Stage, &e.WaitsFor, &proposal); err != nil {
			rows.Close()
			return nil, err
		}
		e.Proposal = proposal.Int64
		entries[e.Card] = append(entries[e.Card], e)
	}
	if err := errorsOf(rows); err != nil {
		return nil, err
	}
	for card, es := range entries {
		if i, ok := at[card]; ok {
			out[i].Interventions = Interventions(es)
		}
	}

	rows, err = tx.Query(runAtSelect + ` ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	runs := map[int64][]RunAt{}
	for rows.Next() {
		r, err := scanRunAt(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := at[r.Card]; ok {
			if r.Ending == "" {
				out[i].InFlight = &r
			}
			runs[r.Card] = append(runs[r.Card], r)
		}
	}
	if err := errorsOf(rows); err != nil {
		return nil, err
	}
	for card, rs := range runs {
		out[at[card]].Cost = CostOf(rs)
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

	// A predecessor is open while it is not in done; a queued card waits for
	// its open ones.
	rows, err = tx.Query(`SELECT card, predecessor FROM predecessors ORDER BY card, predecessor`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var card, p int64
		if err := rows.Scan(&card, &p); err != nil {
			rows.Close()
			return nil, err
		}
		i, ok := at[card]
		j, known := at[p]
		if !ok || !known {
			continue
		}
		out[i].Predecessors = append(out[i].Predecessors, p)
		if out[i].State == queued && out[j].Stage != Done {
			out[i].QueuedFor = append(out[i].QueuedFor, p)
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
