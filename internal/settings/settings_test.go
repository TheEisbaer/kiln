package settings

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	kiln := Repo{Name: "kiln", Copy: "/a/kiln.git", Own: true}
	second := Repo{Name: "second", Copy: "/a/second.git"}
	for _, tc := range []struct {
		name  string
		repos []Repo
		want  string // a part of the reason, "" for a list that is taken
	}{
		{"one repo", []Repo{kiln}, ""},
		{"two repos", []Repo{kiln, second}, ""},
		{"a name of 40 characters", []Repo{{Name: strings.Repeat("a", 40), Copy: "/c", Own: true}}, ""},
		{"no repo", nil, "has no repo"},
		{"no name", []Repo{kiln, {Copy: "/c"}}, "repo 2 has no name"},
		{"a name with /", []Repo{kiln, {Name: "a/b", Copy: "/c"}}, `"a/b" is not only`},
		{"a name with a letter that is not ASCII", []Repo{kiln, {Name: "ünter", Copy: "/c"}}, "is not only"},
		{"a name of 41 characters", []Repo{kiln, {Name: strings.Repeat("a", 41), Copy: "/c"}}, "longer than 40"},
		{"a name twice", []Repo{kiln, second, {Name: "second", Copy: "/d"}}, `"second" is given twice`},
		{"no copy", []Repo{kiln, {Name: "second"}}, `"second" has no copy`},
		{"none own", []Repo{second}, "no repo is marked"},
		{"two own", []Repo{kiln, {Name: "second", Copy: "/c", Own: true}}, "more than one repo is marked as Kiln's own: kiln, second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.repos)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && err == nil:
				t.Errorf("taken, want a refusal with %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Errorf("refused with %q, want %q in it", err, tc.want)
			}
		})
	}
}
