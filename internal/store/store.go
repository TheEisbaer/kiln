// Package store keeps a card, with its state and its history, in the one
// embedded database file of the instance (ADR 0032). A card's state and the
// entry that says what changed are written together, in one transaction, by
// one function (ADR 0042), and only a pairing of the closed lists that ADR 0043
// allows can be written.
package store

import (
	"cmp"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver" // registers the "sqlite3" driver
)

// version is the version of the store's shape, kept in the file's
// PRAGMA user_version. 0 is a file with nothing in it (ADR 0036).
const version = 10

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

// Card is a card as it is now. SendBacks is its number of send-backs since
// its spec was approved (ADR 0054): one for every review that went back and
// every return from the Git host because main moved.
type Card struct {
	ID   int64
	Text string
	State
	SendBacks int
}

// MaxSendBacks is the most send-backs the rule makes on a card; at that
// number the card waits for the developer instead (ADR 0015, ADR 0054).
const MaxSendBacks = 2

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

// Doc is a run's document as EndRun keeps it: its kind, KindSpec, KindReport,
// KindPullRequest or KindReview, and its text, exactly as the run wrote it.
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
	Note   string // the developer's note on approving this spec, "" if none
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
	func(tx *sql.Tx) error {
		// When a card got its state, so that the tick takes the card that has
		// waited longest first without reading the history (ADR 0042, rule
		// 3): for a card that was there, the time of its last entry, which
		// wrote that state.
		_, err := tx.Exec(`ALTER TABLE cards ADD COLUMN since TEXT;
UPDATE cards SET since = (SELECT at FROM entries WHERE entries.card = cards.id ORDER BY number DESC LIMIT 1);`)
		return err
	},
	func(tx *sql.Tx) error {
		// An implement run writes two documents, its report and the pull
		// request's text, so a document is kept by its run and its kind; the
		// rows keep their rowids, which give their order. And a card's
		// publication, one row per card the developer has published, written
		// with the moves of publishing (card 19).
		_, err := tx.Exec(`
ALTER TABLE specs RENAME TO old_specs;
CREATE TABLE specs (
	run TEXT NOT NULL,
	card INTEGER NOT NULL,
	text TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT 'spec',
	PRIMARY KEY (run, kind)
) STRICT;
INSERT INTO specs (rowid, run, card, text, kind) SELECT rowid, run, card, text, kind FROM old_specs;
DROP TABLE old_specs;
CREATE TABLE publishes (
	card INTEGER PRIMARY KEY,
	workflow_yes TEXT,
	commit_id TEXT,
	address TEXT,
	branch TEXT
) STRICT;`)
		return err
	},
	func(tx *sql.Tx) error {
		// The developer's note on approving a spec, kept on that spec's row;
		// NULL on every other row.
		_, err := tx.Exec(`ALTER TABLE specs ADD COLUMN note TEXT`)
		return err
	},
	func(tx *sql.Tx) error {
		// The instance's state, whether it is paused and why, and its history,
		// one entry per change, written together (ADR 0055).
		_, err := tx.Exec(`
ALTER TABLE instance ADD COLUMN paused TEXT;
CREATE TABLE instance_entries (
	number INTEGER PRIMARY KEY,
	at TEXT NOT NULL,
	paused TEXT
) STRICT;`)
		return err
	},
	func(tx *sql.Tx) error {
		// The card's number of send-backs since its spec was approved (ADR
		// 0054); every card that was there starts at 0.
		_, err := tx.Exec(`ALTER TABLE cards ADD COLUMN send_backs INTEGER NOT NULL DEFAULT 0`)
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
	return s.change(edit{create: true, text: text, next: fixed(State{Backlog, Developer}), cause: CauseDeveloper})
}

// Move changes a card's state. A pairing or a cause that cannot be written is
// refused. A card that already has the state is returned as it is, so that a
// repeated Move writes nothing (ADR 0035).
func (s *Store) Move(id int64, to State, cause Cause) (Card, error) {
	return s.change(edit{id: id, next: fixed(to), cause: cause})
}

// MoveWithReason moves a card that is in the state from, with a reason on the
// entry, for a card that the core stops without a run, such as a review run
// that is not started. A card that is no longer in from, such as one the
// developer dropped meanwhile, is refused with ErrRefused and returned as it
// is.
func (s *Store) MoveWithReason(id int64, from, to State, cause Cause, reason string) (Card, error) {
	return s.change(edit{id: id, next: from.only(to), cause: cause, reason: reason})
}

// MoveSentBack is MoveWithReason with the cause rule for a send-back that is
// not a review's, such as a card at the Git host whose change no longer
// applies: in the same transaction the card's number of send-backs goes up by
// one. A move to implement, waiting for a run, is refused with ErrRefused when
// the number is MaxSendBacks or more, and nothing is written.
func (s *Store) MoveSentBack(id int64, from, to State, reason string) (Card, error) {
	return s.change(edit{id: id, next: from.only(to), cause: CauseRule, reason: reason, sendBack: true})
}

// only is the next state of a change that is made only from the state s.
func (s State) only(to State) func(State, Facts) (State, error) {
	return func(have State, _ Facts) (State, error) {
		if have != s {
			return State{}, fmt.Errorf("%w: the card is in %s waiting for %s, not %s waiting for %s",
				ErrRefused, have.Stage, have.WaitsFor, s.Stage, s.WaitsFor)
		}
		return to, nil
	}
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
// new state, and the documents it wrote, if any.
type runEnd struct {
	id     string
	ending Ending
	docs   []Doc
}

// edit is one change to a card, as change writes it: a card created with the
// text when create is set, or else the card id; the next state, from the card's
// state as it is in the transaction, the zero State for a card that is
// created, and the facts the table of moves looks at; the cause and the reason
// on its entry; and, when end is set, the run whose ending is written with it.
// write, when it is set, writes more in the same transaction, after the
// card's row; always writes the entry also when the state stays as it is.
type edit struct {
	create bool
	id     int64
	text   string
	next   func(from State, f Facts) (State, error)
	cause  Cause
	reason string
	end    *runEnd
	write  func(tx *sql.Tx, card int64) error
	always bool
	// rule, when set with end, is a second step in the same transaction: the
	// card ends at rule.To, with a second entry of the cause rule and no run.
	rule *Rule
	// sendBack raises the card's number of send-backs by one, and refuses a
	// card that would end in implement, waiting for a run, at MaxSendBacks or
	// more; approved sets the number to 0 (ADR 0054).
	sendBack bool
	approved bool
}

// change is the one place every change to a card goes through (ADR 0042). In
// one transaction it checks the pairing and the cause, writes the card's row
// and then the entry, and commits. An error of next is returned and nothing is
// written. When end is set, the card is the one of that run, and the run's
// ending is written in the same transaction, with the run on the entry, and
// with the run's documents; a run that has ended already writes nothing, its
// documents included. A change to the state the card has writes nothing
// either, unless it ends a run or is always written.
func (s *Store) change(e edit) (Card, error) {
	if !causes[e.cause] {
		return Card{}, fmt.Errorf("%q is not a cause of a change (ADR 0042)", e.cause)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Card{}, err
	}
	defer tx.Rollback()

	id := e.id
	if e.end != nil {
		run, err := readRun(tx, e.end.id)
		if err != nil {
			return Card{}, err
		}
		if run.Ending != "" {
			return readCard(tx, run.Card)
		}
		id = run.Card
	}

	var card Card
	var f Facts
	if e.create {
		card = Card{Text: e.text}
	} else if card, err = readCard(tx, id); err != nil {
		return Card{}, err
	} else if f, err = readFacts(tx, id); err != nil {
		return Card{}, err
	}
	to, err := e.next(card.State, f)
	if err != nil {
		return card, err
	}
	if !valid[to] {
		return Card{}, fmt.Errorf("a card cannot be in %s waiting for %s (ADR 0043)", to.Stage, to.WaitsFor)
	}

	if e.end != nil {
		if _, err := tx.Exec(`UPDATE runs SET ending = ? WHERE id = ?`, e.end.ending, e.end.id); err != nil {
			return Card{}, err
		}
		for _, d := range e.end.docs {
			if _, err := tx.Exec(`INSERT INTO specs (run, card, text, kind) VALUES (?, ?, ?, ?)`,
				e.end.id, id, d.Text, d.Kind); err != nil {
				return Card{}, err
			}
		}
	}

	// The card's row and its entries carry the same time: since is when the
	// card got the state it has.
	final := to
	if e.rule != nil {
		final = e.rule.To
	}
	if e.sendBack && final == (State{Implement, Run}) && card.SendBacks >= MaxSendBacks {
		return card, fmt.Errorf("%w: the card was sent back %d times already, and the rule sends a change back %d times at most",
			ErrRefused, card.SendBacks, MaxSendBacks)
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if e.create {
		res, err := tx.Exec(`INSERT INTO cards (text, stage, waits_for, since) VALUES (?, ?, ?, ?)`,
			e.text, to.Stage, to.WaitsFor, at)
		if err != nil {
			return Card{}, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return Card{}, err
		}
		card.ID = id
	} else {
		if card.State == to && e.end == nil && !e.always {
			return card, nil
		}
		if _, err := tx.Exec(`UPDATE cards SET stage = ?, waits_for = ?, since = ? WHERE id = ?`,
			final.Stage, final.WaitsFor, at, id); err != nil {
			return Card{}, err
		}
	}
	card.State = final
	switch {
	case e.sendBack:
		if _, err := tx.Exec(`UPDATE cards SET send_backs = send_backs + 1 WHERE id = ?`, id); err != nil {
			return Card{}, err
		}
		card.SendBacks++
	case e.approved:
		if _, err := tx.Exec(`UPDATE cards SET send_backs = 0 WHERE id = ?`, id); err != nil {
			return Card{}, err
		}
		card.SendBacks = 0
	}
	if e.write != nil {
		if err := e.write(tx, id); err != nil {
			return Card{}, err
		}
	}

	if afterCardWritten != nil {
		if err := afterCardWritten(); err != nil {
			return Card{}, err
		}
	}
	var run sql.NullString
	if e.end != nil {
		run = sql.NullString{String: e.end.id, Valid: true}
	}
	why := sql.NullString{String: e.reason, Valid: e.reason != ""}
	if _, err := tx.Exec(`INSERT INTO entries (card, at, cause, stage, waits_for, run, reason) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, at, e.cause, to.Stage, to.WaitsFor, run, why); err != nil {
		return Card{}, err
	}
	if e.rule != nil {
		if _, err := tx.Exec(`INSERT INTO entries (card, at, cause, stage, waits_for, run, reason) VALUES (?, ?, ?, ?, ?, NULL, ?)`,
			id, at, CauseRule, final.Stage, final.WaitsFor, e.rule.Reason); err != nil {
			return Card{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Card{}, err
	}
	return card, nil
}

// fixed is the next state of a change to one given state, whatever the card's
// state is.
func fixed(to State) func(State, Facts) (State, error) {
	return func(State, Facts) (State, error) { return to, nil }
}

// Instance reads the instance's identity, the eight hex digits by which the
// things in the container engine are known to be this instance's.
func (s *Store) Instance() (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM instance`).Scan(&id)
	return id, err
}

// furthest ranks the stages that wait for a run, the one furthest along first.
var furthest = map[Stage]int{Review: 0, Implement: 1, Spec: 2}

// Waiting lists the cards that wait for a run, in the order the tick takes
// them: the one furthest along first, review, then implement, then spec, so
// that cards reach the developer before new ones begin; among those of a
// stage, the one that has waited longest; and among those, by ID.
func (s *Store) Waiting() ([]int64, error) {
	type waiting struct {
		id    int64
		rank  int
		since time.Time
	}
	rows, err := s.db.Query(`SELECT id, stage, since FROM cards WHERE waits_for = ?`, Run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cards []waiting
	for rows.Next() {
		var w waiting
		var stage Stage
		var since string
		if err := rows.Scan(&w.id, &stage, &since); err != nil {
			return nil, err
		}
		w.rank = furthest[stage]
		if w.since, err = time.Parse(time.RFC3339Nano, since); err != nil {
			return nil, fmt.Errorf("card %d: %w", w.id, err)
		}
		cards = append(cards, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(cards, func(a, b waiting) int {
		return cmp.Or(cmp.Compare(a.rank, b.rank), a.since.Compare(b.since), cmp.Compare(a.id, b.id))
	})
	ids := make([]int64, len(cards))
	for i, w := range cards {
		ids[i] = w.id
	}
	return ids, nil
}

// InFlightRuns lists every run in flight, by its card.
func (s *Store) InFlightRuns() (map[int64]string, error) {
	rows, err := s.db.Query(`SELECT card, id FROM runs WHERE ending IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := map[int64]string{}
	for rows.Next() {
		var card int64
		var id string
		if err := rows.Scan(&card, &id); err != nil {
			return nil, err
		}
		runs[card] = id
	}
	return runs, rows.Err()
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

// AddRun writes a run in flight, while fewer than limit runs are in flight.
// The same row written again writes nothing; anything else under the same ID
// is refused, and so is a second run in flight for the card, which the store's
// unique index refuses, a run of a card that does not wait for one, such as a
// card the developer dropped after the tick read it (card 16), and a run that
// would make more than limit runs in flight (card 17). The tick counts before
// it starts a run, and the store refuses as well, as it does a second run in
// flight per card.
func (s *Store) AddRun(r RunRow, limit int) error {
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
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM runs WHERE ending IS NULL`).Scan(&n); err != nil {
		return err
	}
	if n >= limit {
		return fmt.Errorf("%w: run %s not written: %d runs are in flight, and the limit is %d", ErrLimit, r.ID, n, limit)
	}
	if _, err := tx.Exec(`INSERT INTO runs (id, card, commit_id, model, effort, started) VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.Card, r.Commit, r.Model, r.Effort, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("run %s not written; card %d may have a run in flight already: %w", r.ID, r.Card, err)
	}
	return tx.Commit()
}

// ErrLimit is AddRun's error for a run that would make more runs in flight
// than its limit.
var ErrLimit = errors.New("the limit of runs in flight is reached")

// EndRun ends a run in flight, finished or failed, and moves its card to the
// state to, in one transaction through change: the run's ending, the card's
// state, an entry with the cause run, the run's ID and the reason, and the
// run's documents that are given, nil ones left out, are written together or
// not at all. A run that has ended already writes nothing, so a repeat after a
// kill is safe (ADR 0035), and a document is written once. A run writes at
// most one document of each kind.
func (s *Store) EndRun(id string, ending Ending, reason string, to State, docs ...*Doc) (Card, error) {
	if ending != Finished && ending != Failed {
		return Card{}, fmt.Errorf("%q is not an ending of a run (ADR 0043)", ending)
	}
	kept, err := keepDocs(id, docs)
	if err != nil {
		return Card{}, err
	}
	return s.change(edit{next: fixed(to), cause: CauseRun, reason: reason, end: &runEnd{id, ending, kept}})
}

// keepDocs checks a run's documents, nil ones left out.
func keepDocs(id string, docs []*Doc) ([]Doc, error) {
	var kept []Doc
	seen := map[string]bool{}
	for _, d := range docs {
		if d == nil {
			continue
		}
		if !kinds[d.Kind] {
			return nil, fmt.Errorf("%q is not a kind of document", d.Kind)
		}
		if seen[d.Kind] {
			return nil, fmt.Errorf("run %s: two documents of the kind %s", id, d.Kind)
		}
		seen[d.Kind] = true
		kept = append(kept, *d)
	}
	return kept, nil
}

// Rule is the step of the rule that follows a run's end: the state the card
// goes on to, the reason on the rule's entry, whether the card's
// publication is written, as the move publish writes it without a yes, and
// whether it is a send-back, which raises the card's number of send-backs by
// one (ADR 0054).
type Rule struct {
	To       State
	Reason   string
	Publish  bool
	SendBack bool
}

// EndRunByRule does what EndRun does, and in the same transaction of change
// writes a second entry, of the cause rule and with no run, at rule.To with
// rule.Reason; the card's row ends at rule.To. With rule.Publish the card's
// publication is written anew, with no yes, no commit and no address, and its
// branch kept. A rule with no reason, or a state that is not a valid pairing,
// is refused before anything is written. With rule.SendBack the card's number
// of send-backs goes up by one in the same transaction; a send-back to
// implement, waiting for a run, of a card at MaxSendBacks or more is refused
// with ErrRefused, and so is a send-back that publishes, and nothing is
// written. A run that has ended already writes nothing.
func (s *Store) EndRunByRule(id string, ending Ending, reason string, to State, rule Rule, docs ...*Doc) (Card, error) {
	if ending != Finished && ending != Failed {
		return Card{}, fmt.Errorf("%q is not an ending of a run (ADR 0043)", ending)
	}
	if rule.Reason == "" {
		return Card{}, errors.New("a step of the rule needs a reason")
	}
	if !valid[rule.To] {
		return Card{}, fmt.Errorf("a card cannot be in %s waiting for %s (ADR 0043)", rule.To.Stage, rule.To.WaitsFor)
	}
	if rule.SendBack && rule.Publish {
		return Card{}, fmt.Errorf("%w: a send-back does not publish", ErrRefused)
	}
	kept, err := keepDocs(id, docs)
	if err != nil {
		return Card{}, err
	}
	e := edit{next: fixed(to), cause: CauseRun, reason: reason, end: &runEnd{id, ending, kept}, rule: &rule, sendBack: rule.SendBack}
	if rule.Publish {
		e.write = func(tx *sql.Tx, card int64) error { return writePublication(tx, card, "") }
	}
	return s.change(e)
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
	var note sql.NullString
	err := s.db.QueryRow(`SELECT specs.run, runs.commit_id, specs.text, specs.note FROM specs
		JOIN runs ON runs.id = specs.run WHERE specs.card = ? AND specs.kind = ? ORDER BY specs.rowid DESC LIMIT 1`,
		card, KindSpec).
		Scan(&sp.Run, &sp.Commit, &sp.Text, &note)
	if errors.Is(err, sql.ErrNoRows) {
		return SpecRow{}, fmt.Errorf("card %d has no spec: %w", card, err)
	}
	sp.Note = note.String
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
	err := q.QueryRow(`SELECT text, stage, waits_for, send_backs FROM cards WHERE id = ?`, id).
		Scan(&c.Text, &c.Stage, &c.WaitsFor, &c.SendBacks)
	if errors.Is(err, sql.ErrNoRows) {
		return Card{}, fmt.Errorf("%w %d: %w", ErrNoCard, id, err)
	}
	return c, err
}
