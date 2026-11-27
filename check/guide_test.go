package main

import (
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// validGuide is a guide that parseGuide takes: every block marked, every step
// checked, one values block and one start block.
const validGuide = "# Guide\n\nIntro.\n\n## 0. Values\n\n" +
	"<!-- kiln-install: values -->\n```sh\nA=1\nB=$HOME/x\n```\n\n**Check:** it is there.\n\n" +
	"## 1. Tools\n\n<!-- kiln-install: mac -->\n```sh\nbrew install x\n```\n\n" +
	"- A list item, with a block:\n\n  <!-- kiln-install: run -->\n  ```sh\n  echo indented\n  ```\n\n" +
	"**Check:** this block prints ok.\n\n<!-- kiln-install: check -->\n```sh\necho ok\n```\n\n" +
	"## 2. Start\n\n<!-- kiln-install: start -->\n```sh\nkiln-up\n```\n\n" +
	"<!-- kiln-install: azure -->\n```json\n{}\n```\n\n**Check:** the screen opens.\n\n" +
	"## What goes wrong\n\n- Nothing.\n"

func TestParseGuide(t *testing.T) {
	blocks, problems := parseGuide(validGuide)
	if len(problems) > 0 {
		t.Fatalf("a valid guide: %v", problems)
	}
	var kinds []string
	for _, b := range blocks {
		kinds = append(kinds, b.Kind)
	}
	if want := []string{"values", "mac", "run", "check", "start", "azure"}; !slices.Equal(kinds, want) {
		t.Errorf("the kinds are %v, want %v", kinds, want)
	}
	if b := blocks[2]; b.Text != "echo indented\n" || b.Step != "1. Tools" || b.Line != 25 {
		t.Errorf("the indented block is %+v, want its text without the indent, in step 1, at line 25", b)
	}
	if got := valueNames(blocks[0].Text); !slices.Equal(got, []string{"A", "B"}) {
		t.Errorf("the values are %v, want [A B]", got)
	}
}

func TestParseGuideRefuses(t *testing.T) {
	for _, c := range []struct {
		name, from, to, want string
	}{
		{"a block without a marker", "<!-- kiln-install: check -->\n```sh\necho ok", "```sh\necho ok",
			guideFile + ":31: a fenced block without a marker"},
		{"a marker of an unknown kind", "kiln-install: mac", "kiln-install: linux", `:17: a marker of the unknown kind "linux"`},
		{"a marker that is misspelt", "<!-- kiln-install: mac -->", "<!-- kiln-install mac -->", ":17: a marker that is not"},
		{"a marker without a block", "<!-- kiln-install: mac -->\n", "<!-- kiln-install: mac -->\n\n", ":17: a marker without a fenced block"},
		{"a run block that is not sh", "```sh\n  echo indented", "```bash\n  echo indented", `a run block must be a sh block, not "bash"`},
		{"a block that is not closed", "```json\n{}\n```\n", "```json\n{}\n", ":44: the fenced block is not closed"},
		{"two values blocks", "kiln-install: run", "kiln-install: values", ": 2 values blocks (at lines 8, 25), want exactly one"},
		{"no start block", "kiln-install: start", "kiln-install: run", ": no start block, want exactly one"},
		{"a step without a check", "**Check:** it is there.", "It is there.", `:5: the step "0. Values" has no paragraph that begins with **Check:**`},
		{"the last step without a check", "**Check:** the screen opens.", "The screen opens.", `the step "2. Start" has no paragraph`},
		{"a home directory on macOS", "Intro.", "Intro, in /Users/someone/kiln.", ":3: holds /Users/"},
		{"a home directory on Linux, in a block", "brew install x", "cd /home/someone", ":19: holds /home/"},
		{"the developer's checkout", "Intro.", "See ~/work/kiln.", ":3: holds ~/work/"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(validGuide, c.from) {
				t.Fatalf("the valid guide has no %q", c.from)
			}
			_, problems := parseGuide(strings.Replace(validGuide, c.from, c.to, 1))
			if !strings.Contains(strings.Join(problems, "\n"), c.want) {
				t.Errorf("the problems are %q, want one with %q", problems, c.want)
			}
		})
	}
}

func TestGuideTestable(t *testing.T) {
	if err := guideTestable(fstest.MapFS{guideFile: {Data: []byte(validGuide)}}); err != nil {
		t.Errorf("a valid guide: %v", err)
	}
	if err := guideTestable(fstest.MapFS{guideFile: {Data: []byte(strings.Replace(validGuide, "**Check:** it", "It", 1))}}); err == nil {
		t.Error("a step without a check: no error")
	}
	if err := guideTestable(fstest.MapFS{}); err == nil {
		t.Error("no guide: no error")
	}
}

// TestTheGuide parses the repo's own guide: it has the values that
// TestFreshInstall sets, and its blocks of every kind.
func TestTheGuide(t *testing.T) {
	text, err := os.ReadFile("../" + guideFile)
	if err != nil {
		t.Fatal(err)
	}
	blocks, problems := parseGuide(string(text))
	if len(problems) > 0 {
		t.Fatalf("the guide: %s", strings.Join(problems, "\n"))
	}
	for _, b := range blocks {
		if b.Kind == kindValues {
			if got := valueNames(b.Text); !slices.Equal(got, installValueNames()) {
				t.Errorf("the guide's values are %v, want %v, the names TestFreshInstall sets", got, installValueNames())
			}
		}
	}
}
