package settings

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
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
		{"a limit below 0", []Repo{kiln, {Name: "second", Copy: "/c", Limit: -1}}, `the limit of the repo "second" is -1; it is at least 1`},
		{"none own", []Repo{second}, "no repo is marked"},
		{"two own", []Repo{kiln, {Name: "second", Copy: "/c", Own: true}}, "more than one repo is marked as Kiln's own: kiln, second"},
		{"a toolchain", []Repo{kiln, {Name: "work-2", Copy: "/c", Toolchain: &Toolchain{Dockerfile: "/d"}}}, ""},
		{"a toolchain on Kiln's own repo", []Repo{{Name: "kiln", Copy: "/c", Own: true, Toolchain: &Toolchain{Dockerfile: "/d"}}},
			`the repo "kiln" is Kiln's own and has a toolchain`},
		{"a toolchain on a name with upper case", []Repo{kiln, {Name: "Work", Copy: "/c", Toolchain: &Toolchain{Dockerfile: "/d"}}},
			`the repo "Work" has a toolchain, so its name is lower-case`},
		{"a toolchain on a name ending in -", []Repo{kiln, {Name: "work-", Copy: "/c", Toolchain: &Toolchain{Dockerfile: "/d"}}},
			`the repo "work-" has a toolchain`},
		{"upper case without a toolchain", []Repo{kiln, {Name: "Work", Copy: "/c"}}, ""},
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
		want := Settings{Repos: []Repo{{Name: "kiln", Copy: "/a/kiln.git", Own: true, PublishCommand: "pub", CheckResultCommand: "chk", SpecHosts: []string{"*"}},
			{Name: "work", Copy: "/a/work.git"}}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Load = %+v, %v, want %+v", got, err, want)
		}
	})
	t.Run("one repo without the optional members", func(t *testing.T) {
		s, err := Load(write(t, `{"repos":[{"name":"kiln","copy":"/a/k.git","own":true}]}`))
		got := s.Repos
		if err != nil || len(got) != 1 || got[0].PublishCommand != "" || got[0].CheckResultCommand != "" || got[0].SpecHosts != nil || s.Limit != 0 {
			t.Fatalf("Load = %+v, %v", s, err)
		}
	})
	one := func(member string) string {
		return `{"repos":[{"name":"kiln","copy":"/a/k.git","own":true` + member + `}]}`
	}
	work := func(member string) string {
		return `{"repos":[{"name":"kiln","copy":"/a/k.git","own":true},{"name":"work","copy":"/a/w.git"` + member + `}]}`
	}
	t.Run("a toolchain", func(t *testing.T) {
		s, err := Load(write(t, work(`,"toolchain":{"dockerfile":"/i/work.Dockerfile","inputs":["package-lock.json","src/lib/a..b"]}`)))
		want := &Toolchain{Dockerfile: "/i/work.Dockerfile", Inputs: []string{"package-lock.json", "src/lib/a..b"}}
		if err != nil || s.Repos[0].Toolchain != nil || !reflect.DeepEqual(s.Repos[1].Toolchain, want) {
			t.Fatalf("Load = %+v, %v, want the toolchain %+v on work only", s, err, want)
		}
		s, err = Load(write(t, work(`,"toolchain":{"dockerfile":"/i/work.Dockerfile"}`)))
		if err != nil || s.Repos[1].Toolchain == nil || len(s.Repos[1].Toolchain.Inputs) != 0 {
			t.Fatalf("Load without inputs = %+v, %v", s, err)
		}
	})
	t.Run("limit", func(t *testing.T) {
		s, err := Load(write(t, `{"repos":[{"name":"kiln","copy":"/a/k.git","own":true},{"name":"work","copy":"/a/w.git","limit":2}]}`))
		got := s.Repos
		if err != nil || len(got) != 2 || got[0].Limit != 0 || got[1].Limit != 2 {
			t.Fatalf("Load = %+v, %v, want the limits 0 and 2", got, err)
		}
		if l := (store.Limits{Repos: map[string]int{got[0].Name: got[0].Limit}, Instance: s.Limit}); l.Of("kiln") != 3 || l.OfInstance() != 3 {
			t.Errorf("a repo without a limit has the limit %d, and an instance without one %d; want 3 and 3", l.Of("kiln"), l.OfInstance())
		}
	})
	t.Run("the instance's limit", func(t *testing.T) {
		s, err := Load(write(t, `{"limit":2,"repos":[{"name":"kiln","copy":"/a/k.git","own":true}]}`))
		if err != nil || s.Limit != 2 || len(s.Repos) != 1 || s.Repos[0].Limit != 0 {
			t.Fatalf("Load = %+v, %v, want the instance's limit 2", s, err)
		}
	})
	for _, tc := range []struct{ name, content, want string }{
		{"unknown member", one(`,"publish_comand":"x"`), `unknown field "publish_comand"`},
		{"the instance's limit 0", `{"limit":0,"repos":[{"name":"kiln","copy":"/a/k.git","own":true}]}`, `: the instance's limit is 0; it is at least 1`},
		{"the instance's limit as a string", `{"limit":"2","repos":[{"name":"kiln","copy":"/a/k.git","own":true}]}`, "limit"},
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
		{"limit 0", one(`,"limit":0`), `: the limit of the repo "kiln" is 0; it is at least 1`},
		{"limit -1", one(`,"limit":-1`), `: the limit of the repo "kiln" is -1; it is at least 1`},
		{"limit 1.5", one(`,"limit":1.5`), "limit"},
		{"limit as a string", one(`,"limit":"3"`), "limit"},
		{"over 1 MiB", one("") + strings.Repeat(" ", MaxFile), "is larger than 1 MiB"},
		{"a toolchain's unknown member", work(`,"toolchain":{"dockerfile":"/d","input":["a"]}`), `unknown field "input"`},
		{"a toolchain without a Dockerfile", work(`,"toolchain":{"inputs":["a"]}`), `the toolchain's Dockerfile of the repo "work" is not an absolute path: ""`},
		{"a relative Dockerfile", work(`,"toolchain":{"dockerfile":"Dockerfile"}`), `the toolchain's Dockerfile of the repo "work" is not an absolute path: "Dockerfile"`},
		{"an empty input", work(`,"toolchain":{"dockerfile":"/d","inputs":[""]}`), `the toolchain's input "" of the repo "work" is not a path in the repo`},
		{"an absolute input", work(`,"toolchain":{"dockerfile":"/d","inputs":["/a"]}`), `the toolchain's input "/a"`},
		{"an input not in clean form", work(`,"toolchain":{"dockerfile":"/d","inputs":["a/"]}`), `the toolchain's input "a/"`},
		{"an input with //", work(`,"toolchain":{"dockerfile":"/d","inputs":["a//b"]}`), `the toolchain's input "a//b"`},
		{"the input .", work(`,"toolchain":{"dockerfile":"/d","inputs":["."]}`), `the toolchain's input "."`},
		{"the input ..", work(`,"toolchain":{"dockerfile":"/d","inputs":[".."]}`), `the toolchain's input ".."`},
		{"an input below ..", work(`,"toolchain":{"dockerfile":"/d","inputs":["../a"]}`), `the toolchain's input "../a"`},
		{"a toolchain on Kiln's own repo", one(`,"toolchain":{"dockerfile":"/d"}`), `the repo "kiln" is Kiln's own and has a toolchain`},
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

func TestLoadLimits(t *testing.T) {
	load := func(t *testing.T, member string) (Settings, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "settings.json")
		body := `{"repos":[{"name":"kiln","copy":"/a/kiln.git","own":true},{"name":"work","copy":"/a/w.git"` + member + `}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	for _, tc := range []struct {
		member string
		memory string
		cpus   int
	}{
		{``, "", 0},
		{`,"memory":"6m"`, "6m", 0},
		{`,"memory":"4g"`, "4g", 0},
		{`,"memory":"16384m"`, "16384m", 0},
		{`,"cpus":1`, "", 1},
		{`,"cpus":64`, "", 64},
		{`,"memory":"6g","cpus":3`, "6g", 3},
		{`,"memory":null,"cpus":null`, "", 0},
	} {
		t.Run("accepted "+tc.member, func(t *testing.T) {
			s, err := load(t, tc.member)
			if err != nil {
				t.Fatal(err)
			}
			if r := s.Repos[1]; r.Memory != tc.memory || r.CPUs != tc.cpus {
				t.Errorf("the repo has %q and %d, want %q and %d", r.Memory, r.CPUs, tc.memory, tc.cpus)
			}
			if r := s.Repos[0]; r.Memory != "" || r.CPUs != 0 {
				t.Errorf("Kiln's repo has %q and %d, want the defaults", r.Memory, r.CPUs)
			}
		})
	}
	for _, tc := range []struct{ member, want string }{
		{`,"memory":"5m"`, "below 6m"},
		{`,"memory":"0g"`, "not <n>m or <n>g"},
		{`,"memory":"04g"`, "not <n>m or <n>g"},
		{`,"memory":"4G"`, "not <n>m or <n>g"},
		{`,"memory":"4"`, "not <n>m or <n>g"},
		{`,"memory":"4gb"`, "not <n>m or <n>g"},
		{`,"memory":"-1g"`, "not <n>m or <n>g"},
		{`,"memory":" 4g"`, "not <n>m or <n>g"},
		{`,"memory":""`, `the memory is ""`},
		{`,"memory":"9999999999g"`, "more bytes"},
		{`,"memory":4`, "the memory is 4; it is a string"},
		{`,"cpus":1.5`, "the CPUs are 1.5"},
		{`,"cpus":0`, "the CPUs are 0"},
		{`,"cpus":-1`, "the CPUs are -1"},
		{`,"cpus":"2"`, `the CPUs are "2"`},
	} {
		t.Run("refused "+tc.member, func(t *testing.T) {
			_, err := load(t, tc.member)
			if err == nil || !strings.Contains(err.Error(), `the repo "work": `) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load error = %v, want the repo \"work\" and %q", err, tc.want)
			}
		})
	}
}

func TestLoadWorkItem(t *testing.T) {
	load := func(member string) (Settings, error) {
		path := filepath.Join(t.TempDir(), "settings.json")
		body := `{"repos":[{"name":"kiln","copy":"/a/kiln.git","own":true` + member + `}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	for member, want := range map[string]bool{``: false, `,"work_item":"required"`: true, `,"work_item":""`: false} {
		got, err := load(member)
		if err != nil || got.Repos[0].NeedsWorkItem != want {
			t.Errorf("%q: %+v, %v; want NeedsWorkItem %v", member, got.Repos, err, want)
		}
	}
	_, err := load(`,"work_item":"optional"`)
	if err == nil || !strings.Contains(err.Error(), `the work item of the repo "kiln" is "optional"; it is "required" or left out`) {
		t.Errorf(`"optional": %v`, err)
	}
	if _, err := load(`,"work_item":1`); err == nil {
		t.Error("a number was taken as the work item")
	}
}

// TestLoadCache takes a toolchain's cache, and refuses an unknown member, a
// relative Dockerfile, an input not in clean form and a path the seal
// refuses, each with the repo's name.
func TestLoadCache(t *testing.T) {
	load := func(t *testing.T, cache string) (Settings, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "settings.json")
		body := `{"repos":[{"name":"kiln","copy":"/a/kiln.git","own":true},` +
			`{"name":"work","copy":"/a/w.git","toolchain":{"dockerfile":"/d/work.Dockerfile"` + cache + `}}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	s, err := load(t, `,"cache":{"dockerfile":"/d/cache.Dockerfile","inputs":["package-lock.json","nuget"],"path":"/opt/cache"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := &Cache{Dockerfile: "/d/cache.Dockerfile", Inputs: []string{"package-lock.json", "nuget"}, Path: "/opt/cache"}
	if got := s.Repos[1].Toolchain.Cache; !reflect.DeepEqual(got, want) {
		t.Errorf("the cache is %+v, want %+v", got, want)
	}
	for _, cache := range []string{``, `,"cache":null`} {
		if s, err := load(t, cache); err != nil || s.Repos[1].Toolchain.Cache != nil {
			t.Errorf("%q: %v, %+v; want no cache", cache, err, s)
		}
	}
	if s, err := load(t, `,"cache":{"dockerfile":"/d/c","path":"/var/cache/nuget"}`); err != nil || s.Repos[1].Toolchain.Cache.Inputs != nil {
		t.Errorf("a cache without inputs: %v, %+v", err, s)
	}
	for _, tc := range []struct{ cache, want string }{
		{`,"cache":{"dockerfile":"/d/c","path":"/opt/cache","size":1}`, `the cache of the repo "work": json: unknown field "size"`},
		{`,"cache":{"dockerfile":"c.Dockerfile","path":"/opt/cache"}`, `the cache's Dockerfile of the repo "work" is not an absolute path`},
		{`,"cache":{"path":"/opt/cache"}`, `the cache's Dockerfile of the repo "work" is not an absolute path`},
		{`,"cache":{"dockerfile":"/d/c","inputs":["./lock.json"],"path":"/opt/cache"}`, `the cache's input "./lock.json" of the repo "work"`},
		{`,"cache":{"dockerfile":"/d/c","inputs":["../x"],"path":"/opt/cache"}`, `the cache's input "../x" of the repo "work"`},
		{`,"cache":{"dockerfile":"/d/c","path":"/kiln/cache"}`, `the repo "work": the cache's path "/kiln/cache"`},
		{`,"cache":{"dockerfile":"/d/c","path":"opt/cache"}`, `the repo "work": the cache's path "opt/cache"`},
		{`,"cache":{"dockerfile":"/d/c"}`, `the repo "work": the cache's path ""`},
		{`,"cache":{"dockerfile":"/d/c","path":3}`, `the cache of the repo "work": json: cannot unmarshal`},
		{`,"cache":[]`, `the cache of the repo "work": json: cannot unmarshal`},
	} {
		t.Run("refused "+tc.cache, func(t *testing.T) {
			if _, err := load(t, tc.cache); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}
