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

// maxSpec is the largest spec that is read: 256 KiB, 7.6 times the largest
// spec written by hand so far (34,160 bytes, card 12).
const maxSpec = 256 << 10

// specEntry is the spec's name in documents.tar, the archive the engine hands
// over of /kiln/out.
const specEntry = "out/spec.md"

// ReadSpec reads the spec a run wrote, /kiln/out/spec.md, out of
// <runs>/<id>/documents.tar, without unpacking anything (ADR 0038). It reads
// the first entry of that name, and only it, into memory, and returns its bytes
// exactly as written.
//
// A spec that cannot be taken is a problem, a verdict on the run: no such
// entry, one that is not a regular file, one over maxSpec, one of nothing but
// white space, or an archive that is damaged. A file that cannot be opened or
// read from the disk is an error, which says nothing of the run. The two are
// told apart by the file's own errors: when the archive cannot be read and the
// file returned no error, every byte was read and the archive itself is wrong.
func ReadSpec(runs, id string) (spec []byte, problem string, err error) {
	f, err := os.Open(filepath.Join(runs, id, "documents.tar"))
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	file := &firstError{r: f}
	damaged := func(err error) ([]byte, string, error) {
		if file.err != nil {
			return nil, "", file.err
		}
		return nil, "the run's documents.tar is damaged: " + err.Error(), nil
	}

	tr := tar.NewReader(file)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, "the run wrote no spec (/kiln/out/spec.md)", nil
		}
		if err != nil {
			return damaged(err)
		}
		// The engine names a directory with a slash at its end.
		if strings.TrimSuffix(h.Name, "/") != specEntry {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return nil, "/kiln/out/spec.md is not a file", nil
		}
		if h.Size > maxSpec {
			return nil, fmt.Sprintf("the spec is %d bytes, more than %d", h.Size, maxSpec), nil
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return damaged(err)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, "the spec is empty", nil
		}
		return data, "", nil
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
