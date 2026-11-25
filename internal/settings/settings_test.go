package settings

import (
	"os"
	"path/filepath"
	"reflect"
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

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(dir, strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())+".json")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := `{"repos": [{"name": "kiln", "copy": "/a/kiln.git", "own": true, "publish_command": "pub", "check_result_command": "chk", "spec_hosts": ["*"]},
		{"name": "work", "copy": "/a/work.git"}]}`
	t.Run("good", func(t *testing.T) {
		got, err := Load(write(t, good))
		want := []Repo{{Name: "kiln", Copy: "/a/kiln.git", Own: true, PublishCommand: "pub", CheckResultCommand: "chk", SpecHosts: []string{"*"}},
			{Name: "work", Copy: "/a/work.git"}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Load = %+v, %v, want %+v", got, err, want)
		}
	})
	t.Run("one repo without the optional members", func(t *testing.T) {
		got, err := Load(write(t, `{"repos":[{"name":"kiln","copy":"/a/k.git","own":true}]}`))
		if err != nil || len(got) != 1 || got[0].PublishCommand != "" || got[0].CheckResultCommand != "" || got[0].SpecHosts != nil {
			t.Fatalf("Load = %+v, %v", got, err)
		}
	})
	one := func(member string) string {
		return `{"repos":[{"name":"kiln","copy":"/a/k.git","own":true` + member + `}]}`
	}
	for _, tc := range []struct{ name, content, want string }{
		{"unknown member", one(`,"publish_comand":"x"`), `unknown field "publish_comand"`},
		{"limit", `{"limit":3,"repos":[]}`, `unknown field "limit"`},
		{"own as a string", `{"repos":[{"name":"kiln","copy":"/a/k.git","own":"yes"}]}`, "own"},
		{"a second value", one("") + ` {}`, "more after"},
		{"stray text", one("") + ` x`, "settings file"},
		{"empty", "", "EOF"},
		{"an array", "[]", "cannot unmarshal array"},
		{"no repos", `{"repos":[]}`, "the instance has no repo"},
		{"no repos member", `{}`, "the instance has no repo"},
		{"relative copy", `{"repos":[{"name":"kiln","copy":"repo.git","own":true}]}`, `the copy of the repo "kiln" is not an absolute path`},
		{"missing copy", `{"repos":[{"name":"kiln","own":true}]}`, `"kiln" has no copy`},
		{"two own", `{"repos":[{"name":"a","copy":"/a","own":true},{"name":"b","copy":"/b","own":true}]}`, "more than one repo is marked"},
		{"name with /", `{"repos":[{"name":"a/b","copy":"/a","own":true}]}`, "is not only ASCII"},
		{"over 1 MiB", one("") + strings.Repeat(" ", MaxFile), "is larger than 1 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := write(t, tc.content)
			_, err := Load(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "the settings file "+p) {
				t.Fatalf("Load error = %v, want the settings file %s and %q", err, p, tc.want)
			}
		})
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil || !strings.Contains(err.Error(), "the settings file") {
		t.Errorf("a missing file: %v", err)
	}
}
