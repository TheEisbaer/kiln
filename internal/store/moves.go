package store

import (
	"errors"
	"fmt"
)

// Action is one of the developer's moves on a card, sent from the screen.
type Action string

// The developer's moves.
const (
	Approve  Action = "approve"   // approve a spec (ADR 0008)
	Again    Action = "again"     // give a stage that needs you another run
	SendBack Action = "send-back" // send a review that needs you back to implement
	MarkDone Action = "done"      // mark a card whose review passed done
)

// move is one row of the table of moves: from a state, to a state.
type move struct {
	action Action
	from   State
	to     State
}

// moves is the table of the developer's moves, the only ones Decide makes. A
// card whose review passed waits in review for the developer's approval
// (card 14); until Kiln publishes, the developer says that it is done.
var moves = []move{
	{Approve, State{Spec, Approval}, State{Implement, Run}},
	{Again, State{Spec, Developer}, State{Spec, Run}},
	{Again, State{Implement, Developer}, State{Implement, Run}},
	{Again, State{Review, Developer}, State{Review, Run}},
	{SendBack, State{Review, Developer}, State{Implement, Run}},
	{MarkDone, State{Review, Approval}, State{Done, Nothing}},
}

// ErrRefused is Decide's error for a move the table does not have for the
// card's state.
var ErrRefused = errors.New("the move is not in the table for the card's state")

// Moves are the moves the table allows from a state, in the table's order.
func Moves(from State) []Action {
	var out []Action
	for _, m := range moves {
		if m.from == from {
			out = append(out, m.action)
		}
	}
	return out
}

// Decide makes one of the developer's moves on a card, by the table, with the
// cause developer. The card's state is read and changed in the one
// transaction of change, so that a move is judged against the state it
// changes. A move the table does not have for the card's state is refused
// with ErrRefused, and the card is returned as it is.
func (s *Store) Decide(id int64, action Action) (Card, error) {
	return s.change(false, id, "", func(from State) (State, error) {
		for _, m := range moves {
			if m.action == action && m.from == from {
				return m.to, nil
			}
		}
		return State{}, fmt.Errorf("%w: %q from %s waiting for %s", ErrRefused, action, from.Stage, from.WaitsFor)
	}, CauseDeveloper, "", nil)
}
