package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/TheEisbaer/kiln/internal/seal"
)

// noChange is the record's save when the run changed nothing.
const noChange = "no change"

// record is result.json, the record of one run in <runs>/<id>/.
type record struct {
	Inputs    RecordInputs `json:"inputs"`
	Ending    Ending       `json:"ending"`
	ModelSeen string       `json:"model_seen"` // the model system/init named
	Result    *Result      `json:"result"`     // the last result event, or none
	// Lines of output.jsonl longer than maxLine, which were not read as events:
	// with one, a missing result may have been too long.
	TooLongLines int    `json:"too_long_lines"`
	Saved        string `json:"saved"` // the saved commit, or "no change"
	// The run's list of hosts, from its container's label kiln.hosts; empty
	// without a list. wayout.jsonl holds what its way out let through and
	// refused, unless WayOut is "gone": the way out was not there at collect.
	Hosts  []string `json:"hosts"`
	WayOut string   `json:"wayout,omitempty"`
}

// wayOutGone is the record's WayOut for a run with a list that had no way out
// when it was collected.
const wayOutGone = "gone"

// RecordInputs are the inputs a run was started with.
type RecordInputs struct {
	Run               string `json:"run"`
	Repo              string `json:"repo"`
	Commit            string `json:"commit"`
	Branch            string `json:"branch"`
	Instruction       string `json:"instruction"`
	InstructionSHA256 string `json:"instruction_sha256"`
	Model             string `json:"model"`
	Effort            string `json:"effort"` // as given: Claude Code's output does not show it
	Image             string `json:"image"`
	Proxy             string `json:"proxy"`
}

// Ending is how the run's container ended.
type Ending struct {
	Status     string    `json:"status"`
	ExitCode   int       `json:"exit_code"`
	OOMKilled  bool      `json:"oom_killed"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

func endingOf(s *seal.RunState) Ending {
	return Ending{Status: s.Status, ExitCode: s.ExitCode, OOMKilled: s.OOMKilled,
		StartedAt: s.StartedAt, FinishedAt: s.FinishedAt}
}

func (r *run) recordInputs() RecordInputs {
	return RecordInputs{Run: r.ID, Repo: r.Repo, Commit: r.commit, Branch: r.Branch,
		Instruction: r.Instruction, InstructionSHA256: r.sum, Model: r.Model, Effort: r.Effort,
		Image: r.Image, Proxy: r.Proxy}
}

// matches refuses a run recorded with other inputs than those it is started
// with again: the commit, the branch, the model, the effort and the
// instruction, as on the container's labels.
func (in RecordInputs) matches(r *run) error {
	want := r.recordInputs()
	for _, f := range []struct{ what, got, want string }{
		{"commit", in.Commit, want.Commit},
		{"branch", in.Branch, want.Branch},
		{"model", in.Model, want.Model},
		{"effort", in.Effort, want.Effort},
		{"instruction", in.InstructionSHA256, want.InstructionSHA256},
	} {
		if f.got != f.want {
			return fmt.Errorf("the run %s is recorded with the %s %q, not %q; it is left alone", r.ID, f.what, f.got, f.want)
		}
	}
	return nil
}

// status is the command's exit status for a recorded run: 0 only when the
// container exited with 0, the result is success, and the model matched. The
// work is saved, or nothing changed, or there would be no record.
func (rec *record) status() int {
	if rec.Ending.ExitCode == 0 && rec.Result != nil && rec.Result.Subtype == "success" &&
		rec.ModelSeen == rec.Inputs.Model && rec.Saved != "" {
		return 0
	}
	return exitFailed
}

// reason is the verdict's line: the container's ending, the result's subtype,
// turns and cost, and the saved commit or "no change".
func (rec *record) reason() string {
	ending := fmt.Sprintf("exit %d", rec.Ending.ExitCode)
	if rec.Ending.OOMKilled {
		ending += ", killed for memory"
	}
	result := "no result"
	if rec.Result != nil {
		result = fmt.Sprintf("result %s, %d turns, $%.4f", printable(rec.Result.Subtype), rec.Result.NumTurns, rec.Result.TotalCostUSD)
	}
	saved := rec.Saved
	if saved != noChange {
		saved = "saved " + saved
	}
	return ending + ", " + result + ", " + saved
}

// partialDir is where a run is collected before it is recorded.
func partialDir(runs, id string) string { return filepath.Join(runs, id+".partial") }

// readRecord reads <runs>/<id>/result.json. It returns nil when there is no
// <runs>/<id>, and refuses one without a readable result.json.
func readRecord(runs, id string) (*record, error) {
	dir := filepath.Join(runs, id)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return nil, fmt.Errorf("%s has no readable result.json; it is left alone: %w", dir, err)
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("%s has no readable result.json; it is left alone: %w", dir, err)
	}
	return &rec, nil
}

// record is step 7: it writes result.json into .partial, syncs it, and renames
// the directory to <runs>/<id>.
func (r *run) record(rec *record) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	partial := partialDir(r.Runs, r.ID)
	if err := writeFile(filepath.Join(partial, "result.json"), func(w io.Writer) error {
		_, err := w.Write(append(data, '\n'))
		return err
	}); err != nil {
		return err
	}
	if err := syncDir(partial); err != nil {
		return err
	}
	if err := os.Rename(partial, filepath.Join(r.Runs, r.ID)); err != nil {
		return err
	}
	if err := syncDir(r.Runs); err != nil {
		return err
	}
	return after("record")
}

// writeFile writes a new file from what write gives it, and syncs it.
func writeFile(name string, write func(io.Writer) error) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	err = write(f)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// writeOutput writes a run's output and its standard error to output.jsonl and
// stderr.txt in dir, and syncs both.
func writeOutput(dir string, read func(stdout, stderr io.Writer) error) error {
	return writeFile(filepath.Join(dir, "output.jsonl"), func(stdout io.Writer) error {
		return writeFile(filepath.Join(dir, "stderr.txt"), func(stderr io.Writer) error {
			return read(stdout, stderr)
		})
	})
}

// syncDir syncs a directory, so that what was created or renamed in it stays.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
