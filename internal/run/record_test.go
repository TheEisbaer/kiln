package run

import (
	"archive/tar"
	"errors"
	"testing"
)

// TestOtherInstruction has a recorded run given again with another
// instruction: the refusal matches ErrOtherInstruction with the text it always
// had. A run given again with another commit is refused as before, and does
// not match it.
func TestOtherInstruction(t *testing.T) {
	r := &run{Inputs: Inputs{ID: "i-1-1", Branch: "kiln/card-1", Model: "m", Effort: "high"}, commit: "c1", sum: "s1"}
	recorded := r.recordInputs()

	other := recorded
	other.InstructionSHA256 = "s2"
	err := other.matches(r)
	if !errors.Is(err, ErrOtherInstruction) {
		t.Errorf("another instruction gives %v, which does not match ErrOtherInstruction", err)
	}
	if want := `the run i-1-1 is recorded with the instruction "s2", not "s1"; it is left alone`; err == nil || err.Error() != want {
		t.Errorf("another instruction gives %q, want %q", err, want)
	}

	other = recorded
	other.Commit = "c2"
	err = other.matches(r)
	if err == nil || errors.Is(err, ErrOtherInstruction) {
		t.Errorf("another commit gives %v", err)
	}
	if want := `the run i-1-1 is recorded with the commit "c2", not "c1"; it is left alone`; err == nil || err.Error() != want {
		t.Errorf("another commit gives %q, want %q", err, want)
	}

	if err := recorded.matches(r); err != nil {
		t.Errorf("the same inputs give %v", err)
	}
}

// TestKilledForMemory: a kill for memory fails a run unless it exited with 0
// with a success result and its document is whole.
func TestKilledForMemory(t *testing.T) {
	ok := &Result{Subtype: "success", NumTurns: 3, TotalCostUSD: 0.25}
	for _, c := range []struct {
		name     string
		exit     int
		oom      bool
		result   *Result
		document bool
		finished bool
		reason   string
	}{
		{"kept", 0, true, ok, true, true, "exit 0, result success, 3 turns, $0.2500, no change, a process in the run was killed for memory"},
		{"no document", 0, true, ok, false, false, "exit 0, killed for memory, result success, 3 turns, $0.2500, no change"},
		{"exit 137", 137, true, ok, true, false, "exit 137, killed for memory, result success, 3 turns, $0.2500, no change"},
		{"no result", 137, true, nil, true, false, "exit 137, killed for memory, no result, no change"},
		{"exit 0, no result", 0, true, nil, true, false, "exit 0, killed for memory, no result, no change"},
		{"is_error", 0, true, &Result{Subtype: "success", IsError: true, NumTurns: 3, TotalCostUSD: 0.25}, true, false,
			"exit 0, killed for memory, result is_error, 3 turns, $0.2500, no change"},
		{"not killed", 0, false, ok, false, true, "exit 0, result success, 3 turns, $0.2500, no change"},
	} {
		rec := &record{Inputs: RecordInputs{Model: "m"}, ModelSeen: "m", Ending: Ending{ExitCode: c.exit, OOMKilled: c.oom},
			Result: c.result, Saved: noChange}
		if got := rec.status(c.document) == 0; got != c.finished {
			t.Errorf("%s: finished is %v, want %v", c.name, got, c.finished)
		}
		if got := rec.reason(c.document); got != c.reason {
			t.Errorf("%s: the reason is %q, want %q", c.name, got, c.reason)
		}
	}
}

func TestDocumentWhole(t *testing.T) {
	spec := func(e entry) string { return writeRecord(t, makeTar(t, outDir, e)) }
	good := spec(entry{"out/spec.md", tar.TypeReg, "# Spec\n"})
	if !documentWhole(good, "r", "spec") {
		t.Error("a spec with text is not whole")
	}
	if documentWhole(good, "r", "review") {
		t.Error("a missing review is whole")
	}
	if documentWhole(spec(entry{"out/spec.md", tar.TypeReg, " \n\t"}), "r", "spec") {
		t.Error("a spec of white space is whole")
	}
	if documentWhole(spec(entry{"out/spec.md", tar.TypeDir, ""}), "r", "spec") {
		t.Error("a directory is whole")
	}
	if documentWhole(writeRecord(t, []byte("not a tar, not a tar, not a tar")), "r", "spec") {
		t.Error("a damaged archive is whole")
	}
	if documentWhole(good, "other", "spec") {
		t.Error("a run with no documents.tar is whole")
	}
}
