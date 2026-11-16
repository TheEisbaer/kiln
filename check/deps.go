package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// parts are the four parts every entry has (ADR 0051), as the labels used in
// docs/dependencies.md.
var parts = []string{"For", "Looked at", "Why this one", "Licence"}

// replacedLabel marks an entry whose dependency is gone.
const replacedLabel = "Replaced"

var fieldLine = regexp.MustCompile(`^- \*\*([^*]+):\*\*\s*(.*)$`)

// checkDependencies applies the dependency rule (ADR 0051): every direct
// dependency in goMod has a complete entry in the "Go modules" section of doc,
// and an entry without a direct dependency is marked as replaced. It returns
// one message per problem, in a stable order.
func checkDependencies(goMod, doc string) []string {
	direct := directModules(goMod)
	entries := moduleEntries(doc)

	var problems []string
	for _, mod := range sorted(direct) {
		e, ok := entries[mod]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: direct dependency without an entry in docs/dependencies.md", mod))
		case e.has(replacedLabel):
			problems = append(problems, fmt.Sprintf("%s: entry is marked as replaced, but it is still a direct dependency", mod))
		}
	}
	for _, name := range sortedKeys(entries) {
		e := entries[name]
		for _, p := range parts {
			if !e.has(p) {
				problems = append(problems, fmt.Sprintf("%s: entry lacks %q", name, p))
			}
		}
		if _, ok := direct[name]; !ok && !e.has(replacedLabel) {
			problems = append(problems, fmt.Sprintf("%s: entry without a direct dependency, and not marked as replaced", name))
		}
	}
	return problems
}

// entry maps the label of each part to its text.
type entry map[string]string

// has reports whether the part is present with some text.
func (e entry) has(label string) bool { return strings.TrimSpace(e[label]) != "" }

// directModules returns the modules go.mod requires without the indirect mark.
func directModules(goMod string) map[string]bool {
	mods := map[string]bool{}
	inBlock := false
	for _, line := range strings.Split(goMod, "\n") {
		text, comment, _ := strings.Cut(line, "//")
		fields := strings.Fields(text)
		if len(fields) == 0 {
			continue
		}
		switch {
		case inBlock && fields[0] == ")":
			inBlock = false
			continue
		case !inBlock && fields[0] == "require" && len(fields) == 2 && fields[1] == "(":
			inBlock = true
			continue
		case !inBlock && fields[0] == "require" && len(fields) >= 2:
			fields = fields[1:]
		case !inBlock:
			continue
		}
		if !strings.Contains(comment, "indirect") {
			mods[fields[0]] = true
		}
	}
	return mods
}

// moduleEntries reads the entries of the "## Go modules" section: a "###"
// heading naming the module, then lines of the form "- **Label:** text", which
// may continue on following lines.
func moduleEntries(doc string) map[string]entry {
	entries := map[string]entry{}
	inSection := false
	var cur entry
	var label string
	for _, line := range strings.Split(doc, "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			inSection = strings.TrimSpace(strings.TrimPrefix(line, "## ")) == "Go modules"
			cur = nil
		case !inSection:
		case strings.HasPrefix(line, "### "):
			name := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "### ")), "`")
			cur = entry{}
			entries[name] = cur
			label = ""
		case cur == nil:
		default:
			if m := fieldLine.FindStringSubmatch(line); m != nil {
				label = m[1]
				cur[label] = m[2]
			} else if label != "" {
				cur[label] += " " + strings.TrimSpace(line)
			}
		}
	}
	return entries
}

func sorted(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys(m map[string]entry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
