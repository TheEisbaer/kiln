// Package store keeps a card, with its state and its history, in the one
// embedded database file of the instance (ADR 0032). A card's state and the
// entry that says what changed are written together, in one transaction, by
// one function (ADR 0042), and only a pairing of the closed lists that ADR 0043
// allows can be written.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver" // registers the "sqlite3" driver
)

// version is the version of the store's shape, kept in the file's
// PRAGMA user_version. 0 is a file with nothing in it (ADR 0036).
const version = 1

// Stage is one of the five stages of a card (ADR 0009).
type Stage string

// The stages, in order.
const (
	Backlog   Stage = "backlog"
	Spec      Stage = "spec"
	Implement Stage = "implement"
	Review    Stage = "review"
	Done      Stage = "done"
)

// WaitsFor is what a card waits for (ADR 0043).
type WaitsFor string

// What a card waits for.
const (
	Developer WaitsFor = "developer"
	Room      WaitsFor = "room"
	Run       WaitsFor = "run"
	Approval  WaitsFor = "approval"
	Host      WaitsFor = "host"
	Nothing   WaitsFor = "nothing"
)

// Cause is who caused a change (ADR 0042).
type Cause string

// The causes of a change.
const (
	CauseDeveloper Cause = "developer"
	CauseRun       Cause = "run"
	CauseHost      Cause = "host"
	CauseRule      Cause = "rule"
)

// State is a card's stage and what it waits for.
type State struct {
	Stage    Stage
	WaitsFor WaitsFor
}

// valid are the 11 pairings of ADR 0043 that can be written.
var valid = map[State]bool{
	{Backlog, Developer}: true, {Backlog, Room}: true,
	{Spec, Run}: true, {Spec, Approval}: true, {Spec, Developer}: true,
	{Implement, Run}: true, {Implement, Developer}: true,
	{Review, Run}: true, {Review, Developer}: true, {Review, Host}: true,
	{Done, Nothing}: true,
}

// causes are the causes that can be written.
var causes = map[Cause]bool{CauseDeveloper: true, CauseRun: true, CauseHost: true, CauseRule: true}

// Card is a card as it is now.
type Card struct {
	ID   int64
	Text string
	State
}

// Entry is one change of a card: its state after the change, when, and by
// whom. The state before it is in the card's previous entry.
type Entry struct {
	Number int64
	Card   int64
	At     time.Time
	Cause  Cause
	State
}

// Store is the open store.
type Store struct {
	db *sql.DB
}

// schema is version 1. Times are RFC 3339 text in UTC, so that a dump can be
// read.
const schema = `
CREATE TABLE cards (
	id INTEGER PRIMARY KEY,
	text TEXT NOT NULL,
	stage TEXT NOT NULL,
	waits_for TEXT NOT NULL
) STRICT;
CREATE TABLE entries (
	number INTEGER PRIMARY KEY,
	card INTEGER NOT NULL,
	at TEXT NOT NULL,
	cause TEXT NOT NULL,
	stage TEXT NOT NULL,
	waits_for TEXT NOT NULL
) STRICT;
PRAGMA user_version = 1;
`

// Open opens the store in the file at path, and creates it when the file is
// empty. A store of a newer version is refused, and nothing is written to it
// (ADR 0036).
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.prepare(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	return s, nil
}

// prepare creates the schema of an empty file, in one transaction, or refuses a
// file of a newer version.
func (s *Store) prepare() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	switch {
	case v == 0:
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
	case v > version:
		return fmt.Errorf("the store is version %d and this core knows version %d", v, version)
	}
	return tx.Commit()
}

// Close closes the store.
func (s *Store) Close() error { return s.db.Close() }

// Create writes a new card in the backlog, waiting for the developer, with its
// first entry.
func (s *Store) Create(text string) (Card, error) {
	if text == "" {
		return Card{}, errors.New("a card needs a text")
	}
	return s.change(true, 0, text, State{Backlog, Developer}, CauseDeveloper)
}

// Move changes a card's state. A pairing or a cause that cannot be written is
// refused. A card that already has the state is returned as it is, so that a
// repeated Move writes nothing (ADR 0035).
func (s *Store) Move(id int64, to State, cause Cause) (Card, error) {
	return s.change(false, id, "", to, cause)
}

// Card reads a card.
func (s *Store) Card(id int64) (Card, error) {
	return readCard(s.db, id)
}

// History reads the entries of a card, in the order of their numbers.
func (s *Store) History(id int64) ([]Entry, error) {
	rows, err := s.db.Query(`SELECT number, card, at, cause, stage, waits_for
		FROM entries WHERE card = ? ORDER BY number`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		var e Entry
		var at string
		if err := rows.Scan(&e.Number, &e.Card, &at, &e.Cause, &e.Stage, &e.WaitsFor); err != nil {
			return nil, err
		}
		if e.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("entry %d: %w", e.Number, err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// afterCardWritten, set only by the test, is called in change between the
// card's row and its entry. It can stop the process, as a kill there would.
var afterCardWritten func() error

// change is the one place every change to a card goes through (ADR 0042). In
// one transaction it checks the pairing and the cause, writes the card's row
// and then the entry, and commits. It creates a card with the text when create
// is set, and changes the card id otherwise.
func (s *Store) change(create bool, id int64, text string, to State, cause Cause) (Card, error) {
	if !valid[to] {
		return Card{}, fmt.Errorf("a card cannot be in %s waiting for %s (ADR 0043)", to.Stage, to.WaitsFor)
	}
	if !causes[cause] {
		return Card{}, fmt.Errorf("%q is not a cause of a change (ADR 0042)", cause)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Card{}, err
	}
	defer tx.Rollback()

	var card Card
	if create {
		res, err := tx.Exec(`INSERT INTO cards (text, stage, waits_for) VALUES (?, ?, ?)`,
			text, to.Stage, to.WaitsFor)
		if err != nil {
			return Card{}, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return Card{}, err
		}
		card = Card{ID: id, Text: text}
	} else {
		if card, err = readCard(tx, id); err != nil {
			return Card{}, err
		}
		if card.State == to {
			return card, nil
		}
		if _, err := tx.Exec(`UPDATE cards SET stage = ?, waits_for = ? WHERE id = ?`,
			to.Stage, to.WaitsFor, id); err != nil {
			return Card{}, err
		}
	}
	card.State = to

	if afterCardWritten != nil {
		if err := afterCardWritten(); err != nil {
			return Card{}, err
		}
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO entries (card, at, cause, stage, waits_for) VALUES (?, ?, ?, ?, ?)`,
		id, at, cause, to.Stage, to.WaitsFor); err != nil {
		return Card{}, err
	}
	if err := tx.Commit(); err != nil {
		return Card{}, err
	}
	return card, nil
}

// querier is a database or a transaction.
type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func readCard(q querier, id int64) (Card, error) {
	c := Card{ID: id}
	err := q.QueryRow(`SELECT text, stage, waits_for FROM cards WHERE id = ?`, id).
		Scan(&c.Text, &c.Stage, &c.WaitsFor)
	if errors.Is(err, sql.ErrNoRows) {
		return Card{}, fmt.Errorf("no card %d: %w", id, err)
	}
	return c, err
}
