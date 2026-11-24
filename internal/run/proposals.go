package run

import (
	"regexp"
	"strings"
)

// Proposal is one proposal a review run wrote to /kiln/out/proposals.md
// (ADR 0033): its title and its text, as written.
type Proposal struct {
	Title string
	Text  string
}

// proposalLine is a proposal's title line: after white space and list, quote,
// heading and bold marks, the word "proposal" in any case, a number if any,
// bold marks if any, and a colon; the title is what follows.
var proposalLine = regexp.MustCompile(`(?i)^[\s#>\-*+_]*proposal(?:[ \t]*[0-9]+)?[ \t]*[*_]*[ \t]*:(.*)$`)

// Proposals reads the proposals of a review run's proposals.md. Each starts
// with a title line, "Proposal: <title>", and its text is the lines after it,
// up to the next title line or the end, exactly as written, with blank lines
// removed only at its start and end. What the shape does not fit is kept as
// written, never dropped: lines before the first title line that are more
// than blank lines and headings are a proposal with the title "", and so is a
// file with no title line, unless it is blank. A title line with neither a
// title nor a text is no proposal, and neither is a title of "none" with no
// text: the review has none.
func Proposals(text []byte) []Proposal {
	lines := strings.Split(string(text), "\n")
	var out []Proposal
	add := func(title string, body []string) {
		p := Proposal{Title: title, Text: strings.Join(trimBlank(body), "\n")}
		none := strings.TrimSuffix(strings.ToLower(p.Title), ".") == "none"
		if p.Text == "" && (p.Title == "" || none) {
			return
		}
		out = append(out, p)
	}
	first := -1
	for i, line := range lines {
		if proposalLine.MatchString(line) {
			first = i
			break
		}
	}
	if first < 0 {
		add("", lines)
		return out
	}
	for _, line := range lines[:first] {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			add("", lines[:first])
			break
		}
	}
	var title string
	var body []string
	for i, line := range lines[first:] {
		m := proposalLine.FindStringSubmatch(line)
		if m == nil {
			body = append(body, line)
			continue
		}
		if i > 0 {
			add(title, body)
		}
		title, body = titleOf(m[1]), nil
	}
	add(title, body)
	return out
}

// titleOf is the title on a title line: the rest of the line without white
// space, bold marks at both ends, and heading marks at its end.
func titleOf(rest string) string {
	t := strings.Trim(rest, " \t\r*_")
	return strings.Trim(strings.TrimRight(t, "# \t"), " \t*_")
}

// trimBlank removes the lines of only white space at the start and the end.
func trimBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
