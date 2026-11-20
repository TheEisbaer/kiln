// Package store keeps a card, with its state and its history, in the one
// embedded database file of the instance (ADR 0032). A card's state and the
// entry that says what changed are written together, in one transaction, by
// one function (ADR 0042), and only a pairing of the closed lists that ADR 0043
// allows can be written.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver" // registers the "sqlite3" driver
)

// version is the version of the store's shape, kept in the file's
// PRAGMA user_version. 0 is a file with nothing in it (ADR 0036).
const version = 5

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

// valid are the 11 pairings of ADR 0043 that can be written, and review
// waiting for the developer's approval: a review that passed, until the core
// publishes (card 14).
var valid = map[State]bool{
	{Backlog, Developer}: true, {Backlog, Room}: true,
	{Spec, Run}: true, {Spec, Approval}: true, {Spec, Developer}: true,
	{Implement, Run}: true, {Implement, Developer}: true,
	{Review, Run}: true, {Review, Developer}: true, {Review, Host}: true, {Review, Approval}: true,
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
// whom. The state before it is in the card's previous entry. Run and Reason
// are empty except on the entry that ends a run.
type Entry struct {
	Number int64
	Card   int64
	At     time.Time
	Cause  Cause
	State
	Run    string
	Reason string
}

// Ending is how a run ended. A run in flight has none (ADR 0043).
type Ending string

// The endings a run has so far.
const (
	Finished Ending = "finished"
	Failed   Ending = "failed"
)

// Doc is a run's document as EndRun keeps it: its kind, KindSpec, KindReport
// or KindReview, and its text, exactly as the run wrote it.
type Doc struct {
	Kind string
	Text string
}

// SpecRow is a card's spec, as the store keeps it: its text, exactly as the run
// wrote it, and the run that wrote it, with that run's commit.
type SpecRow struct {
	Run    string
	Commit string
	Text   string
}

// RunRow is one run of a card, as the store keeps it. An empty Ending means
// the run is in flight.
type RunRow struct {
	ID     string
	Card   int64
	Commit string
	Model  string
	Effort string
	Ending Ending
}

// Store is the open store.
//
// The tick and the screen's handlers share one Store. It holds a single
// connection to the file (SetMaxOpenConns(1) in Open), and a transaction holds
// that connection from its Begin to its Commit or Rollback: a handler's write
// waits for the tick's transaction to end, and the tick's for the handler's.
// Every read and write is one statement or one transaction, and none of them
// waits on anything outside the store, so no wait is longer than one of them.
type Store struct {
	db *sql.DB
}

// migrations[n] moves a store of version n to version n+1, so that a store of
// version 0, an empty file, is made by all of them in turn (ADR 0036). Times
// are RFC 3339 text in UTC, so that a dump can be read.
var migrations = []func(tx *sql.Tx) error{
	func(tx *sql.Tx) error {
		_, err := tx.Exec(`
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
) STRICT;`)
		return err
	},
	func(tx *sql.Tx) error {
		// The instance's identity is eight random hex digits, written once.
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE instance (id TEXT NOT NULL) STRICT;
CREATE TABLE runs (
	id TEXT PRIMARY KEY,
	card INTEGER NOT NULL,
	commit_id TEXT NOT NULL,
	model TEXT NOT NULL,
	effort TEXT NOT NULL,
	ending TEXT
) STRICT;
CREATE UNIQUE INDEX runs_in_flight ON runs(card) WHERE ending IS NULL;
ALTER TABLE entries ADD COLUMN run TEXT;
ALTER TABLE entries ADD COLUMN reason TEXT;`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO instance (id) VALUES (?)`, hex.EncodeToString(b[:]))
		return err
	},
	func(tx *sql.Tx) error {
		// One row per spec, written with the ending of the run that wrote it.
		_, err := tx.Exec(`CREATE TABLE specs (run TEXT PRIMARY KEY, card INTEGER NOT NULL, text TEXT NOT NULL) STRICT`)
		return err
	},
	func(tx *sql.Tx) error {
		// When a run was written in flight, for the screen; a run written
		// before has none.
		_, err := tx.Exec(`ALTER TABLE runs ADD COLUMN started TEXT`)
		return err
	},
	func(tx *sql.Tx) error {
		// The table holds every run's document, one per run, by its kind; the
		// rows that are there are specs.
		_, err := tx.Exec(`ALTER TABLE specs ADD COLUMN kind TEXT NOT NULL DEFAULT 'spec'`)
		return err
	},
}

// afterMigration, set only by the test, is called when the store has been moved
// forward and before the transaction is committed. It can stop the process, as
// a kill there would.
var afterMigration func()

// Open opens the store in the file at path, and creates it when the file is
// empty. A store of an older version is moved forward, and a store of a newer
// one is refused, and nothing is written to it (ADR 0036).
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

// prepare moves the schema forward to the current version, in one transaction,
// or refuses a file of a newer version.
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
	if v > version {
		return fmt.Errorf("the store is version %d and this core knows version %d", v, version)
	}
	if v < version {
		for _, migrate := range migrations[v:] {
			if err := migrate(tx); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
			return err
		}
		if afterMigration != nil {
			afterMigration()
		}
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
	return s.change(true, 0, text, fixed(State{Backlog, Developer}), CauseDeveloper, "", nil)
}

// Move changes a card's state. A pairing or a cause that cannot be written is
// refused. A card that already has the state is returned as it is, so that a
// repeated Move writes nothing (ADR 0035).
func (s *Store) Move(id int64, to State, cause Cause) (Card, error) {
	return s.change(false, id, "", fixed(to), cause, "", nil)
}

// MoveWithReason moves a card that is in the state from, with a reason on the
// entry, for a card that the core stops without a run, such as a review run
// that is not started. A card that is no longer in from, such as one the
// developer dropped meanwhile, is refused with ErrRefused and returned as it
// is.
func (s *Store) MoveWithReason(id int64, from, to State, cause Cause, reason string) (Card, error) {
	return s.change(false, id, "", func(have State, _ bool) (State, error) {
		if have != from {
			return State{}, fmt.Errorf("%w: the card is in %s waiting for %s, not %s waiting for %s",
				ErrRefused, have.Stage, have.WaitsFor, from.Stage, from.WaitsFor)
		}
		return to, nil
	}, cause, reason, nil)
}

// ErrNoCard is the error of a card that is not in the store.
var ErrNoCard = errors.New("no card")

// Card reads a card.
func (s *Store) Card(id int64) (Card, error) {
	return readCard(s.db, id)
}

// History reads the entries of a card, in the order of their numbers.
func (s *Store) History(id int64) ([]Entry, error) {
	rows, err := s.db.Query(`SELECT number, card, at, cause, stage, waits_for, run, reason
		FROM entries WHERE card = ? ORDER BY number`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		var e Entry
		var at string
		var run, reason sql.NullString
		if err := rows.Scan(&e.Number, &e.Card, &at, &e.Cause, &e.Stage, &e.WaitsFor, &run, &reason); err != nil {
			return nil, err
		}
		e.Run, e.Reason = run.String, reason.String
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

// runEnd is the ending of a run, written by change together with the card's
// new state, and the document it wrote, if any.
type runEnd struct {
	id     string
	ending Ending
	doc    *Doc
}

// change is the one place every change to a card goes through (ADR 0042). In
// one transaction it checks the pairing and the cause, writes the card's row
// and then the entry, and commits. It creates a card with the text when create
// is set, and changes the card id otherwise. The new state is next of the
// card's state as it is in the transaction, the zero State for a card that is
// created, and whether the card has a run in flight; an error of next is
// returned and nothing is written. When end is
// set, the card is the one of that run, and the run's ending is written in the
// same transaction, with the run on the entry, and with the run's document
// when it is given; a run that has ended already writes nothing, its document
// included. The reason, if any, is on the entry.
func (s *Store) change(create bool, id int64, text string, next func(from State, inFlight bool) (State, error), cause Cause, reason string, end *runEnd) (Card, error) {
	if !causes[cause] {
		return Card{}, fmt.Errorf("%q is not a cause of a change (ADR 0042)", cause)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Card{}, err
	}
	defer tx.Rollback()

	if end != nil {
		run, err := readRun(tx, end.id)
		if err != nil {
			return Card{}, err
		}
		if run.Ending != "" {
			return readCard(tx, run.Card)
		}
		id = run.Card
	}

	var card Card
	inFlight := false
	if create {
		card = Card{Text: text}
	} else if card, err = readCard(tx, id); err != nil {
		return Card{}, err
	} else if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM runs WHERE card = ? AND ending IS NULL)`, id).Scan(&inFlight); err != nil {
		return Card{}, err
	}
	to, err := next(card.State, inFlight)
	if err != nil {
		return card, err
	}
	if !valid[to] {
		return Card{}, fmt.Errorf("a card cannot be in %s waiting for %s (ADR 0043)", to.Stage, to.WaitsFor)
	}

	if end != nil {
		if _, err := tx.Exec(`UPDATE runs SET ending = ? WHERE id = ?`, end.ending, end.id); err != nil {
			return Card{}, err
		}
		if end.doc != nil {
			if _, err := tx.Exec(`INSERT INTO specs (run, card, text, kind) VALUES (?, ?, ?, ?)`,
				end.id, id, end.doc.Text, end.doc.Kind); err != nil {
				return Card{}, err
			}
		}
	}

	if create {
		res, err := tx.Exec(`INSERT INTO cards (text, stage, waits_for) VALUES (?, ?, ?)`,
			text, to.Stage, to.WaitsFor)
		if err != nil {
			return Card{}, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return Card{}, err
		}
		card.ID = id
	} else {
		if card.State == to && end == nil {
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
	var run sql.NullString
	if end != nil {
		run = sql.NullString{String: end.id, Valid: true}
	}
	why := sql.NullString{String: reason, Valid: reason != ""}
	if _, err := tx.Exec(`INSERT INTO entries (card, at, cause, stage, waits_for, run, reason) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, at, cause, to.Stage, to.WaitsFor, run, why); err != nil {
		return Card{}, err
	}
	if err := tx.Commit(); err != nil {
		return Card{}, err
	}
	return card, nil
}

// fixed is the next state of a change to one given state, whatever the card's
// state is.
func fixed(to State) func(State, bool) (State, error) {
	return func(State, bool) (State, error) { return to, nil }
}

// Instance reads the instance's identity, the eight hex digits by which the
// things in the container engine are known to be this instance's.
func (s *Store) Instance() (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM instance`).Scan(&id)
	return id, err
}

// Waiting lists the cards that wait for a run, by ID.
func (s *Store) Waiting() ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM cards WHERE waits_for = ? ORDER BY id`, Run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// InFlight returns the card's run in flight, or nil when it has none.
func (s *Store) InFlight(card int64) (*RunRow, error) {
	r, err := scanRun(s.db.QueryRow(runSelect+` WHERE card = ? AND ending IS NULL`, card))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// RunCount counts the card's runs, in flight or ended.
func (s *Store) RunCount(card int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM runs WHERE card = ?`, card).Scan(&n)
	return n, err
}

// Run reads a run.
func (s *Store) Run(id string) (RunRow, error) {
	return readRun(s.db, id)
}

// AddRun writes a run in flight. The same row written again writes nothing;
// anything else under the same ID is refused, and so is a second run in flight
// for the card, which the store's unique index refuses, and a run of a card
// that does not wait for one, such as a card the developer dropped after the
// tick read it (card 16).
func (s *Store) AddRun(r RunRow) error {
	if r.ID == "" || r.Card == 0 || r.Commit == "" || r.Model == "" || r.Effort == "" {
		return fmt.Errorf("a run needs an ID, a card, a commit, a model and an effort: %+v", r)
	}
	if r.Ending != "" {
		return fmt.Errorf("run %s: a run is written in flight, not as %s", r.ID, r.Ending)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	have, err := readRun(tx, r.ID)
	switch {
	case err == nil && have == r:
		return nil
	case err == nil:
		return fmt.Errorf("run %s exists as %+v and is not %+v", r.ID, have, r)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	card, err := readCard(tx, r.Card)
	if err != nil {
		return err
	}
	if card.WaitsFor != Run {
		return fmt.Errorf("run %s not written: card %d is in %s waiting for %s, not for a run", r.ID, r.Card, card.Stage, card.WaitsFor)
	}
	if _, err := tx.Exec(`INSERT INTO runs (id, card, commit_id, model, effort, started) VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.Card, r.Commit, r.Model, r.Effort, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("run %s not written; card %d may have a run in flight already: %w", r.ID, r.Card, err)
	}
	return tx.Commit()
}

// EndRun ends a run in flight, finished or failed, and moves its card to the
// state to, in one transaction through change: the run's ending, the card's
// state, an entry with the cause run, the run's ID and the reason, and the
// run's document when it is given, are written together or not at all. A run
// that has ended already writes nothing, so a repeat after a kill is safe
// (ADR 0035), and a document is written once.
func (s *Store) EndRun(id string, ending Ending, reason string, to State, doc *Doc) (Card, error) {
	if ending != Finished && ending != Failed {
		return Card{}, fmt.Errorf("%q is not an ending of a run (ADR 0043)", ending)
	}
	if doc != nil && doc.Kind != KindSpec && doc.Kind != KindReport && doc.Kind != KindReview {
		return Card{}, fmt.Errorf("%q is not a kind of document", doc.Kind)
	}
	return s.change(false, 0, "", fixed(to), CauseRun, reason, &runEnd{id, ending, doc})
}

// Note reads the developer's note that a card's next run is told: the note on
// the entry that made the card wait for a run, when that entry is the
// developer's; "" otherwise.
func (s *Store) Note(card int64) (string, error) {
	var e Entry
	var reason sql.NullString
	err := s.db.QueryRow(`SELECT cause, stage, waits_for, reason FROM entries WHERE card = ? ORDER BY number DESC LIMIT 1`, card).
		Scan(&e.Cause, &e.Stage, &e.WaitsFor, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w %d", ErrNoCard, card)
	}
	if err != nil || e.WaitsFor != Run {
		return "", err
	}
	e.Reason = reason.String
	return e.Note(), nil
}

// Spec reads the newest spec of a card, the one written last, with its run's
// ID and commit.
func (s *Store) Spec(card int64) (SpecRow, error) {
	var sp SpecRow
	err := s.db.QueryRow(`SELECT specs.run, runs.commit_id, specs.text FROM specs
		JOIN runs ON runs.id = specs.run WHERE specs.card = ? AND specs.kind = ? ORDER BY specs.rowid DESC LIMIT 1`,
		card, KindSpec).
		Scan(&sp.Run, &sp.Commit, &sp.Text)
	if errors.Is(err, sql.ErrNoRows) {
		return SpecRow{}, fmt.Errorf("card %d has no spec: %w", card, err)
	}
	return sp, err
}

// Newest reads the newest document of a card among the kinds given, the one
// written last.
func (s *Store) Newest(card int64, kinds ...string) (Document, error) {
	if len(kinds) == 0 {
		return Document{}, errors.New("no kind of document is given")
	}
	query := `SELECT kind, run, text FROM specs WHERE card = ? AND kind IN (?` +
		strings.Repeat(", ?", len(kinds)-1) + `) ORDER BY rowid DESC LIMIT 1`
	args := []any{card}
	for _, k := range kinds {
		args = append(args, k)
	}
	var d Document
	err := s.db.QueryRow(query, args...).Scan(&d.Kind, &d.Run, &d.Text)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, fmt.Errorf("card %d has no document of the kinds %v: %w", card, kinds, err)
	}
	return d, err
}

// querier is a database or a transaction.
type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

const runSelect = `SELECT id, card, commit_id, model, effort, ending FROM runs`

func scanRun(row *sql.Row) (RunRow, error) {
	var r RunRow
	var ending sql.NullString
	err := row.Scan(&r.ID, &r.Card, &r.Commit, &r.Model, &r.Effort, &ending)
	r.Ending = Ending(ending.String)
	return r, err
}

func readRun(q querier, id string) (RunRow, error) {
	r, err := scanRun(q.QueryRow(runSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RunRow{}, fmt.Errorf("no run %s: %w", id, err)
	}
	return r, err
}

func readCard(q querier, id int64) (Card, error) {
	c := Card{ID: id}
	err := q.QueryRow(`SELECT text, stage, waits_for FROM cards WHERE id = ?`, id).
		Scan(&c.Text, &c.Stage, &c.WaitsFor)
	if errors.Is(err, sql.ErrNoRows) {
		return Card{}, fmt.Errorf("%w %d: %w", ErrNoCard, id, err)
	}
	return c, err
}
