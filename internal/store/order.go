package store

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
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
	// RoomPrefix begins the reason of the entry that queues a card for room
	// under a limit of cards in progress (ADR 0014): its repo's, followed by
	// the repo's name (see roomReason), or the instance's (see
	// instanceReason). A queued card waits for room when the reason of its
	// last entry begins with it, and for its predecessors otherwise. The
	// entries written before the instance's limit, which begin "waits for room
	// in its repo ", begin with it too.
	RoomPrefix = "waits for room in "
	// repoRoomPrefix begins the reason of a card queued for its repo's limit.
	repoRoomPrefix = RoomPrefix + "its repo "
	// instanceRoomPrefix begins the reason of a card queued for the
	// instance's limit.
	instanceRoomPrefix = RoomPrefix + "the instance"
)

// queued is where a card waits for its predecessors, or for room under a
// limit: in the backlog, waiting for room.
var queued = State{Backlog, Room}

// DefaultLimit is a repo's limit of cards in progress when the settings give
// none, and the instance's when they give none (ADR 0014).
const DefaultLimit = 3

// Limits are the limits of cards in progress (ADR 0014): each repo's, by the
// repo's name, and the instance's. The zero Limits has the defaults, and is
// never "no limit".
type Limits struct {
	Repos    map[string]int
	Instance int
}

// Of is the limit of the repo: its value when that is 1 or more, and
// DefaultLimit otherwise, for a nil map too, so that leaving the limits out
// is never "no limit".
func (l Limits) Of(repo string) int {
	if n := l.Repos[repo]; n >= 1 {
		return n
	}
	return DefaultLimit
}

// OfInstance is the instance's limit: Instance when it is 1 or more, and
// DefaultLimit otherwise.
func (l Limits) OfInstance() int {
	if l.Instance >= 1 {
		return l.Instance
	}
	return DefaultLimit
}

// roomReason is the reason of an entry that queues a card for its repo's
// limit.
func roomReason(repo string, l Limits) string {
	return fmt.Sprintf("%s%s, whose limit is %d cards in progress", repoRoomPrefix, repo, l.Of(repo))
}

// instanceReason is the reason of an entry that queues a card for the
// instance's limit.
func instanceReason(l Limits) string {
	return fmt.Sprintf("%s, whose limit is %d cards in progress that are not ready to merge", instanceRoomPrefix, l.OfInstance())
}

// roomFreedReason is the reason of the rule's entry that starts a card that
// waited for room: the limit it last waited for has room.
func roomFreedReason(f Facts, l Limits) string {
	if strings.HasPrefix(f.last, instanceRoomPrefix) {
		return fmt.Sprintf("the instance has room under its limit of %d", l.OfInstance())
	}
	return fmt.Sprintf("its repo %s has room under its limit of %d", f.repo, l.Of(f.repo))
}

// hold is the reason of the limit that holds the card, "" when it fits. It
// goes through the cards that wait for room ahead of it, from every repo, in
// order: one takes a place in its repo and in the instance only when both
// have room for it then, so that a card held by its own repo's limit holds
// back no card of another repo. The card then fits when its repo and the
// instance both have room. While its repo has none, the repo is named, and
// the instance otherwise.
func (f Facts) hold(l Limits) string {
	counts := make(map[string]int, len(f.counts)+1)
	for r, n := range f.counts {
		counts[r] = n
	}
	counts[f.repo] = f.InProgress
	instance := f.InInstance
	for _, a := range f.ahead {
		if counts[a] < l.Of(a) && instance < l.OfInstance() {
			counts[a]++
			instance++
		}
	}
	switch {
	case counts[f.repo] >= l.Of(f.repo):
		return roomReason(f.repo, l)
	case instance >= l.OfInstance():
		return instanceReason(l)
	}
	return ""
}

// inProgress is the SQL condition of a card in progress for its repo's limit:
// in spec, implement or review, whatever it waits for, ready to merge
// included.
const inProgress = `stage IN ('spec','implement','review')`

// forInstance is the SQL condition of a card that counts against the
// instance's limit: in progress and not ready to merge (see
// CountsForInstance).
const forInstance = inProgress + ` AND NOT (stage = 'review' AND waits_for = 'host')`

// CountsForInstance reports whether a card in the state counts against the
// instance's limit (ADR 0014): in spec, implement or review, whatever it
// waits for, except ready to merge, review waiting for the Git host. It is
// the Go form of forInstance.
func CountsForInstance(st State) bool {
	switch st.Stage {
	case Spec, Implement, Review:
		return st != atHost
	}
	return false
}

// readRoom reads, into f, the facts of the room of a card in the backlog,
// whose last entry, apart from those about proposals, has the reason last:
// its repo's cards in progress; the instance's cards that count against its
// limit; whether the card is queued and waits for room; the other queued
// cards that wait for room before it, from every repo, in the queue's order,
// each by its repo; and the cards in progress of those repos. For a card that
// does not wait for room, every such card is before it, since a card joins
// the queue for room at its end; for one that does, those whose since and ID
// are earlier. The times are compared parsed, never as strings. A card in
// another stage gets only its repo.
func readRoom(q querier, card int64, last string, f *Facts) error {
	var st State
	var since string
	err := q.QueryRow(`SELECT repo, stage, waits_for, since FROM cards WHERE id = ?`, card).Scan(&f.repo, &st.Stage, &st.WaitsFor, &since)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	case st.Stage != Backlog:
		return nil
	}
	f.last = last
	if err := q.QueryRow(`SELECT COUNT(*) FROM cards WHERE repo = ? AND `+inProgress, f.repo).Scan(&f.InProgress); err != nil {
		return err
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM cards WHERE ` + forInstance).Scan(&f.InInstance); err != nil {
		return err
	}
	f.ForRoom = st == queued && strings.HasPrefix(last, RoomPrefix)
	var at time.Time
	if f.ForRoom {
		if at, err = time.Parse(time.RFC3339Nano, since); err != nil {
			return fmt.Errorf("card %d: %w", card, err)
		}
	}
	others, err := waitingForRoom(q, card)
	if err != nil {
		return err
	}
	slices.SortFunc(others, func(a, b queuedCard) int {
		return cmp.Or(a.since.Compare(b.since), cmp.Compare(a.id, b.id))
	})
	f.counts = map[string]int{}
	for _, o := range others {
		if f.ForRoom && !o.before(queuedCard{id: card, since: at}) {
			continue
		}
		f.ahead = append(f.ahead, o.repo)
		if _, ok := f.counts[o.repo]; ok || o.repo == f.repo {
			continue
		}
		var n int
		if err := q.QueryRow(`SELECT COUNT(*) FROM cards WHERE repo = ? AND `+inProgress, o.repo).Scan(&n); err != nil {
			return err
		}
		f.counts[o.repo] = n
	}
	return nil
}

// queuedCard is a queued card, with the time it got that state and its repo.
type queuedCard struct {
	id    int64
	since time.Time
	repo  string
}

// before reports whether a is before b in the queue: by since, then by ID.
func (a queuedCard) before(b queuedCard) bool {
	return cmp.Or(a.since.Compare(b.since), cmp.Compare(a.id, b.id)) < 0
}

// waitingForRoom reads the cards of every repo, but the card not, that are
// queued and wait for room, as their last entries, apart from those about
// proposals, say. A card whose since cannot be read takes no place in the queue.
func waitingForRoom(q querier, not int64) ([]queuedCard, error) {
	rows, err := q.Query(`SELECT c.id, c.since, c.repo, COALESCE((SELECT e.reason FROM entries e
		WHERE e.card = c.id AND e.proposal IS NULL ORDER BY e.number DESC LIMIT 1), '')
		FROM cards c WHERE c.stage = ? AND c.waits_for = ? AND c.id != ?`, queued.Stage, queued.WaitsFor, not)
	if err != nil {
		return nil, err
	}
	var out []queuedCard
	for rows.Next() {
		var c queuedCard
		var since, reason string
		if err := rows.Scan(&c.id, &since, &c.repo, &reason); err != nil {
			rows.Close()
			return nil, err
		}
		if !strings.HasPrefix(reason, RoomPrefix) {
			continue
		}
		if c.since, err = time.Parse(time.RFC3339Nano, since); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, errorsOf(rows)
}

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
// queued while a predecessor is not done, or while the card's repo or the
// instance has no room under the limits.
func queueing(to State, f Facts, l Limits) State {
	if len(f.Open) > 0 || f.hold(l) != "" {
		return queued
	}
	return to
}

// queuedReason is the reason of a start's entry: the predecessors it waits
// for, or the limit that holds it, when it is queued, and none when it goes to
// spec.
func queuedReason(to State, f Facts, l Limits) string {
	switch {
	case to != queued:
		return ""
	case len(f.Open) > 0:
		return QueuedPrefix + cards(f.Open)
	}
	return f.hold(l)
}

// Start starts a card that waits for the developer in the backlog, as the
// developer's move start, under the default limits (see StartWith).
func (s *Store) Start(id int64) (Card, error) {
	return s.StartWith(id, Limits{})
}

// StartWith starts a card that waits for the developer in the backlog, as the
// developer's move start: to spec, waiting for a run, or queued, in the
// backlog waiting for room, while a predecessor is not done (ADR 0010) or
// while its repo or the instance has no room under the limits (ADR 0014).
func (s *Store) StartWith(id int64, l Limits) (Card, error) {
	return s.DecideWith(id, Start, "", Options{Limits: l})
}

// Released is a card that Release wrote an entry for, with the reason of its
// entry.
type Released struct {
	Card
	Reason string
}

// errStays is the error of a release that writes nothing: the card is no
// longer queued, or it waits on.
var errStays = errors.New("the card stays as it is")

// Release is the rule that ends a queued card's wait (ADR 0010, ADR 0014, ADR
// 0035). It takes every card in the backlog, waiting for room, in the order of
// its since and then its ID, and in a transaction of its own, made only from
// that state, reads its facts. While a predecessor is not done, the card
// stays and nothing is written. A card that waits for its predecessors, none
// open and one dropped, waits for the developer in the backlog, to be started
// without it or dropped. Otherwise, when its repo and the instance have room
// under the limits, it goes to spec, waiting for a run, with the reason of the
// limit it last waited for. When one has none, a card that waited for its
// predecessors gets one entry that keeps it queued for room, so that it joins
// the end of the queue for room. A card that already waits for room stays
// and nothing is written while its last reason begins with the reason of the
// limit that holds it now; otherwise it gets one entry with that reason, and
// its since is kept, so that it keeps its place in the queue. A card that is
// no longer queued is passed over. It returns the cards it wrote an entry for
// and, joined, one error per card it could not take, each with the card's ID:
// a card whose since cannot be read, or whose transaction failed. The rule
// goes on with the next card, and a card it could not take is tried again on
// the next call. An error of the query or of reading the rows ends it at once.
func (s *Store) Release(l Limits) ([]Released, error) {
	rows, err := s.db.Query(`SELECT id, since FROM cards WHERE stage = ? AND waits_for = ?`, queued.Stage, queued.WaitsFor)
	if err != nil {
		return nil, err
	}
	var queue []queuedCard
	var errs []error
	for rows.Next() {
		var c queuedCard
		var since string
		if err := rows.Scan(&c.id, &since); err != nil {
			rows.Close()
			return nil, err
		}
		if c.since, err = time.Parse(time.RFC3339Nano, since); err != nil {
			errs = append(errs, fmt.Errorf("card %d: %w", c.id, err))
			continue
		}
		queue = append(queue, c)
	}
	if err := errorsOf(rows); err != nil {
		return nil, err
	}
	slices.SortFunc(queue, func(a, b queuedCard) int {
		return cmp.Or(a.since.Compare(b.since), cmp.Compare(a.id, b.id))
	})
	var out []Released
	for _, q := range queue {
		var reason string
		// always: the entry that keeps a card queued for room is written
		// although the state stays; every case that writes nothing is
		// errStays.
		e := edit{id: q.id, cause: CauseRule, always: true}
		e.next = func(from State, f Facts) (State, error) {
			switch {
			case from != queued, len(f.Open) > 0:
				return State{}, errStays
			case !f.ForRoom && len(f.Dropped) > 0:
				return State{Backlog, Developer}, nil
			}
			hold := f.hold(l)
			switch {
			case hold == "":
				return State{Spec, Run}, nil
			case f.ForRoom && strings.HasPrefix(f.last, hold):
				return State{}, errStays
			}
			return queued, nil
		}
		// A card that waits for room and is now held by another limit keeps
		// its place: only its reason changes.
		e.keepSince = func(to State, f Facts) bool { return to == queued && f.ForRoom }
		e.because = func(to State, f Facts) string {
			switch {
			case to == queued && f.ForRoom:
				reason = f.hold(l)
			case to == queued:
				reason = f.hold(l) + "; " + ReleasedPrefix + cards(f.all)
			case to == (State{Spec, Run}) && f.ForRoom:
				reason = roomFreedReason(f, l)
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
			errs = append(errs, fmt.Errorf("card %d: %w", q.id, err))
			continue
		}
		out = append(out, Released{c, reason})
	}
	return out, errors.Join(errs...)
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

// ErrFix is the error of a card that a new card cannot name as the card it
// fixes: one that does not exist, was dropped or is not merged, or the new
// card itself. The error CreateFixing returns is the reason alone, and
// errors.Is finds ErrFix in it.
var ErrFix = errors.New("the card cannot fix that card")

// fixError is a refused fix, with its reason.
type fixError string

func (e fixError) Error() string        { return string(e) }
func (e fixError) Is(target error) bool { return target == ErrFix }

// writeFix writes the card a new card fixes, in the transaction that writes
// the card, after its row. Only a merged card can be named. The fixed card is
// not changed: its last entry stays the Git host's.
func writeFix(tx *sql.Tx, card, fixes int64) error {
	if fixes == card {
		return fixError(fmt.Sprintf("card %d cannot fix itself", card))
	}
	_, err := readCard(tx, fixes)
	if errors.Is(err, ErrNoCard) {
		return fixError(fmt.Sprintf("no card %d to fix", fixes))
	}
	if err != nil {
		return err
	}
	e, err := lastEntry(tx, fixes)
	if err != nil {
		return err
	}
	switch {
	case Dropped(e.State, e.Cause, e.Reason):
		return fixError(fmt.Sprintf("card %d was dropped; only a merged card can be fixed", fixes))
	case !Merged(e.State, e.Cause, e.Reason):
		return fixError(fmt.Sprintf("card %d is not merged; only a merged card can be fixed", fixes))
	}
	_, err = tx.Exec(`UPDATE cards SET fixes = ? WHERE id = ?`, fixes, card)
	return err
}

// Fix is the card a card fixes, 0 for none, and the cards that name it as the
// card they fix, in ID order.
type Fix struct {
	Fixes   int64
	FixedBy []int64
}

// Fix reads a card's Fix, in one transaction.
func (s *Store) Fix(card int64) (Fix, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Fix{}, err
	}
	defer tx.Rollback()
	var f Fix
	var fixes sql.NullInt64
	err = tx.QueryRow(`SELECT fixes FROM cards WHERE id = ?`, card).Scan(&fixes)
	if errors.Is(err, sql.ErrNoRows) {
		return Fix{}, fmt.Errorf("%w %d: %w", ErrNoCard, card, err)
	}
	if err != nil {
		return Fix{}, err
	}
	f.Fixes = fixes.Int64
	rows, err := tx.Query(`SELECT id FROM cards WHERE fixes = ? ORDER BY id`, card)
	if err != nil {
		return Fix{}, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Fix{}, err
		}
		f.FixedBy = append(f.FixedBy, id)
	}
	if err := errorsOf(rows); err != nil {
		return Fix{}, err
	}
	return f, tx.Commit()
}
