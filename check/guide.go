package main

import (
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// guideFile is the colleague's install guide, which TestFreshInstall follows
// (ADR 0061).
const guideFile = "docs/getting-started.md"

// guideStep is the name of the step that keeps the guide testable.
const guideStep = "the guide is testable"

// The kinds of a guide's block. TestFreshInstall writes its own values for
// the values block, runs the run, check and start blocks, and skips the mac
// and azure blocks, which only a Mac and a sign-in at Azure DevOps can run.
const (
	kindValues = "values"
	kindRun    = "run"
	kindCheck  = "check"
	kindStart  = "start"
	kindMac    = "mac"
	kindAzure  = "azure"
)

// runKinds are the kinds whose block is run by a shell, and so a sh block.
var runKinds = map[string]bool{kindValues: true, kindRun: true, kindCheck: true, kindStart: true}

// knownKinds are every kind a marker may name.
var knownKinds = map[string]bool{kindValues: true, kindRun: true, kindCheck: true, kindStart: true, kindMac: true, kindAzure: true}

// guideMarker is the line just before every fenced block of the guide.
var guideMarker = regexp.MustCompile(`^<!-- kiln-install: ([a-z]+) -->$`)

// guideMarkerLike is any line that looks like a marker, so that a misspelt
// one is named rather than taken as prose.
var guideMarkerLike = regexp.MustCompile(`kiln-install`)

// stepHeading is a step's heading, "## <n>. <title>".
var stepHeading = regexp.MustCompile(`^## [0-9]+\. `)

// guideForbidden are the texts the guide may not hold: paths of someone's own
// home directory, which would not fit the colleague's Mac.
var guideForbidden = []string{"/Users/", "/home/", "~/work/"}

// guideBlock is one fenced block of the guide.
type guideBlock struct {
	Kind string
	Info string // the fence's info string, such as sh
	Line int    // the line of the opening fence, from 1
	Step string // the heading of the step it is in, "" before the first
	Text string // the lines between the fences, without the fence's indent
}

// parseGuide reads the guide's blocks, in order, and returns every problem
// with the guide, each with its line: a fenced block without a marker on the
// line just before it, a marker of an unknown kind or without a block after
// it, a run, check, values or start block that is not sh, a fence that is not
// closed, not exactly one values and one start block, a step without a
// paragraph that begins with **Check:**, and a forbidden text.
func parseGuide(text string) ([]guideBlock, []string) {
	lines := strings.Split(text, "\n")
	var blocks []guideBlock
	var problems []string
	problem := func(line int, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s:%d: %s", guideFile, line, fmt.Sprintf(format, args...)))
	}

	step, stepLine, checked := "", 0, true
	endStep := func() {
		if !checked {
			problem(stepLine, "the step %q has no paragraph that begins with **Check:**", step)
		}
	}
	pending, pendingLine := "", 0 // a marker waiting for its block
	for i := 0; i < len(lines); i++ {
		line, n := lines[i], i+1
		for _, f := range guideForbidden {
			if strings.Contains(line, f) {
				problem(n, "holds %s, a path of someone's own machine", f)
			}
		}
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)

		if strings.HasPrefix(trimmed, "```") {
			info := strings.TrimSpace(strings.TrimLeft(trimmed, "`"))
			fence := trimmed[:len(trimmed)-len(strings.TrimLeft(trimmed, "`"))]
			b := guideBlock{Kind: pending, Info: info, Line: n, Step: step}
			switch {
			case pending == "":
				problem(n, "a fenced block without a marker <!-- kiln-install: <kind> --> on the line just before it")
			case runKinds[pending] && info != "sh":
				problem(n, "a %s block must be a sh block, not %q", pending, info)
			}
			pending = ""
			var body []string
			closed := false
			for i++; i < len(lines); i++ {
				l := lines[i]
				for _, f := range guideForbidden {
					if strings.Contains(l, f) {
						problem(i+1, "holds %s, a path of someone's own machine", f)
					}
				}
				if t := strings.TrimLeft(l, " "); strings.HasPrefix(t, fence) && strings.TrimSpace(strings.TrimLeft(t, "`")) == "" {
					closed = true
					break
				}
				for k := 0; k < indent && strings.HasPrefix(l, " "); k++ {
					l = l[1:]
				}
				body = append(body, l)
			}
			if !closed {
				problem(n, "the fenced block is not closed")
			}
			b.Text = strings.Join(body, "\n") + "\n"
			if b.Kind != "" {
				blocks = append(blocks, b)
			}
			continue
		}

		if pending != "" {
			problem(pendingLine, "a marker without a fenced block on the line after it")
			pending = ""
		}
		if m := guideMarker.FindStringSubmatch(trimmed); m != nil {
			if !knownKinds[m[1]] {
				problem(n, "a marker of the unknown kind %q", m[1])
				continue
			}
			pending, pendingLine = m[1], n
			continue
		}
		if guideMarkerLike.MatchString(trimmed) && strings.HasPrefix(trimmed, "<!--") {
			problem(n, "a marker that is not <!-- kiln-install: <kind> -->")
			continue
		}
		if strings.HasPrefix(line, "## ") {
			endStep()
			step, stepLine, checked = "", 0, true
			if stepHeading.MatchString(line) {
				step, stepLine, checked = strings.TrimPrefix(line, "## "), n, false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "**Check:**") {
			checked = true
		}
	}
	if pending != "" {
		problem(pendingLine, "a marker without a fenced block on the line after it")
	}
	endStep()

	for _, kind := range []string{kindValues, kindStart} {
		var at []string
		for _, b := range blocks {
			if b.Kind == kind {
				at = append(at, fmt.Sprint(b.Line))
			}
		}
		switch len(at) {
		case 0:
			problems = append(problems, fmt.Sprintf("%s: no %s block, want exactly one", guideFile, kind))
		case 1:
		default:
			problems = append(problems, fmt.Sprintf("%s: %d %s blocks (at lines %s), want exactly one", guideFile, len(at), kind, strings.Join(at, ", ")))
		}
	}
	return blocks, problems
}

// valueAssignment is a line of the values block that assigns a value.
var valueAssignment = regexp.MustCompile(`^\s*([A-Z][A-Z0-9_]*)=`)

// valueNames are the names a values block assigns, sorted, each once.
func valueNames(text string) []string {
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(text, "\n") {
		if m := valueAssignment.FindStringSubmatch(line); m != nil && !seen[m[1]] {
			seen[m[1]] = true
			names = append(names, m[1])
		}
	}
	sort.Strings(names)
	return names
}

// guideTestable fails with every problem parseGuide finds in the guide.
func guideTestable(fsys fs.FS) error {
	text, err := fs.ReadFile(fsys, guideFile)
	if err != nil {
		return err
	}
	if _, problems := parseGuide(string(text)); len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	return nil
}
