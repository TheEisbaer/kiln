package store

import (
	"database/sql"
	"fmt"
	"time"
)

// The instance's own state (ADR 0055): whether it is paused, and why, and
// whether automatic merging is off after a failed update (ADR 0019, rule 3).
// A paused instance starts no run (ADR 0016, rule 3); with merging off the
// rule publishes no card. The state and its entry are written together, in
// one transaction, as a card's are (ADR 0042).

// InstanceEntry is one change of the instance's state: its whole state after
// the change, and who caused it.
type InstanceEntry struct {
	Number int64
	At     time.Time
	Cause  Cause
	// Paused is the reason the instance is paused for from then on, or ""
	// when it is not.
	Paused string
	// MergingOff is why automatic merging is off from then on, or "" when it
	// is on; Failed is the commit of the failed update.
	MergingOff string
	Failed     string
}

// Merging is whether automatic merging is on.
type Merging struct {
	// Off is why automatic merging is off, or "" when it is on.
	Off string
	// Failed is the commit of the failed update, which the core does not
	// update to while merging is off; "" when it is on, or when the failed
	// program had no commit in its name.
	Failed string
}

// Paused is the reason the instance is paused, or "" when it is not.
func (s *Store) Paused() (string, error) {
	var why sql.NullString
	err := s.db.QueryRow(`SELECT paused FROM instance`).Scan(&why)
	return why.String, err
}

// Merging reads whether automatic merging is on.
func (s *Store) Merging() (Merging, error) {
	var off, failed sql.NullString
	err := s.db.QueryRow(`SELECT merging_off, failed_update FROM instance`).Scan(&off, &failed)
	return Merging{Off: off.String, Failed: failed.String}, err
}

// instanceEntry writes an entry of the instance's state as the row holds it
// in tx, after a change.
func instanceEntry(tx *sql.Tx, cause Cause) error {
	at := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := tx.Exec(`INSERT INTO instance_entries (at, cause, paused, merging_off, failed_update)
SELECT ?, ?, paused, merging_off, failed_update FROM instance`, at, string(cause))
	return err
}

// nullable is text written as NULL when it is empty.
func nullable(text string) sql.NullString {
	return sql.NullString{String: text, Valid: text != ""}
}

// SetPaused pauses the instance with a reason, or ends its pause with "", and
// reports whether that changed anything. The state and its entry are written
// in one transaction; a reason the instance is paused for already writes
// nothing.
func (s *Store) SetPaused(reason string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var was sql.NullString
	if err := tx.QueryRow(`SELECT paused FROM instance`).Scan(&was); err != nil {
		return false, err
	}
	if was.String == reason {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE instance SET paused = ?`, nullable(reason)); err != nil {
		return false, err
	}
	if err := instanceEntry(tx, CauseRule); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// TurnMergingOff turns automatic merging off by the rule, with why and the
// commit of the failed update, and reports whether that changed anything.
// The state and its entry are written in one transaction; the same reason and
// commit again write nothing, and others replace them.
func (s *Store) TurnMergingOff(reason, failed string) (bool, error) {
	if reason == "" {
		return false, fmt.Errorf("automatic merging is not turned off without a reason")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var off, was sql.NullString
	if err := tx.QueryRow(`SELECT merging_off, failed_update FROM instance`).Scan(&off, &was); err != nil {
		return false, err
	}
	if off.String == reason && was.String == failed {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE instance SET merging_off = ?, failed_update = ?`, reason, nullable(failed)); err != nil {
		return false, err
	}
	if err := instanceEntry(tx, CauseRule); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// TurnMergingOn is the developer's move that turns automatic merging on
// again, and forgets the failed update's commit, so that the core may update
// to it again. The state and its entry are written in one transaction; with
// merging on it is refused with ErrRefused and writes nothing.
func (s *Store) TurnMergingOn() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var off sql.NullString
	if err := tx.QueryRow(`SELECT merging_off FROM instance`).Scan(&off); err != nil {
		return err
	}
	if off.String == "" {
		return fmt.Errorf("%w: automatic merging is on", ErrRefused)
	}
	if _, err := tx.Exec(`UPDATE instance SET merging_off = NULL, failed_update = NULL`); err != nil {
		return err
	}
	if err := instanceEntry(tx, CauseDeveloper); err != nil {
		return err
	}
	return tx.Commit()
}

// InstanceHistory reads the instance's entries, in the order of their numbers.
func (s *Store) InstanceHistory() ([]InstanceEntry, error) {
	rows, err := s.db.Query(`SELECT number, at, cause, paused, merging_off, failed_update FROM instance_entries ORDER BY number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []InstanceEntry
	for rows.Next() {
		var e InstanceEntry
		var at, cause string
		var why, off, failed sql.NullString
		if err := rows.Scan(&e.Number, &at, &cause, &why, &off, &failed); err != nil {
			return nil, err
		}
		if e.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		e.Cause, e.Paused, e.MergingOff, e.Failed = Cause(cause), why.String, off.String, failed.String
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
