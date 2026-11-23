package run

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/instructions"
)

// entry is one entry of a tar the test makes.
type entry struct {
	name string
	typ  byte
	data string
}

// makeTar makes a tar as `docker cp <container>:/kiln/out -` hands it over.
func makeTar(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Uid: 1000, Gid: 1000}
		switch e.typ {
		case tar.TypeReg:
			h.Size = int64(len(e.data))
		case tar.TypeSymlink:
			h.Linkname = e.data
		case tar.TypeDir:
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeRecord writes documents.tar into a new record of the run "r", and
// returns the directory of the runs' records.
func writeRecord(t *testing.T, documents []byte) string {
	t.Helper()
	runs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runs, "r"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runs, "r", "documents.tar"), documents, 0o644); err != nil {
		t.Fatal(err)
	}
	return runs
}

var outDir = entry{name: "out/", typ: tar.TypeDir}

func TestReadSpec(t *testing.T) {
	spec := "# Spec\n\nNot UTF-8: \xff, an escape: \x1b[2J.\n"
	atLimit := strings.Repeat("x", maxSpec)
	for _, tc := range []struct {
		what      string
		documents []byte
		spec      string
		problem   string
	}{
		{"a spec", makeTar(t, outDir, entry{"out/spec.md", tar.TypeReg, spec}), spec, ""},
		{"the first of two", makeTar(t, outDir, entry{"out/other.md", tar.TypeReg, "other"},
			entry{"out/spec.md", tar.TypeReg, spec}, entry{"out/spec.md", tar.TypeReg, "second"}), spec, ""},
		{"262144 bytes", makeTar(t, outDir, entry{"out/spec.md", tar.TypeReg, atLimit}), atLimit, ""},
		{"262145 bytes", makeTar(t, outDir, entry{"out/spec.md", tar.TypeReg, atLimit + "x"}), "",
			"the spec is 262145 bytes, more than 262144"},
		// Nothing of a spec over the limit is read: its header alone, with no
		// data after it, gives the same problem.
		{"a header over the limit, and no data", makeTar(t, outDir, entry{"out/spec.md", tar.TypeReg, atLimit + "x"})[:1024], "",
			"the spec is 262145 bytes, more than 262144"},
		{"no spec", makeTar(t, outDir, entry{"out/notes.md", tar.TypeReg, spec}), "",
			"the run wrote no spec (/kiln/out/spec.md)"},
		{"an empty documents.tar", nil, "", "the run wrote no spec (/kiln/out/spec.md)"},
		{"a tar of zero blocks", make([]byte, 1024), "", "the run wrote no spec (/kiln/out/spec.md)"},
		{"a symlink", makeTar(t, outDir, entry{"out/spec.md", tar.TypeSymlink, "/etc/passwd"}), "",
			"/kiln/out/spec.md is not a file"},
		{"a directory", makeTar(t, outDir, entry{"out/spec.md/", tar.TypeDir, ""}), "",
			"/kiln/out/spec.md is not a file"},
		{"white space only", makeTar(t, outDir, entry{"out/spec.md", tar.TypeReg, " \n\t\r\n"}), "",
			"the spec is empty"},
		{"an empty spec", makeTar(t, outDir, entry{"out/spec.md", tar.TypeReg, ""}), "", "the spec is empty"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got, problem, err := ReadSpec(writeRecord(t, tc.documents), "r")
			if err != nil {
				t.Fatalf("ReadSpec: %v", err)
			}
			if problem != tc.problem || string(got) != tc.spec {
				t.Errorf("ReadSpec gave %d bytes and the problem %q, want %d bytes and %q", len(got), problem, len(tc.spec), tc.problem)
			}
		})
	}
}

// TestReadSpecDamaged checks that an archive that is wrong in itself is a
// problem and not an error: the run is finished, and its card stops.
func TestReadSpecDamaged(t *testing.T) {
	whole := makeTar(t, outDir, entry{"out/spec.md", tar.TypeReg, strings.Repeat("a line of the spec\n", 100)})
	// The spec's data begins after the two headers of 512 bytes each.
	cut := whole[:1024+700]
	badSum := bytes.Clone(whole)
	badSum[512+148] ^= 0x01 // a digit of the second header's checksum
	noise := make([]byte, 1024)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	for what, documents := range map[string][]byte{
		"cut off inside the spec's data": cut,
		"a header's checksum is wrong":   badSum,
		"1024 bytes of noise":            noise,
	} {
		t.Run(what, func(t *testing.T) {
			got, problem, err := ReadSpec(writeRecord(t, documents), "r")
			if err != nil {
				t.Fatalf("ReadSpec returned the error %v, want a problem", err)
			}
			if got != nil || !strings.HasPrefix(problem, "the run's documents.tar is damaged: ") {
				t.Errorf("ReadSpec gave %d bytes and the problem %q, want the archive damaged", len(got), problem)
			}
			t.Logf("%s", problem)
		})
	}
}

// TestReadSpecDisk checks that a file that cannot be opened or read is an
// error, and no problem: the tick tries again on the next tick.
func TestReadSpecDisk(t *testing.T) {
	t.Run("no read permission", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root reads a file without read permission")
		}
		runs := writeRecord(t, makeTar(t, outDir, entry{"out/spec.md", tar.TypeReg, "# Spec\n"}))
		if err := os.Chmod(filepath.Join(runs, "r", "documents.tar"), 0); err != nil {
			t.Fatal(err)
		}
		got, problem, err := ReadSpec(runs, "r")
		if err == nil || problem != "" || got != nil {
			t.Errorf("ReadSpec gave %d bytes, the problem %q and the error %v, want only an error", len(got), problem, err)
		}
	})
	// Open fails for root as well, which the check runs as.
	t.Run("documents.tar a symlink to itself", func(t *testing.T) {
		runs := t.TempDir()
		if err := os.MkdirAll(filepath.Join(runs, "r"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("documents.tar", filepath.Join(runs, "r", "documents.tar")); err != nil {
			t.Fatal(err)
		}
		got, problem, err := ReadSpec(runs, "r")
		if err == nil || problem != "" || got != nil {
			t.Errorf("ReadSpec gave %d bytes, the problem %q and the error %v, want only an error", len(got), problem, err)
		}
	})
	t.Run("documents.tar a directory", func(t *testing.T) {
		runs := t.TempDir()
		if err := os.MkdirAll(filepath.Join(runs, "r", "documents.tar"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, problem, err := ReadSpec(runs, "r")
		if err == nil || problem != "" || got != nil {
			t.Errorf("ReadSpec gave %d bytes, the problem %q and the error %v, want only an error", len(got), problem, err)
		}
		t.Logf("%v", err)
	})
	t.Run("no record", func(t *testing.T) {
		if _, problem, err := ReadSpec(t.TempDir(), "r"); err == nil || problem != "" {
			t.Errorf("ReadSpec gave the problem %q and the error %v, want only an error", problem, err)
		}
	})
}

// TestReadDocument reads a review and a report as a spec is read, with the
// same limit, and says when the run wrote none.
func TestReadDocument(t *testing.T) {
	review := "Verdict: passes\n\nNo findings.\n"
	both := makeTar(t, outDir, entry{"out/report.md", tar.TypeReg, "the report"}, entry{"out/review.md", tar.TypeReg, review})
	for _, tc := range []struct {
		what      string
		documents []byte
		kind      string
		want      Document
	}{
		{"a review", both, "review", Document{Text: []byte(review)}},
		{"a report", both, "report", Document{Text: []byte("the report")}},
		{"no review", makeTar(t, outDir, entry{"out/spec.md", tar.TypeReg, review}), "review",
			Document{Problem: "the run wrote no review (/kiln/out/review.md)", Missing: true}},
		{"no report", makeTar(t, outDir), "report",
			Document{Problem: "the run wrote no report (/kiln/out/report.md)", Missing: true}},
		{"a review too long", makeTar(t, outDir, entry{"out/review.md", tar.TypeReg, strings.Repeat("x", maxSpec+1)}), "review",
			Document{Problem: "the review is 262145 bytes, more than 262144"}},
		{"an empty report", makeTar(t, outDir, entry{"out/report.md", tar.TypeReg, "\n"}), "report",
			Document{Problem: "the report is empty"}},
		{"a review that is a symlink", makeTar(t, outDir, entry{"out/review.md", tar.TypeSymlink, "/etc/passwd"}), "review",
			Document{Problem: "/kiln/out/review.md is not a file"}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got, err := ReadDocument(writeRecord(t, tc.documents), "r", tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Text) != string(tc.want.Text) || got.Problem != tc.want.Problem || got.Missing != tc.want.Missing {
				t.Errorf("ReadDocument gave %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestTitleOf(t *testing.T) {
	const head = "# Spec: a card\n\nModel for implement: claude-sonnet-5-5.\n\n"
	for _, c := range []struct {
		what, spec, want string
	}{
		{"a plain head line", head + "Pull request title: `feat(run): x`\n\n## 1. What\n", "feat(run): x"},
		{"a list item", head + "- Pull request title: `feat: x`\n## 1. What\n", "feat: x"},
		{"a list item with *", head + "  * Pull request title: `feat: x`\n", "feat: x"},
		{"CRLF", "# Spec\r\n\r\nPull request title: `feat: x`\r\n\r\n## 1. What\r\n", "feat: x"},
		{"trailing spaces", head + "Pull request title: `feat: x` \t \n", "feat: x"},
		{"spaces inside the backticks", head + "Pull request title: `  feat: x  `\n", "feat: x"},
		{"no ## heading", head + "Pull request title: `feat: x`\n\nMore text.\n", "feat: x"},
		{"a non-ASCII title", head + "Pull request title: `feat: é`\n", "feat: é"},

		{"no such line", head + "## 1. What\n", ""},
		{"only after the first ##", head + "## 1. What\n\nPull request title: `feat: x`\n", ""},
		{"empty backticks", head + "Pull request title: ``\n", ""},
		{"spaces in the backticks", head + "Pull request title: `   `\n", ""},
		{"a backtick inside", head + "Pull request title: `feat: `x``\n", ""},
		{"a control character", head + "Pull request title: `feat: \x1b[2Kx`\n", ""},
		{"a tab", head + "Pull request title: `feat:\tx`\n", ""},
		{"U+202E", head + "Pull request title: `feat: ‮x`\n", ""},
		{"257 bytes", head + "Pull request title: `" + strings.Repeat("a", 257) + "`\n", ""},
		{"invalid UTF-8", head + "Pull request title: `feat: \xff`\n", ""},
		{"no backticks", head + "Pull request title: feat: x\n", ""},
		{"an invalid first line before a valid one", head + "Pull request title: ``\nPull request title: `feat: x`\n", ""},
		{"two list markers", head + "- - Pull request title: `feat: x`\n", ""},
	} {
		if got := TitleOf(c.spec); got != c.want {
			t.Errorf("%s: TitleOf = %q, want %q", c.what, got, c.want)
		}
	}
	if got := TitleOf(head + "Pull request title: `" + strings.Repeat("a", 256) + "`\n"); len(got) != 256 {
		t.Errorf("a title of 256 bytes gave %d bytes", len(got))
	}
}

func TestModelOf(t *testing.T) {
	for _, c := range []struct {
		what, spec, want string
	}{
		{"plain", "# Spec\n\nModel for implement: claude-sonnet-5-5\n", "claude-sonnet-5-5"},
		{"trailing period", "Model for implement: claude-haiku-5-5.\n", "claude-haiku-5-5"},
		{"comma and why", "Model for implement: claude-haiku-5-5, as the change is small.\n", "claude-haiku-5-5"},
		{"dash and why", "Model for implement: claude-opus-5-5 — the tick is subtle.\n", "claude-opus-5-5"},
		{"parenthesis", "Model for implement: claude-opus-5-5 (subtle)\n", "claude-opus-5-5"},
		{"backticks", "Model for implement: `claude-sonnet-5-5`, because it is small.\n", "claude-sonnet-5-5"},
		{"list item", "- Model for implement: claude-opus-5-5\n\n## 1. What\n", "claude-opus-5-5"},
		{"CRLF", "# Spec\r\n\r\nModel for implement: claude-opus-5-5\r\n\r\n## 1.\r\n", "claude-opus-5-5"},
		{"not allowed", "Model for implement: claude-opus-5-6\n", ""},
		{"an alias", "Model for implement: sonnet\n", ""},
		{"a version suffix", "Model for implement: claude-opus-5-5.1\n", ""},
		{"a letter after", "Model for implement: claude-opus-5-5x\n", ""},
		{"case", "Model for implement: Claude-Opus-5-5\n", ""},
		{"no line", "# Spec\n\nText.\n", ""},
		{"after the first ##", "# Spec\n\n## 1. What\n\nModel for implement: claude-opus-5-5\n", ""},
		{"unclosed backtick", "Model for implement: `claude-opus-5-5\n", ""},
		{"invalid first line", "Model for implement: gpt\nModel for implement: claude-opus-5-5\n", ""},
		{"bold label", "**Model for implement:** claude-haiku-5-5\n", ""},
		{"empty", "Model for implement:\n", ""},
	} {
		if got := ModelOf(c.spec); got != c.want {
			t.Errorf("%s: ModelOf = %q, want %q", c.what, got, c.want)
		}
	}
}

func TestImplementModelsAreTheInstructions(t *testing.T) {
	var line string
	for _, l := range strings.Split(instructions.Spec, "\n") {
		if strings.HasPrefix(l, "- one line `Model for implement:") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("instructions/spec.md has no line that starts with - one line `Model for implement:")
	}
	var got []string
	for _, part := range strings.Split(line, "`")[1:] {
		if strings.HasPrefix(part, "claude-") {
			got = append(got, part)
		}
	}
	if !slices.Equal(got, ImplementModels) {
		t.Errorf("the line names %q, ImplementModels is %q", got, ImplementModels)
	}
}
