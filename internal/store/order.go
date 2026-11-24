package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The fixed words of the reasons of a card's wait for its predecessors (ADR
// 0010). Each is followed by the cards it names, as "#3, #5".
const (
	// QueuedPrefix begins the reason of the developer's entry that queues a
	// card, followed by its predecessors that are not done.
	QueuedPrefix = "waits for its predecessors to be done: "
	// ReleasedPrefix begins the reason of the rule's entry that starts a
	// queued card, followed by all its predecessors.
	ReleasedPrefix = "its predecessors are done: "
	// PredecessorDroppedPrefix begins the reason of the rule's entry that
	// leaves a queued card waiting for the developer because a predecessor
	// was dropped. In the backlog, waiting for the developer, no other
	// reason begins with it.
	PredecessorDroppedPrefix = "its predecessor"
)

// queued is where a card waits for its predecessors: in the backlog, waiting
// for room.
var queued = State{Backlog, Room}

// ErrPredecessor is the error of a predecessor that a new card cannot name:
// itself, a card that does not exist, or a card that was dropped. The error
// Create returns is the reason alone, and errors.Is finds ErrPredecessor in it.
var ErrPredecessor = errors.New("the card cannot name that predecessor")

// predecessorError is a refused predecessor, with its reason.
type predecessorError string

func (e predecessorError) Error() string        { return string(e) }
func (e predecessorError) Is(target error) bool { return target == ErrPredecessor }

// cards writes card IDs as the reasons name them: "#3, #5".
func cards(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("#%d", id)
	}
	return strings.Join(parts, ", ")
}

// writePredecessors writes a new card's predecessors, in the transaction that
// writes the card, after its row. A predecessor that is the card, that does
// not exist or that was dropped is refused with ErrPredecessor, and the
// transaction is then rolled back. One named twice is written once.
func writePredecessors(tx *sql.Tx, card int64, predecessors []int64) error {
	for _, p := range predecessors {
		if p == card {
			return predecessorError("a card cannot be its own predecessor")
		}
		c, err := readCard(tx, p)
		if errors.Is(err, ErrNoCard) {
			return predecessorError(fmt.Sprintf("no card %d to wait for", p))
		}
		if err != nil {
			return err
		}
		if c.State == dropped {
			e, err := lastEntry(tx, p)
			if err != nil {
				return err
			}
			if Dropped(e.State, e.Cause, e.Reason) {
				return predecessorError(fmt.Sprintf("card %d was dropped, so no card can wait for it", p))
			}
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO predecessors (card, predecessor) VALUES (?, ?)`, card, p); err != nil {
			return err
		}
	}
	return nil
}

// readPredecessors reads a card's predecessors, in ID order: all of them,
// those whose stage is not done, and those that were dropped. A predecessor is
// done when it is in done and was not dropped, a card closed without
// publishing included; a dropped one is read from its last entry, as the board
// reads it (ADR 0043).
func readPredecessors(q querier, card int64) (all, open, gone []int64, err error) {
	rows, err := q.Query(`SELECT p.predecessor, c.stage, c.waits_for FROM predecessors p
		JOIN cards c ON c.id = p.predecessor WHERE p.card = ? ORDER BY p.predecessor`, card)
	if err != nil {
		return nil, nil, nil, err
	}
	var done []int64
	for rows.Next() {
		var id int64
		var st State
		if err := rows.Scan(&id, &st.Stage, &st.WaitsFor); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		all = append(all, id)
		switch {
		case st.Stage != Done:
			open = append(open, id)
		case st == dropped:
			done = append(done, id)
		}
	}
	// The rows are closed before the last entries are read: the store has one
	// connection.
	if err := errorsOf(rows); err != nil {
		return nil, nil, nil, err
	}
	for _, id := range done {
		e, err := lastEntry(q, id)
		if err != nil {
			return nil, nil, nil, err
		}
		if Dropped(e.State, e.Cause, e.Reason) {
			gone = append(gone, id)
		}
	}
	return all, open, gone, nil
}

// queueing is the state a start goes to, from the state the table gives:
// queued while a predecessor is not done.
func queueing(to State, f Facts) State {
	if len(f.Open) > 0 {
		return queued
	}
	return to
}

// queuedReason is the reason of a start's entry: the predecessors it waits
// for, when it is queued, and none when it goes to spec.
func queuedReason(to State, f Facts) string {
	if to == queued {
		return QueuedPrefix + cards(f.Open)
	}
	return ""
}

// Start starts a card that waits for the developer in the backlog, as the
// developer's move start: to spec, waiting for a run, or queued, in the
// backlog waiting for room, while a predecessor is not done (ADR 0010).
func (s *Store) Start(id int64) (Card, error) {
	return s.Decide(id, Start, "")
}

// Released is a card that Release moved, with the reason of its entry.
type Released struct {
	Card
	Reason string
}

// errStays is the error of a release whose card is no longer queued.
var errStays = errors.New("the card is no longer queued")

// Release is the rule that ends a queued card's wait (ADR 0010, ADR 0035). It
// takes each card in the backlog, waiting for room, that has a predecessor,
// and in a transaction of its own, made only from that state, reads its
// predecessors. While one is not done, the card stays and nothing is written.
// When every one is done, the card goes to spec, waiting for a run; when none
// is open and one was dropped, it waits for the developer in the backlog, to
// be started without it or dropped. A card that is no longer queued is passed
// over. It returns the cards it moved.
func (s *Store) Release() ([]Released, error) {
	rows, err := s.db.Query(`SELECT id FROM cards WHERE stage = ? AND waits_for = ?
		AND EXISTS (SELECT 1 FROM predecessors WHERE card = cards.id) ORDER BY id`, queued.Stage, queued.WaitsFor)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := errorsOf(rows); err != nil {
		return nil, err
	}
	var out []Released
	for _, id := range ids {
		var reason string
		e := edit{id: id, cause: CauseRule}
		e.next = func(from State, f Facts) (State, error) {
			switch {
			case from != queued:
				return State{}, errStays
			case len(f.Open) > 0:
				return from, nil // writes nothing
			case len(f.Dropped) > 0:
				return State{Backlog, Developer}, nil
			}
			return State{Spec, Run}, nil
		}
		e.because = func(to State, f Facts) string {
			switch {
			case to == queued:
				return ""
			case len(f.Dropped) == 1:
				reason = fmt.Sprintf("%s %s was dropped; start it without it, or drop it", PredecessorDroppedPrefix, cards(f.Dropped))
			case len(f.Dropped) > 1:
				reason = fmt.Sprintf("%ss %s were dropped; start it without them, or drop them", PredecessorDroppedPrefix, cards(f.Dropped))
			default:
				reason = ReleasedPrefix + cards(f.all)
			}
			return reason
		}
		c, err := s.change(e)
		switch {
		case errors.Is(err, errStays):
			continue
		case err != nil:
			return out, fmt.Errorf("card %d: %w", id, err)
		case c.State != queued:
			out = append(out, Released{c, reason})
		}
	}
	return out, nil
}

// Order is a card's place among its predecessors: the cards it names, those
// of them it waits for while it is queued, and the cards that name it and are
// not in done.
type Order struct {
	Predecessors []int64
	QueuedFor    []int64
	NamedBy      []Card
}

// Order reads a card's Order, in one transaction.
func (s *Store) Order(card int64) (Order, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Order{}, err
	}
	defer tx.Rollback()
	c, err := readCard(tx, card)
	if err != nil {
		return Order{}, err
	}
	var o Order
	var open []int64
	if o.Predecessors, open, _, err = readPredecessors(tx, card); err != nil {
		return Order{}, err
	}
	if c.State == queued {
		o.QueuedFor = open
	}
	rows, err := tx.Query(`SELECT c.id, c.text, c.stage, c.waits_for, c.send_backs FROM predecessors p
		JOIN cards c ON c.id = p.card WHERE p.predecessor = ? AND c.stage != ? ORDER BY c.id`, card, Done)
	if err != nil {
		return Order{}, err
	}
	for rows.Next() {
		var n Card
		if err := rows.Scan(&n.ID, &n.Text, &n.Stage, &n.WaitsFor, &n.SendBacks); err != nil {
			rows.Close()
			return Order{}, err
		}
		o.NamedBy = append(o.NamedBy, n)
	}
	if err := errorsOf(rows); err != nil {
		return Order{}, err
	}
	return o, tx.Commit()
}
