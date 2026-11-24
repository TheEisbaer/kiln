package run

import (
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
