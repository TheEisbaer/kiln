package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var (
	passed    = State{Review, Approval}
	atTheHost = State{Review, Host}
	needsYou  = State{Review, Developer}
	done      = State{Done, Nothing}
)

// TestPublishMoves follows publish row by row: refused without a publish
// command, from a review that passed, with and without the yes; no move while
// the core publishes; the moves at the Git host once the address is kept;
// publish again after a failure, and only then from "needs you".
func TestPublishMoves(t *testing.T) {
	s := open(t)
	with := Options{Publishing: true}
	card := mustCreate(t, s, "a card")
	if _, err := s.Move(card.ID, passed, CauseRun); err != nil {
		t.Fatal(err)
	}
	moves := func(publishing bool) []Allowed {
		t.Helper()
		c, err := s.Card(card.ID)
		if err != nil {
			t.Fatal(err)
		}
		m, err := s.Moves(c, publishing)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	// Without a publish command there is no publish.
	if want := []Allowed{{MarkDone, NoNote}, {SendBack, RequiredNote}, {Drop, OptionalNote}}; !reflect.DeepEqual(moves(false), want) {
		t.Errorf("the moves of a review that passed, without a publish command: %v, want %v", moves(false), want)
	}
	if got, err := s.Decide(card.ID, Publish, ""); !errors.Is(err, ErrRefused) || got.State != passed {
		t.Errorf("publish without a publish command: %+v, %v", got, err)
	}
	// With one, publish comes first, and takes no note.
	if want := []Allowed{{Publish, NoNote}, {MarkDone, NoNote}, {SendBack, RequiredNote}, {Drop, OptionalNote}}; !reflect.DeepEqual(moves(true), want) {
		t.Errorf("the moves of a review that passed, with a publish command: %v, want %v", moves(true), want)
	}
	if got, err := s.DecideWith(card.ID, Publish, "a note", with); !errors.Is(err, ErrNote) || got.State != passed {
		t.Errorf("publish with a note: %+v, %v", got, err)
	}
	got, err := s.DecideWith(card.ID, Publish, "", Options{Publishing: true, WorkflowYes: "c0ffee"})
	if err != nil || got.State != atTheHost {
		t.Fatalf("publish: %+v, %v", got, err)
	}
	h := mustHistory(t, s, card.ID)
	if e := h[len(h)-1]; e.Cause != CauseDeveloper || e.State != atTheHost || e.Reason != "" {
		t.Errorf("the entry of publish: %+v", e)
	}
	if p, err := s.Publication(card.ID); err != nil || p != (Publication{WorkflowYes: "c0ffee"}) {
		t.Errorf("the publication after publish: %+v, %v", p, err)
	}
	if ids, err := s.Publishing(); err != nil || !reflect.DeepEqual(ids, []int64{card.ID}) {
		t.Errorf("the cards the core publishes: %v, %v", ids, err)
	}

	// While the core publishes, the card has no move, drop included.
	if m := moves(true); len(m) != 0 {
		t.Errorf("the moves while the core publishes: %v", m)
	}
	for _, action := range []Action{MarkDone, SendBack, Drop, Publish} {
		if got, err := s.DecideWith(card.ID, action, "a note", with); !errors.Is(err, ErrRefused) || got.State != atTheHost {
			t.Errorf("%s while the core publishes: %+v, %v", action, got, err)
		}
	}
	// A core without the command lets the developer take the card back.
	if want := []Allowed{{MarkDone, NoNote}, {SendBack, RequiredNote}, {Drop, OptionalNote}}; !reflect.DeepEqual(moves(false), want) {
		t.Errorf("the moves at the Git host without a publish command: %v", moves(false))
	}

	// The address is kept with an entry of its own, and the card stays.
	got, err = s.KeepPullRequest(card.ID, "beef", "https://git.example/pull/7")
	if err != nil || got.State != atTheHost {
		t.Fatalf("KeepPullRequest: %+v, %v", got, err)
	}
	h = mustHistory(t, s, card.ID)
	if e := h[len(h)-1]; e.Cause != CauseRule || e.State != atTheHost || e.Reason != "published as https://git.example/pull/7" || e.Note() != "" {
		t.Errorf("the entry of the address: %+v", e)
	}
	if p, _ := s.Publication(card.ID); p != (Publication{WorkflowYes: "c0ffee", Commit: "beef", Address: "https://git.example/pull/7"}) {
		t.Errorf("the publication: %+v", p)
	}
	if _, err := s.KeepPullRequest(card.ID, "beef", "https://git.example/pull/8"); !errors.Is(err, ErrRefused) {
		t.Errorf("a second address: %v", err)
	}
	if at, err := s.AtHost(); err != nil || !reflect.DeepEqual(at, map[int64]string{card.ID: "beef"}) {
		t.Errorf("AtHost: %v, %v", at, err)
	}
	if ids, _ := s.Publishing(); len(ids) != 0 {
		t.Errorf("the cards the core publishes: %v", ids)
	}
	if want := []Allowed{{MarkDone, NoNote}, {SendBack, RequiredNote}, {Drop, OptionalNote}}; !reflect.DeepEqual(moves(true), want) {
		t.Errorf("the moves at the Git host: %v, want %v", moves(true), want)
	}
	if got, err := s.DecideWith(card.ID, SendBack, "", with); !errors.Is(err, ErrNote) || got.State != atTheHost {
		t.Errorf("send-back from the Git host without a note: %+v, %v", got, err)
	}

	// Sent back, reviewed again, published again: the publication starts
	// anew, without the earlier yes.
	if _, err := s.DecideWith(card.ID, SendBack, "Rename it.", with); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(card.ID, passed, CauseRun); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideWith(card.ID, Publish, "", with); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Publication(card.ID); p != (Publication{}) {
		t.Errorf("the publication after a second publish: %+v", p)
	}
	// The branch on the Git host is named once and kept, also when the
	// card is published anew, so that it has one pull request.
	if b, err := s.PublishBranch(card.ID, "card/1-a-card"); err != nil || b != "card/1-a-card" {
		t.Errorf("the first branch: %q, %v", b, err)
	}
	if b, err := s.PublishBranch(card.ID, "card/1-another-title"); err != nil || b != "card/1-a-card" {
		t.Errorf("the branch named again: %q, %v", b, err)
	}
	if _, err := s.PublishBranch(other0(t, s), "card/9-x"); err == nil {
		t.Error("a branch for a card never published was kept")
	}

	// A publishing that failed leaves the card under "needs you", from where
	// it is published again; a review that went back offers no publish.
	if _, err := s.MoveWithReason(card.ID, atTheHost, needsYou, CauseRule, "publishing failed: exit status 1"); err != nil {
		t.Fatal(err)
	}
	if m := moves(true); len(m) == 0 || !reflect.DeepEqual(m[2], Allowed{Publish, NoNote}) {
		t.Errorf("the moves after a publishing that failed: %v", m)
	}
	for _, m := range moves(false) {
		if m.Action == Publish {
			t.Error("publish after a failure without a publish command")
		}
	}
	if got, err := s.DecideWith(card.ID, Publish, "", with); err != nil || got.State != atTheHost {
		t.Errorf("publish again: %+v, %v", got, err)
	}
	other := mustCreate(t, s, "another card")
	if _, err := s.Move(other.ID, State{Review, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(other.ID, "r1"))
	if _, err := s.EndRun("r1", Finished, "the review sends the change back; exit 0", needsYou, Cost{}, &Doc{KindReview, "Verdict: goes back\n"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DecideWith(other.ID, Publish, "", with); !errors.Is(err, ErrRefused) || got.State != needsYou {
		t.Errorf("publish after a review that went back: %+v, %v", got, err)
	}

	// Merged: done, by the Git host, told apart on the board.
	if _, err := s.KeepPullRequest(card.ID, "f00d", "https://git.example/pull/7"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveWithReason(card.ID, atTheHost, done, CauseHost, MergedReason); err != nil {
		t.Fatal(err)
	}
	b, err := s.Board()
	if err != nil {
		t.Fatal(err)
	}
	if !b[0].Merged || b[0].Dropped || b[1].Merged {
		t.Errorf("the board: %+v", b)
	}
	if p, _ := s.Publication(card.ID); p.Address != "https://git.example/pull/7" || p.Commit != "f00d" || p.Branch != "card/1-a-card" {
		t.Errorf("the publication of a merged card: %+v", p)
	}
}

// TestPublishFailedAndMerged checks the two marks read off a card's last entry.
func TestPublishFailedAndMerged(t *testing.T) {
	for _, tc := range []struct {
		s              State
		cause          Cause
		reason         string
		failed, merged bool
	}{
		{needsYou, CauseRule, "publishing failed: the publish command exited with status 1", true, false},
		{needsYou, CauseRun, "publishing failed: x", false, false},
		{needsYou, CauseRule, "the change is 1.3 MiB as a diff", false, false},
		{atTheHost, CauseRule, "publishing failed: x", false, false},
		{done, CauseHost, "merged", false, true},
		{done, CauseRule, "merged", false, false},
		{done, CauseHost, "merged, mostly", false, false},
		{done, CauseDeveloper, "", false, false},
	} {
		if got := PublishFailed(tc.s, tc.cause, tc.reason); got != tc.failed {
			t.Errorf("PublishFailed(%v, %s, %q) = %v", tc.s, tc.cause, tc.reason, got)
		}
		if got := Merged(tc.s, tc.cause, tc.reason); got != tc.merged {
			t.Errorf("Merged(%v, %s, %q) = %v", tc.s, tc.cause, tc.reason, got)
		}
	}
}

// TestTwoDocuments keeps an implement run's report and its pull request's text
// together, refuses two documents of one kind, and keeps nothing of a refused
// end.
func TestTwoDocuments(t *testing.T) {
	s := open(t)
	card := mustCreate(t, s, "a card")
	if _, err := s.Move(card.ID, State{Implement, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(card.ID, "r1"))
	if _, err := s.EndRun("r1", Finished, "", State{Review, Run}, Cost{}, &Doc{KindReport, "a"}, &Doc{KindReport, "b"}); err == nil {
		t.Error("two reports of one run were kept")
	}
	if _, err := s.EndRun("r1", Finished, "", State{Review, Run}, Cost{}, &Doc{KindReport, "# Report"}, nil, &Doc{KindPullRequest, "The text."}); err != nil {
		t.Fatal(err)
	}
	want := []Document{{KindReport, "r1", "# Report", ""}, {KindPullRequest, "r1", "The text.", ""}}
	if docs, err := s.Documents(card.ID); err != nil || !reflect.DeepEqual(docs, want) {
		t.Errorf("the documents: %+v, %v", docs, err)
	}
	if d, err := s.Newest(card.ID, KindPullRequest); err != nil || d != want[1] {
		t.Errorf("the newest text: %+v, %v", d, err)
	}
}

// schemaV6 is the store of version 6 as card 17's code makes it: a card with a
// spec and a report, one document per run.
const schemaV6 = `
CREATE TABLE cards (id INTEGER PRIMARY KEY, text TEXT NOT NULL, stage TEXT NOT NULL, waits_for TEXT NOT NULL, since TEXT) STRICT;
CREATE TABLE entries (number INTEGER PRIMARY KEY, card INTEGER NOT NULL, at TEXT NOT NULL, cause TEXT NOT NULL, stage TEXT NOT NULL, waits_for TEXT NOT NULL, run TEXT, reason TEXT) STRICT;
CREATE TABLE instance (id TEXT NOT NULL) STRICT;
CREATE TABLE runs (id TEXT PRIMARY KEY, card INTEGER NOT NULL, commit_id TEXT NOT NULL, model TEXT NOT NULL, effort TEXT NOT NULL, ending TEXT, started TEXT) STRICT;
CREATE UNIQUE INDEX runs_in_flight ON runs(card) WHERE ending IS NULL;
CREATE TABLE specs (run TEXT PRIMARY KEY, card INTEGER NOT NULL, text TEXT NOT NULL, kind TEXT NOT NULL DEFAULT 'spec') STRICT;
PRAGMA user_version = 6;
INSERT INTO instance (id) VALUES ('ab12cd34');
INSERT INTO cards (text, stage, waits_for, since) VALUES ('a card', 'review', 'run', '2026-11-20T10:00:02Z');
INSERT INTO runs (id, card, commit_id, model, effort, ending) VALUES
	('ab12cd34-1-1', 1, 'c0ffee', 'm', 'high', 'finished'), ('ab12cd34-1-2', 1, 'c0ffee', 'm', 'medium', 'finished');
INSERT INTO specs (rowid, run, card, text, kind) VALUES (2, 'ab12cd34-1-2', 1, '# Report', 'report');
INSERT INTO specs (rowid, run, card, text, kind) VALUES (1, 'ab12cd34-1-1', 1, '# Spec', 'spec');
`

// TestMigrateV6 moves a store of version 6 forward: its documents are kept in
// their order, and an implement run can then keep two.
func TestMigrateV6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV6); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s := reopen(t, path)
	if v := userVersion(t, s); v != version {
		t.Fatalf("the store is version %d, want %d", v, version)
	}
	want := []Document{{KindSpec, "ab12cd34-1-1", "# Spec", ""}, {KindReport, "ab12cd34-1-2", "# Report", ""}}
	if docs, err := s.Documents(1); err != nil || !reflect.DeepEqual(docs, want) {
		t.Errorf("the documents after the move forward: %+v, %v", docs, err)
	}
	if p, err := s.Publication(1); err != nil || p != (Publication{}) {
		t.Errorf("the publication of an old card: %+v, %v", p, err)
	}
	// The tables of a new store, by their definitions with the white space
	// ignored; this test's version 6 is written by hand, with its own.
	squeeze := func(m map[string]string) map[string]string {
		for k, v := range m {
			m[k] = strings.NewReplacer(" ", "", "\t", "").Replace(v)
		}
		return m
	}
	if got, want := squeeze(shape(t, s)), squeeze(shape(t, open(t))); !reflect.DeepEqual(got, want) {
		t.Errorf("a migrated store differs from a new one:\n%v\n%v", got, want)
	}
	// An implement run in flight ends with two documents.
	if _, err := s.Move(1, State{Implement, Run}, CauseDeveloper); err != nil {
		t.Fatal(err)
	}
	mustAddRun(t, s, theRun(1, "ab12cd34-1-3"))
	if _, err := s.EndRun("ab12cd34-1-3", Finished, "", State{Review, Run}, Cost{}, &Doc{KindReport, "r"}, &Doc{KindPullRequest, "p"}); err != nil {
		t.Fatal(err)
	}
	if docs, _ := s.Documents(1); len(docs) != 4 {
		t.Errorf("the documents: %+v", docs)
	}
}

// other0 is a card that was never published.
func other0(t *testing.T, s *Store) int64 {
	t.Helper()
	return mustCreate(t, s, "never published").ID
}
