package run

import (
	"io"
	"strings"
	"unicode/utf8"
)

// A run can write any bytes to its output, and a sealed run may follow an
// instruction that was injected into it. Whatever of a run's text the command
// prints therefore goes through printable first, so that no escape sequence
// reaches the developer's terminal: none can erase or forge a line, set the
// window title or write the clipboard. The recorded output.jsonl and
// stderr.txt keep exactly what the run wrote.

// replacement stands for a control character that is not printed.
const replacement = '�'

// isControl reports whether a rune is a control character that is not
// printed: C0 except newline and tab, DEL, and C1, which a terminal may read
// as escape sequences as well.
func isControl(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f)
}

// printable returns s with every control character but newline and tab
// replaced.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return replacement
		}
		return r
	}, s)
}

// Printable is printable, for a caller that prints an error of this package's,
// which can carry text of a run's.
func Printable(s string) string { return printable(s) }

// printableWriter passes on what is written to it through printable, as it
// comes. A character can be split across two writes, so the bytes of one that
// is not complete at the end of a write are held until the next.
type printableWriter struct {
	w       io.Writer
	pending []byte
}

func (p *printableWriter) Write(b []byte) (int, error) {
	data := append(p.pending, b...)
	p.pending = nil
	var out []byte
	for len(data) > 0 {
		if !utf8.FullRune(data) {
			p.pending = append([]byte(nil), data...)
			break
		}
		r, size := utf8.DecodeRune(data)
		switch {
		// A byte that is not UTF-8 is replaced as well, as printable does: a
		// terminal in 8-bit mode reads 0x80 to 0x9f alone as C1.
		case r == utf8.RuneError && size == 1, isControl(r):
			out = utf8.AppendRune(out, replacement)
		default:
			out = append(out, data[:size]...)
		}
		data = data[size:]
	}
	if _, err := p.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}
