package run

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// maxSpec is the largest document that is read: 256 KiB, 7.6 times the
// largest spec written by hand so far (34,160 bytes, card 12). A review and a
// report have the same limit.
const maxSpec = 256 << 10

// Document is what ReadDocument took of a run's document: its bytes exactly as
// written, or the problem why it cannot be taken. Missing is set, with the
// problem, when the run wrote none.
type Document struct {
	Text    []byte
	Problem string
	Missing bool
}

// ReadSpec reads the spec a run wrote, /kiln/out/spec.md, as ReadDocument
// does.
func ReadSpec(runs, id string) (spec []byte, problem string, err error) {
	d, err := ReadDocument(runs, id, "spec")
	return d.Text, d.Problem, err
}

// ReadDocument reads the document of the kind a run wrote, /kiln/out/<kind>.md,
// such as spec, review or report, out of <runs>/<id>/documents.tar, without
// unpacking anything (ADR 0038). It reads the first entry of that name, and
// only it, into memory, and returns its bytes exactly as written.
//
// A document that cannot be taken is a problem, a verdict on the run: no such
// entry, one that is not a regular file, one over maxSpec, one of nothing but
// white space, or an archive that is damaged. A file that cannot be opened or
// read from the disk is an error, which says nothing of the run. The two are
// told apart by the file's own errors: when the archive cannot be read and the
// file returned no error, every byte was read and the archive itself is wrong.
func ReadDocument(runs, id, kind string) (Document, error) {
	f, err := os.Open(filepath.Join(runs, id, "documents.tar"))
	if err != nil {
		return Document{}, err
	}
	defer f.Close()
	file := &firstError{r: f}
	damaged := func(err error) (Document, error) {
		if file.err != nil {
			return Document{}, file.err
		}
		return Document{Problem: "the run's documents.tar is damaged: " + err.Error()}, nil
	}
	// The engine hands over /kiln/out as out/.
	entry, path := "out/"+kind+".md", "/kiln/out/"+kind+".md"

	tr := tar.NewReader(file)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return Document{Problem: fmt.Sprintf("the run wrote no %s (%s)", kind, path), Missing: true}, nil
		}
		if err != nil {
			return damaged(err)
		}
		// The engine names a directory with a slash at its end.
		if strings.TrimSuffix(h.Name, "/") != entry {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return Document{Problem: path + " is not a file"}, nil
		}
		if h.Size > maxSpec {
			return Document{Problem: fmt.Sprintf("the %s is %d bytes, more than %d", kind, h.Size, maxSpec)}, nil
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return damaged(err)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return Document{Problem: "the " + kind + " is empty"}, nil
		}
		return Document{Text: data}, nil
	}
}

// firstError reads from a file and keeps the first error the file returned
// other than io.EOF.
type firstError struct {
	r   io.Reader
	err error
}

func (f *firstError) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && f.err == nil {
		f.err = err
	}
	return n, err
}

// maxTitle is the longest pull request title a spec may name, in bytes: a
// bound chosen here, not a limit the Git host documents.
const maxTitle = 256

// titleLine is how the line that names the title starts.
const titleLine = "Pull request title: `"

// TitleOf returns the pull request title a spec names on its line
// "Pull request title: `<title>`", or "" when it names none that can be used.
// Only the head is read, the lines before the first that starts with "## ", or
// the whole document when it has no such line, so that an example of the line
// in the body is never taken. The line may be a list item and may have white
// space around it. The first such line decides: when its title fails
// validTitle, the result is "" and later lines are not tried.
func TitleOf(spec string) string {
	for _, line := range strings.Split(spec, "\n") {
		if strings.HasPrefix(line, "## ") {
			return ""
		}
		line = strings.TrimRight(line, "\r \t")
		line = strings.TrimLeft(line, " ")
		if rest, ok := strings.CutPrefix(line, "- "); ok {
			line = rest
		} else if rest, ok := strings.CutPrefix(line, "* "); ok {
			line = rest
		}
		inner, ok := strings.CutPrefix(line, titleLine)
		if !ok || !strings.HasSuffix(inner, "`") {
			continue
		}
		title := strings.TrimSpace(strings.TrimSuffix(inner, "`"))
		if validTitle(title) != nil {
			return ""
		}
		return title
	}
	return ""
}

// validTitle checks a title for a save's commit message: not empty, at most
// maxTitle bytes, valid UTF-8, and without a backtick, a control character or
// a bidirectional control character.
func validTitle(title string) error {
	switch {
	case title == "":
		return errors.New("the title is empty")
	case len(title) > maxTitle:
		return fmt.Errorf("the title is %d bytes, more than %d", len(title), maxTitle)
	case !utf8.ValidString(title):
		return errors.New("the title is not valid UTF-8")
	case strings.Contains(title, "`"):
		return errors.New("the title contains a backtick")
	}
	for _, r := range title {
		// isControl lets newline and tab through, which a title must not have.
		if isControl(r) || r == '\n' || r == '\t' {
			return fmt.Errorf("the title contains the control character %U", r)
		}
		if (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return fmt.Errorf("the title contains the bidirectional control character %U", r)
		}
	}
	return nil
}
