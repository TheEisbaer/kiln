package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Proposal is a problem a review run noticed outside its card's change, as
// the store keeps it (ADR 0033): the card it was noticed on, the run that
// wrote it, its place in that run's proposals.md, from 1, its title and text
// as written, and what the developer did with it: the card made from it, 0
// until then, or whether it was dismissed, with the note.
type Proposal struct {
	ID        int64
	Card      int64
	Run       string
	Number    int
	Title     string
	Text      string
	Made      int64
	Dismissed bool
	Note      string
}

// Open says whether the developer has not acted on the proposal yet.
func (p Proposal) Open() bool { return p.Made == 0 && !p.Dismissed }

// ProposalCounts are the proposals over the whole history: those kept, those
// made into a card and those dismissed (ADR 0033, rule 4).
type ProposalCounts struct {
	Made      int
	Accepted  int
	Dismissed int
}

// ErrNoProposal is the error of a proposal that is not in the store.
var ErrNoProposal = errors.New("no proposal")

// The reasons on the entries of the moves on a proposal.
const (
	madeReason      = "made card #%d from proposal %d"
	dismissedReason = "dismissed proposal %d"
)

// writeProposals writes a run's proposals, numbered from 1, with the card and
// the run, in the transaction that ends the run.
func writeProposals(tx *sql.Tx, card int64, run string, ps []Proposal) error {
	for i, p := range ps {
		if _, err := tx.Exec(`INSERT INTO proposals (card, run, number, title, text) VALUES (?, ?, ?, ?, ?)`,
			card, run, i+1, p.Title, p.Text); err != nil {
			return fmt.Errorf("proposal %d of run %s: %w", i+1, run, err)
		}
	}
	return nil
}

const proposalSelect = `SELECT id, card, run, number, title, text, made, dismissed FROM proposals`

// scanner is a row or rows.
type scanner interface{ Scan(dest ...any) error }

func scanProposal(r scanner) (Proposal, error) {
	var p Proposal
	var made sql.NullInt64
	var dismissed sql.NullString
	err := r.Scan(&p.ID, &p.Card, &p.Run, &p.Number, &p.Title, &p.Text, &made, &dismissed)
	p.Made, p.Dismissed, p.Note = made.Int64, dismissed.Valid, dismissed.String
	return p, err
}

func readProposal(q querier, id int64) (Proposal, error) {
	p, err := scanProposal(q.QueryRow(proposalSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, fmt.Errorf("%w %d", ErrNoProposal, id)
	}
	return p, err
}

// openProposal reads a proposal in the transaction of a move on it, and
// refuses one the developer has acted on already with ErrRefused.
func openProposal(tx *sql.Tx, id int64) (Proposal, error) {
	p, err := readProposal(tx, id)
	if err != nil {
		return Proposal{}, err
	}
	switch {
	case p.Made != 0:
		return p, fmt.Errorf("%w: proposal %d was made into card #%d already", ErrRefused, id, p.Made)
	case p.Dismissed:
		return p, fmt.Errorf("%w: proposal %d was dismissed already", ErrRefused, id)
	}
	return p, nil
}

// writeProposalEntry writes the entry of a move on a proposal on the card it
// was noticed on: the card's state as it is, the cause developer, no run, the
// reason and the proposal. It writes neither the card's row nor since, so it
// changes no card, and every reader of a card's last entry passes over it.
func writeProposalEntry(tx *sql.Tx, p Proposal, reason string) error {
	c, err := readCard(tx, p.Card)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO entries (card, at, cause, stage, waits_for, run, reason, proposal) VALUES (?, ?, ?, ?, ?, NULL, ?, ?)`,
		c.ID, time.Now().UTC().Format(time.RFC3339Nano), CauseDeveloper, c.Stage, c.WaitsFor, reason, p.ID)
	return err
}

// MakeCard is the developer's move "Make a card" on an open proposal: in one
// transaction of change it writes a card in the backlog, waiting for the
// developer, not started, whose text is the proposal's title, a blank line
// and its text, or the one of them that is not empty; it keeps the card's ID
// on the proposal, and writes an entry about the proposal on the card it was
// noticed on. A proposal that is not open is refused with ErrRefused and
// nothing is written; an unknown one is ErrNoProposal.
func (s *Store) MakeCard(id int64) (Card, Proposal, error) {
	p, err := readProposal(s.db, id)
	if err != nil {
		return Card{}, Proposal{}, err
	}
	text := strings.Join(nonEmpty(p.Title, p.Text), "\n\n")
	if text == "" {
		return Card{}, p, errors.New("a card needs a text, and the proposal has neither a title nor a text")
	}
	// The new card is in the repo of the card the proposal was noticed on. A
	// card's repo never changes, so reading it here gives the name the
	// transaction would read.
	on, err := readCard(s.db, p.Card)
	if err != nil {
		return Card{}, p, err
	}
	c, err := s.change(edit{create: true, repo: on.Repo, text: text, next: fixed(State{Backlog, Developer}), cause: CauseDeveloper,
		write: func(tx *sql.Tx, card int64) error {
			p, err := openProposal(tx, id)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE proposals SET made = ? WHERE id = ?`, card, id); err != nil {
				return err
			}
			return writeProposalEntry(tx, p, fmt.Sprintf(madeReason, card, id))
		}})
	if err != nil {
		p, _ = readProposal(s.db, id)
		return Card{}, p, err
	}
	p, err = readProposal(s.db, id)
	return c, p, err
}

// nonEmpty are the strings that are not empty, in order.
func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Dismiss is the developer's move "Dismiss" on an open proposal, with a note
// that may be empty: in one transaction it keeps the note, trimmed, on the
// proposal, and writes an entry about the proposal on the card it was noticed
// on, whose reason is "dismissed proposal <id>", followed by ": <note>" when
// there is one. A note over MaxNote is refused with ErrNote; a proposal that
// is not open with ErrRefused, and nothing is written; an unknown one is
// ErrNoProposal.
func (s *Store) Dismiss(id int64, note string) (Proposal, error) {
	note = strings.TrimSpace(note)
	if len(note) > MaxNote {
		p, err := readProposal(s.db, id)
		if err != nil {
			return Proposal{}, err
		}
		return p, fmt.Errorf("%w: the note is more than 16 KiB", ErrNote)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Proposal{}, err
	}
	defer tx.Rollback()
	p, err := openProposal(tx, id)
	if err != nil {
		return p, err
	}
	if _, err := tx.Exec(`UPDATE proposals SET dismissed = ? WHERE id = ?`, note, id); err != nil {
		return Proposal{}, err
	}
	reason := fmt.Sprintf(dismissedReason, id)
	if note != "" {
		reason += ": " + note
	}
	if err := writeProposalEntry(tx, p, reason); err != nil {
		return Proposal{}, err
	}
	if err := tx.Commit(); err != nil {
		return Proposal{}, err
	}
	p.Dismissed, p.Note = true, note
	return p, nil
}

// Proposal reads a proposal.
func (s *Store) Proposal(id int64) (Proposal, error) {
	return readProposal(s.db, id)
}

// OpenProposals reads, in one transaction, the open proposals, by ID, and the
// counts over the whole history.
func (s *Store) OpenProposals() ([]Proposal, ProposalCounts, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, ProposalCounts{}, err
	}
	defer tx.Rollback()
	var n ProposalCounts
	if err := tx.QueryRow(`SELECT count(*), count(made), count(dismissed) FROM proposals`).
		Scan(&n.Made, &n.Accepted, &n.Dismissed); err != nil {
		return nil, ProposalCounts{}, err
	}
	rows, err := tx.Query(proposalSelect + ` WHERE made IS NULL AND dismissed IS NULL ORDER BY id`)
	if err != nil {
		return nil, ProposalCounts{}, err
	}
	var open []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			rows.Close()
			return nil, ProposalCounts{}, err
		}
		open = append(open, p)
	}
	if err := errorsOf(rows); err != nil {
		return nil, ProposalCounts{}, err
	}
	return open, n, tx.Commit()
}

// ProposalsOn reads every proposal noticed on a card, open or acted on, oldest
// first: the order they were written in, by ID. A card with none gives an
// empty list.
func (s *Store) ProposalsOn(card int64) ([]Proposal, error) {
	rows, err := s.db.Query(proposalSelect+` WHERE card = ? ORDER BY id`, card)
	if err != nil {
		return nil, err
	}
	var list []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, p)
	}
	if err := errorsOf(rows); err != nil {
		return nil, err
	}
	return list, nil
}

// ProposalOf reads the proposal a card was made from, or nil when it was not
// made from one.
func (s *Store) ProposalOf(card int64) (*Proposal, error) {
	p, err := scanProposal(s.db.QueryRow(proposalSelect+` WHERE made = ?`, card))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// proposalNote is the developer's note on an entry about a proposal: what
// follows the words of a dismiss, "" on every other.
func (e Entry) proposalNote() string {
	rest, ok := strings.CutPrefix(e.Reason, fmt.Sprintf(dismissedReason, e.Proposal))
	if !ok {
		return ""
	}
	return strings.TrimPrefix(rest, ": ")
}
