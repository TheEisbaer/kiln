package store

import (
	"database/sql"
	"time"
)

// The instance's own state (ADR 0055): whether it is paused, and why. A
// paused instance starts no run (ADR 0016, rule 3). The state and its entry
// are written together, in one transaction, as a card's are (ADR 0042).

// InstanceEntry is one change of the instance's state: the reason it is
// paused for from then on, or "" when the pause ended.
type InstanceEntry struct {
	Number int64
	At     time.Time
	Paused string
}

// Paused is the reason the instance is paused, or "" when it is not.
func (s *Store) Paused() (string, error) {
	var why sql.NullString
	err := s.db.QueryRow(`SELECT paused FROM instance`).Scan(&why)
	return why.String, err
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
	why := sql.NullString{String: reason, Valid: reason != ""}
	if _, err := tx.Exec(`UPDATE instance SET paused = ?`, why); err != nil {
		return false, err
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO instance_entries (at, paused) VALUES (?, ?)`, at, why); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// InstanceHistory reads the instance's entries, in the order of their numbers.
func (s *Store) InstanceHistory() ([]InstanceEntry, error) {
	rows, err := s.db.Query(`SELECT number, at, paused FROM instance_entries ORDER BY number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []InstanceEntry
	for rows.Next() {
		var e InstanceEntry
		var at string
		var why sql.NullString
		if err := rows.Scan(&e.Number, &at, &why); err != nil {
			return nil, err
		}
		if e.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		e.Paused = why.String
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
