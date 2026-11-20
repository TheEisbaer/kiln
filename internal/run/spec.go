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
