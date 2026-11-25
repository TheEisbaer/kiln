package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// MaxWorkItem is the largest work item number a card can hold: the work items
// of Azure DevOps are 32-bit integers.
const MaxWorkItem = 2147483647

// ErrWorkItem is the error of a work item number or a move with one that is
// refused. The error carries the reason alone, and errors.Is finds ErrWorkItem
// in it.
var ErrWorkItem = errors.New("the work item is refused")

// workItemError is a refused work item, with its reason.
type workItemError string

func (e workItemError) Error() string        { return string(e) }
func (e workItemError) Is(target error) bool { return target == ErrWorkItem }

// checkWorkItem refuses a number below 1 or above MaxWorkItem.
func checkWorkItem(n int64) error {
	if n < 1 || n > MaxWorkItem {
		return workItemError(fmt.Sprintf("the work item is %d; it is a whole number from 1 to %d", n, MaxWorkItem))
	}
	return nil
}

// writeWorkItem writes a card's work item number, in the transaction that
// writes the card or the move.
func writeWorkItem(tx *sql.Tx, card, n int64) error {
	_, err := tx.Exec(`UPDATE cards SET work_item = ? WHERE id = ?`, n, card)
	return err
}

// readWorkItem reads a card's work item number, 0 for none.
func readWorkItem(q querier, card int64) (int64, error) {
	var n sql.NullInt64
	err := q.QueryRow(`SELECT work_item FROM cards WHERE id = ?`, card).Scan(&n)
	return n.Int64, err
}

// NeedsWorkItemReason is the reason of a card that a repo publishes only with
// a work item number, and has none.
func NeedsWorkItemReason(repo string) string {
	return fmt.Sprintf("the repo %s needs a work item number before publishing; give it with the approval or when you publish", repo)
}

// writeMoveWorkItem is the work item part of the developer's moves, in the
// move's transaction: approve writes the number, replacing the one there;
// publish gives one to a card without, or repeats the card's own, and refuses a
// card of a repo that needs one and has none.
func writeMoveWorkItem(tx *sql.Tx, card int64, action Action, o Options) error {
	have, err := readWorkItem(tx, card)
	if err != nil {
		return err
	}
	switch action {
	case Approve:
		if o.WorkItem != 0 {
			return writeWorkItem(tx, card, o.WorkItem)
		}
	case Publish:
		switch {
		case o.WorkItem != 0 && have != 0 && have != o.WorkItem:
			return workItemError(fmt.Sprintf("the card's work item is #%d; only an approval of its spec changes it", have))
		case o.WorkItem != 0:
			return writeWorkItem(tx, card, o.WorkItem)
		case o.NeedsWorkItem && have == 0:
			c, err := readCard(tx, card)
			if err != nil {
				return err
			}
			return workItemError(NeedsWorkItemReason(c.Repo))
		}
	}
	return nil
}
