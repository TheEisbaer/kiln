package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

func env(name string) string {
	if name == upstreamVar || name == commandVar {
		return "x"
	}
	return ""
}

func settingsFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const oneRepo = `{"repos":[{"name":"kiln","copy":"/a/kiln.git","own":true,"spec_hosts":["*"]}]}`
const twoRepos = `{"repos":[{"name":"kiln","copy":"/a/kiln.git","own":true},{"name":"work","copy":"/a/work.git"}]}`

func TestCoreConfig(t *testing.T) {
	core := func(args ...string) (code int, out, errs string, repos int, repo string) {
		var o, e bytes.Buffer
		cfg, code := coreConfig(append([]string{"--data", "/d", "--model", "m"}, args...), env, &o, &e)
		return code, o.String(), e.String(), len(cfg.Repos), cfg.Repo
	}
	good := settingsFile(t, oneRepo)

	code, out, _, repos, repo := core("--settings", good)
	if code != 0 || repos != 1 || repo != "" || !strings.Contains(out, "the repos are read from "+good+": kiln (Kiln's own)") {
		t.Errorf("--settings: %d, %d repos, repo %q, %q", code, repos, repo, out)
	}
	for _, extra := range [][]string{{"--repo", "x"}, {"--publish-command", ""}, {"--check-result-command", "c"}, {"--spec-hosts", "*"}} {
		code, _, errs, _, _ := core(append([]string{"--settings", good}, extra...)...)
		if code != 2 || !strings.Contains(errs, "each setting has one place") || !strings.Contains(errs, extra[0]) {
			t.Errorf("--settings with %v: %d %q, want 2 and the flag named", extra, code, errs)
		}
	}
	if code, _, errs, _, _ := core("--settings", good, "--spec-hosts", "x", "--repo", "y"); !strings.Contains(errs, "--settings and --repo, --spec-hosts are both given") {
		t.Errorf("order of flags named: %d %q", code, errs)
	}
	if code, _, _, _, _ := core(); code != 2 {
		t.Errorf("neither --repo nor --settings: %d, want 2", code)
	}
	if code, _, _, _, _ := core("--settings", ""); code != 2 {
		t.Errorf("an empty --settings: %d, want 2", code)
	}
	if code, _, errs, _, _ := core("--settings", settingsFile(t, `{"repos":[{"nme":"x"}]}`)); code != 1 || !strings.Contains(errs, "the settings file") {
		t.Errorf("a broken file: %d %q, want 1", code, errs)
	}
	t.Run("a relative --settings", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(oneRepo), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		code, _, errs, repos, _ := core("--settings", "settings.json")
		if code != 2 || !strings.Contains(errs, "is not an absolute path") || repos != 0 {
			t.Errorf("a relative --settings: %d %q, %d repos, want 2, the reason and none", code, errs, repos)
		}
	})
	var o, e bytes.Buffer
	cfg, code := coreConfig([]string{"--data", "/d", "--model", "m", "--repo", "/r.git", "--spec-hosts", "a.org,b.org"}, env, &o, &e)
	if code != 0 || len(cfg.Repos) != 0 || cfg.Repo != "/r.git" || len(cfg.Hosts) != 2 || o.Len() != 0 {
		t.Errorf("flags only: %d %+v %q", code, cfg, o.String())
	}
}

func TestCardRepo(t *testing.T) {
	card := func(t *testing.T, args ...string) (int, string, string, string) {
		t.Helper()
		data := t.TempDir()
		var o, e bytes.Buffer
		code := cardCommand(append([]string{"--data", data}, append(args, "A card.")...), &o, &e)
		return code, e.String(), data, o.String()
	}
	repoOfOnly := func(t *testing.T, data string) string {
		t.Helper()
		s, err := store.Open(filepath.Join(data, "store.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		board, err := s.Board()
		if err != nil || len(board) != 1 {
			t.Fatalf("the board has %d cards, %v", len(board), err)
		}
		c, err := s.Card(board[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		return c.Repo
	}
	noCard := func(t *testing.T, data string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(data, "store.db")); err == nil {
			s, err := store.Open(filepath.Join(data, "store.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if b, _ := s.Board(); len(b) != 0 {
				t.Errorf("a refused kiln card wrote %d cards", len(b))
			}
		}
	}
	if code, errs, data, _ := card(t); code != 0 || repoOfOnly(t, data) != "kiln" {
		t.Errorf("without --settings: %d %q", code, errs)
	}
	if code, errs, data, _ := card(t, "--settings", settingsFile(t, `{"repos":[{"name":"kiln-own","copy":"/a/k.git","own":true}]}`)); code != 0 || repoOfOnly(t, data) != "kiln-own" {
		t.Errorf("one repo: %d %q", code, errs)
	}
	two := settingsFile(t, twoRepos)
	code, errs, data, _ := card(t, "--settings", two)
	if code != 1 || !strings.Contains(errs, "name one with --repo: kiln, work") {
		t.Errorf("two repos: %d %q", code, errs)
	}
	noCard(t, data)
	if code, errs, data, _ := card(t, "--settings", two, "--repo", "work"); code != 0 || repoOfOnly(t, data) != "work" {
		t.Errorf("--repo work: %d %q", code, errs)
	}
	code, errs, data, _ = card(t, "--settings", two, "--repo", "nope")
	if code != 1 || !strings.Contains(errs, `the settings have no repo "nope"; their repos: kiln, work`) {
		t.Errorf("--repo nope: %d %q", code, errs)
	}
	noCard(t, data)
	if code, _, data, _ := card(t, "--repo", "work"); code != 1 {
		t.Errorf("--repo work without a file: %d", code)
	} else {
		noCard(t, data)
	}
}
