package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Action is one of the developer's moves on a card, sent from the screen.
type Action string

// The developer's moves.
const (
	Approve  Action = "approve"   // approve a spec (ADR 0008)
	Again    Action = "again"     // give a stage another run, or send a spec back
	SendBack Action = "send-back" // send a review back to implement
	MarkDone Action = "done"      // mark a card whose review passed done, or close one at the Git host
	ToSpec   Action = "to-spec"   // send a stage that needs you back to a spec run
	Drop     Action = "drop"      // drop a card that will not produce a change
	Publish  Action = "publish"   // publish a card whose review passed
	Start    Action = "start"     // start a card in the backlog: to a spec run, or queued for its predecessors
	ToHost   Action = "to-host"   // put a card that needs you for a reviewer's feedback back to the Git host (ADR 0028)
	ToReview Action = "to-review" // review the branch as it is, after an implement run that changed nothing
)

// Note is what a move takes of the developer's note: none, one that may be
// left out, or one it cannot be made without.
type Note string

// What a move takes of a note.
const (
	NoNote       Note = "none"
	OptionalNote Note = "optional"
	RequiredNote Note = "required"
)

// MaxNote is the longest note a move takes, in bytes, after it is trimmed.
const MaxNote = 16 << 10

// Facts are what the table of moves looks at besides a card's state: whether
// the card has a run in flight; whether the core has a publish command;
// whether the card's last entry is a publishing that failed (PublishFailed), a
// pull request closed without a merge (PullClosed), or a reviewer's feedback
// on its pull request (Feedback);
// whether the card has a pull request's address, kept by its publishing; and
// its predecessors that are not done, Open, and those that were dropped,
// Dropped, each in ID order (ADR 0010); and, for a card in the backlog, what
// its room under the limits depends on (ADR 0014): the repo's cards in
// progress, InProgress; the instance's cards that count against its limit,
// InInstance; whether the card is queued and waits for room, ForRoom; and the
// cards of every repo that wait for room before it, with their repos' cards
// in progress (see readRoom); whether the card's last entry says its implement
// run changed nothing, Unchanged; and whether its branch holds a change to
// review, HasChange. The store reads all but Publishing and HasChange; the core
// says those.
type Facts struct {
	InFlight      bool
	Publishing    bool
	Unchanged     bool
	HasChange     bool
	PublishFailed bool
	PullClosed    bool
	Feedback      bool
	Published     bool
	Open          []int64
	Dropped       []int64
	InProgress    int
	InInstance    int
	ForRoom       bool
	all           []int64        // every predecessor, in ID order
	repo          string         // the card's repo
	last          string         // the reason of the card's last entry
	ahead         []string       // the repos of the cards that wait for room before it, in order
	counts        map[string]int // the cards in progress of those repos
}

// move is one row of the table of moves: from a state, to a state, what it
// takes of a note, and the facts it needs, if any.
type move struct {
	action Action
	from   State
	to     State
	note   Note
	when   func(Facts) bool
}

// dropped is where a drop leaves a card: in done, waiting for nothing.
var dropped = State{Done, Nothing}

// atHost is where publish leaves a card: in review, waiting for the Git host.
var atHost = State{Review, Host}

// publishing is a row's condition for publish: the core has a publish command.
func publishing(f Facts) bool { return f.Publishing }

// retried is the condition of publish from "needs you": the core has a
// publish command, and the card needs you because its publishing failed or
// its pull request was closed without a merge; publishing then opens a new
// pull request.
func retried(f Facts) bool { return f.Publishing && (f.PublishFailed || f.PullClosed) }

// settled is the condition of a move from the Git host: the card has its pull
// request's address, or the core cannot publish it, so that nothing is under
// way. While the core publishes, a card has no move, as with a run in flight.
func settled(f Facts) bool { return f.Published || !f.Publishing }

// reviewable is the condition of to-review: the card's implement run changed
// nothing, and its branch holds a change to review.
func reviewable(f Facts) bool { return f.Unchanged && f.HasChange }

// feedback is the condition of to-host: the card needs you because a reviewer
// commented on its pull request.
func feedback(f Facts) bool { return f.Feedback }

// notFeedback is the condition of again from review, needing you: running the
// review again is not one of ADR 0028's moves, and a review run would not see
// the reviewer's comments.
func notFeedback(f Facts) bool { return !f.Feedback }

// moves is the table of the developer's moves, the only ones Decide makes. A
// card whose review passed waits in review for the developer's approval:
// with a publish command the developer publishes it, and in any
// case can close it without publishing. A card that needs you because its
// publishing failed, or its pull request was closed without a merge, can be
// published again. A card at the Git host is sent back
// to implement with a note, or closed. A spec that waits for
// approval, or a review that passed, is sent back only with a note that says
// what should change; a stage that needs you is run again or sent back with
// one or without, to implement or to a new spec, when the approved spec no
// longer fits. Drop is in the table from every state but done, and
// only for a card without a run in flight (see Decide), and at the Git host
// only as the moves from there. A card that needs you because a reviewer
// commented on its pull request (ADR 0028) is sent back to implement or to
// spec, dropped, or put back to the Git host, with no again. Start takes a card in the backlog to a spec
// run, or queues it while a predecessor is not done (see DecideWith).
var moves = append([]move{
	{Start, State{Backlog, Developer}, State{Spec, Run}, NoNote, nil},
	{Approve, State{Spec, Approval}, State{Implement, Run}, OptionalNote, nil},
	{Again, State{Spec, Approval}, State{Spec, Run}, RequiredNote, nil},
	{Again, State{Spec, Developer}, State{Spec, Run}, OptionalNote, nil},
	{Again, State{Implement, Developer}, State{Implement, Run}, OptionalNote, nil},
	{ToReview, State{Implement, Developer}, State{Review, Run}, OptionalNote, reviewable},
	{Again, State{Review, Developer}, State{Review, Run}, OptionalNote, notFeedback},
	{SendBack, State{Review, Developer}, State{Implement, Run}, OptionalNote, nil},
	{Publish, State{Review, Developer}, atHost, NoNote, retried},
	{Publish, State{Review, Approval}, atHost, NoNote, publishing},
	{MarkDone, State{Review, Approval}, State{Done, Nothing}, NoNote, nil},
	{SendBack, State{Review, Approval}, State{Implement, Run}, RequiredNote, nil},
	{MarkDone, atHost, State{Done, Nothing}, NoNote, settled},
	{SendBack, atHost, State{Implement, Run}, RequiredNote, settled},
	{ToSpec, State{Implement, Developer}, State{Spec, Run}, OptionalNote, nil},
	{ToSpec, State{Review, Developer}, State{Spec, Run}, OptionalNote, nil},
	{ToHost, State{Review, Developer}, atHost, NoNote, feedback},
}, dropRows()...)

// dropRows are the rows of drop: from every pairing but done, in the order of
// the stages; from the Git host as the other moves from there.
func dropRows() []move {
	var rows []move
	for _, st := range []Stage{Backlog, Spec, Implement, Review} {
		for _, w := range []WaitsFor{Developer, Room, Run, Approval, Host} {
			if from := (State{st, w}); valid[from] {
				var when func(Facts) bool
				if from == atHost {
					when = settled
				}
				rows = append(rows, move{Drop, from, dropped, OptionalNote, when})
			}
		}
	}
	return rows
}

// Allowed is a move the table allows from a state, and what it takes of a note.
type Allowed struct {
	Action Action
	Note   Note
}

// ErrRefused is Decide's error for a move the table does not have for the
// card's state, drop on a card with a run in flight included.
var ErrRefused = errors.New("the move is not in the table for the card's state")

// ErrNote is Decide's error for a note a move does not take: one missing where
// it is required, one given to a move that takes none, or one too long.
var ErrNote = errors.New("the note does not fit the move")

// allows reports whether a row is in the table for a state and its facts;
// with a run in flight there is no drop.
func (m move) allows(from State, f Facts) bool {
	return m.from == from && !(m.action == Drop && f.InFlight) && (m.when == nil || m.when(f))
}

// Moves are the moves the table allows from a state with its facts, in the
// table's order.
func Moves(from State, f Facts) []Allowed {
	var out []Allowed
	for _, m := range moves {
		if m.allows(from, f) {
			out = append(out, Allowed{m.action, m.note})
		}
	}
	return out
}

// Moves are the moves the table allows a card as it is now, for a core that
// has a publish command when publishing is set.
func (s *Store) Moves(c Card, publishing bool) ([]Allowed, error) {
	return s.MovesWith(c, Options{Publishing: publishing})
}

// MovesWith is Moves with the facts the core says, Publishing and HasChange,
// taken from o.
func (s *Store) MovesWith(c Card, o Options) ([]Allowed, error) {
	f, err := readFacts(s.db, c.ID)
	if err != nil {
		return nil, err
	}
	f.Publishing = o.Publishing
	f.HasChange = o.HasChange
	return Moves(c.State, f), nil
}

// DroppedReason is the reason on the entry of a drop, before the developer's
// note, if any.
const DroppedReason = "dropped by the developer"

// Dropped says whether a card's last entry, of the state, the cause and the
// reason, left it dropped: in done by the developer's drop. The table has no
// other move to done with a reason, and nothing moves a card out of done.
func Dropped(s State, cause Cause, reason string) bool {
	return s == dropped && cause == CauseDeveloper && strings.HasPrefix(reason, DroppedReason)
}

// Note is the developer's note on an entry of theirs: the reason of a move, or
// what follows the words of a drop; "" on an entry of another cause. On an
// entry about a proposal it is the note of a dismiss, and "" otherwise.
func (e Entry) Note() string {
	if e.Proposal != 0 {
		return e.proposalNote()
	}
	if e.Cause != CauseDeveloper {
		return ""
	}
	if Dropped(e.State, e.Cause, e.Reason) {
		return strings.TrimPrefix(strings.TrimPrefix(e.Reason, DroppedReason), ": ")
	}
	if strings.HasPrefix(e.Reason, QueuedPrefix) || strings.HasPrefix(e.Reason, RoomPrefix) {
		return "" // the store's words on a start that queued the card
	}
	return e.Reason
}

// Options are what a move depends on besides the card and the note: whether
// the core has a publish command; for publish, the commit of the card's
// branch whose workflow files the developer said yes to, if any (ADR 0060);
// and, for start, the limits of cards in progress (ADR 0014), the zero Limits
// for the defaults. WorkItem is the work item number given with approve or
// publish, 0 for none; NeedsWorkItem says that the card's repo publishes only
// a card with a number, as the core knows it, as it knows Publishing.
type Options struct {
	Publishing    bool
	HasChange     bool // the card's branch holds a change to review, as the core knows it, as it knows Publishing
	WorkflowYes   string
	Limits        Limits
	WorkItem      int64
	NeedsWorkItem bool
}

// Decide makes one of the developer's moves on a card, by the table of a core
// without a publish command (see DecideWith).
func (s *Store) Decide(id int64, action Action, note string) (Card, error) {
	return s.DecideWith(id, action, note, Options{})
}

// DecideWith makes one of the developer's moves on a card, by the table, with
// the cause developer and the note, trimmed, as the reason of its entry; a
// drop's reason is DroppedReason, followed by the note. The card's state and
// its facts are read and changed in the one transaction of change, so that a
// move is judged against the state it changes. Start queues the card, with
// QueuedPrefix and the predecessors it waits for as its reason, while one is
// not done, and, once none is open, with the reason of RoomPrefix while its
// repo or the instance has no room under o.Limits (ADR 0014). A move the table does not have
// for the card's state is refused with ErrRefused, a note that does not fit
// the move with ErrNote, and the card is returned as it is. Publish writes
// the card's publication anew in the same transaction: the developer's yes,
// if any, and no commit and no address yet. ToHost sets the publication's
// feedback_since to the move's time in the same transaction, and leaves the
// rest of the publication as it is.
func (s *Store) DecideWith(id int64, action Action, note string, o Options) (Card, error) {
	if o.WorkItem != 0 {
		if action != Approve && action != Publish {
			return s.refusedWorkItem(id, workItemError("only approve and publish take a work item"))
		}
		if err := checkWorkItem(o.WorkItem); err != nil {
			return s.refusedWorkItem(id, err)
		}
	}
	note = strings.TrimSpace(note)
	if len(note) > MaxNote {
		c, err := s.Card(id)
		if err != nil {
			return Card{}, err
		}
		// In tenths of a KiB, rounded up, so that it never reads as the limit.
		tenths := (len(note)*10 + 1023) / 1024
		return c, fmt.Errorf("%w: the note is %d.%d KiB, more than 16 KiB", ErrNote, tenths/10, tenths%10)
	}
	reason := note
	if action == Approve {
		reason = "" // the note is kept on the spec's row, not told twice
	}
	if action == Drop {
		reason = DroppedReason
		if note != "" {
			reason += ": " + note
		}
	}
	// Approving a spec starts the number of send-backs again (ADR 0054); the
	// developer's other moves leave it.
	e := edit{id: id, cause: CauseDeveloper, reason: reason, approved: action == Approve, mark: action == ToHost}
	var movedFrom State // the state e.next saw, for the correction e.write keeps
	e.next = func(from State, f Facts) (State, error) {
		movedFrom = from
		f.Publishing = o.Publishing
		f.HasChange = o.HasChange
		for _, m := range moves {
			if m.action != action || !m.allows(from, f) {
				continue
			}
			switch {
			case m.note == RequiredNote && note == "":
				return State{}, fmt.Errorf("%w: this move needs a note that says what should change", ErrNote)
			case m.note == NoNote && note != "":
				return State{}, fmt.Errorf("%w: this move takes no note", ErrNote)
			}
			if action == Start {
				return queueing(m.to, f, o.Limits), nil
			}
			return m.to, nil
		}
		switch {
		case action == Drop && f.InFlight:
			return State{}, fmt.Errorf("%w: a card with a run in flight is not dropped", ErrRefused)
		case from == atHost && !settled(f):
			return State{}, fmt.Errorf("%w: the card is being published; it has no move until that ends", ErrRefused)
		}
		return State{}, fmt.Errorf("%w: %q from %s waiting for %s", ErrRefused, action, from.Stage, from.WaitsFor)
	}
	if action == Start {
		e.because = func(to State, f Facts) string { return queuedReason(to, f, o.Limits) }
	}
	if action == Approve || action == Publish {
		e.write = func(tx *sql.Tx, card int64) error {
			if action == Publish {
				if err := writePublication(tx, card, o.WorkflowYes); err != nil {
					return err
				}
			}
			if err := writeMoveWorkItem(tx, card, action, o); err != nil {
				return err
			}
			if action != Approve || note == "" {
				return nil
			}
			res, err := tx.Exec(`UPDATE specs SET note = ? WHERE rowid = (SELECT rowid FROM specs WHERE card = ? AND kind = ? ORDER BY rowid DESC LIMIT 1)`,
				note, card, KindSpec)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 0 {
				return fmt.Errorf("%w: the card has no spec to keep the note with", ErrRefused)
			}
			return nil
		}
	}
	if (action == SendBack || action == Again || action == ToReview) && note != "" {
		e.write = func(tx *sql.Tx, card int64) error {
			if movedFrom.Stage != Implement && movedFrom.Stage != Review {
				return nil
			}
			// No spec, no row: the card is not refused for it.
			_, err := tx.Exec(`INSERT INTO corrections (card, spec, action, from_stage, from_waits_for, note)
				SELECT ?, run, ?, ?, ?, ? FROM specs WHERE card = ? AND kind = ? ORDER BY rowid DESC LIMIT 1`,
				card, action, movedFrom.Stage, movedFrom.WaitsFor, note, card, KindSpec)
			return err
		}
	}
	c, err := s.change(e)
	if errors.Is(err, ErrRefused) && c.ID == 0 {
		// The refusal came from the write, which rolled everything back.
		if cur, cerr := s.Card(id); cerr == nil {
			c = cur
		}
	}
	return c, err
}

// refusedWorkItem returns the card as it is, with the refusal of a work item.
func (s *Store) refusedWorkItem(id int64, err error) (Card, error) {
	c, cerr := s.Card(id)
	if cerr != nil {
		return Card{}, cerr
	}
	return c, err
}

// writePublication writes a card's publication anew: the developer's yes, if
// any, and no commit and no address yet. Its branch and its feedback_since are
// kept.
func writePublication(tx *sql.Tx, card int64, yes string) error {
	_, err := tx.Exec(`INSERT INTO publishes (card, workflow_yes, commit_id, address) VALUES (?, ?, NULL, NULL)
		ON CONFLICT (card) DO UPDATE SET workflow_yes = excluded.workflow_yes, commit_id = NULL, address = NULL`,
		card, sql.NullString{String: yes, Valid: yes != ""})
	return err
}
