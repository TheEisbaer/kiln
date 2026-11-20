package store

import (
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
	MarkDone Action = "done"      // mark a card whose review passed done
	ToSpec   Action = "to-spec"   // send a stage that needs you back to a spec run
	Drop     Action = "drop"      // drop a card that will not produce a change
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

// move is one row of the table of moves: from a state, to a state, and what
// it takes of a note.
type move struct {
	action Action
	from   State
	to     State
	note   Note
}

// dropped is where a drop leaves a card: in done, waiting for nothing.
var dropped = State{Done, Nothing}

// moves is the table of the developer's moves, the only ones Decide makes. A
// card whose review passed waits in review for the developer's approval
// (card 14); until Kiln publishes, the developer says that it is done. A spec
// that waits for approval, or a review that passed, is sent back only with a
// note that says what should change; a stage that needs you is run again or
// sent back with one or without, to implement or to a new spec, when the
// approved spec no longer fits (card 16). Drop is in the table from every
// state but done, and only for a card without a run in flight (see Decide).
var moves = append([]move{
	{Approve, State{Spec, Approval}, State{Implement, Run}, NoNote},
	{Again, State{Spec, Approval}, State{Spec, Run}, RequiredNote},
	{Again, State{Spec, Developer}, State{Spec, Run}, OptionalNote},
	{Again, State{Implement, Developer}, State{Implement, Run}, OptionalNote},
	{Again, State{Review, Developer}, State{Review, Run}, OptionalNote},
	{SendBack, State{Review, Developer}, State{Implement, Run}, OptionalNote},
	{MarkDone, State{Review, Approval}, State{Done, Nothing}, NoNote},
	{SendBack, State{Review, Approval}, State{Implement, Run}, RequiredNote},
	{ToSpec, State{Implement, Developer}, State{Spec, Run}, OptionalNote},
	{ToSpec, State{Review, Developer}, State{Spec, Run}, OptionalNote},
}, dropRows()...)

// dropRows are the rows of drop: from every pairing but done, in the order of
// the stages.
func dropRows() []move {
	var rows []move
	for _, st := range []Stage{Backlog, Spec, Implement, Review} {
		for _, w := range []WaitsFor{Developer, Room, Run, Approval, Host} {
			if from := (State{st, w}); valid[from] {
				rows = append(rows, move{Drop, from, dropped, OptionalNote})
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

// Moves are the moves the table allows from a state, in the table's order; with
// a run in flight there is no drop.
func Moves(from State, inFlight bool) []Allowed {
	var out []Allowed
	for _, m := range moves {
		if m.from == from && !(m.action == Drop && inFlight) {
			out = append(out, Allowed{m.action, m.note})
		}
	}
	return out
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
// what follows the words of a drop; "" on an entry of another cause.
func (e Entry) Note() string {
	if e.Cause != CauseDeveloper {
		return ""
	}
	if Dropped(e.State, e.Cause, e.Reason) {
		return strings.TrimPrefix(strings.TrimPrefix(e.Reason, DroppedReason), ": ")
	}
	return e.Reason
}

// Decide makes one of the developer's moves on a card, by the table, with the
// cause developer and the note, trimmed, as the reason of its entry; a drop's
// reason is DroppedReason, followed by the note. The card's state, and whether
// it has a run in flight, are read and changed in the one transaction of
// change, so that a move is judged against the state it changes. A move the
// table does not have for the card's state is refused with ErrRefused, a note
// that does not fit the move with ErrNote, and the card is returned as it is.
func (s *Store) Decide(id int64, action Action, note string) (Card, error) {
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
	if action == Drop {
		reason = DroppedReason
		if note != "" {
			reason += ": " + note
		}
	}
	return s.change(false, id, "", func(from State, inFlight bool) (State, error) {
		for _, m := range moves {
			if m.action != action || m.from != from || (m.action == Drop && inFlight) {
				continue
			}
			switch {
			case m.note == RequiredNote && note == "":
				return State{}, fmt.Errorf("%w: this move needs a note that says what should change", ErrNote)
			case m.note == NoNote && note != "":
				return State{}, fmt.Errorf("%w: this move takes no note", ErrNote)
			}
			return m.to, nil
		}
		if action == Drop && inFlight {
			return State{}, fmt.Errorf("%w: a card with a run in flight is not dropped", ErrRefused)
		}
		return State{}, fmt.Errorf("%w: %q from %s waiting for %s", ErrRefused, action, from.Stage, from.WaitsFor)
	}, CauseDeveloper, reason, nil)
}
