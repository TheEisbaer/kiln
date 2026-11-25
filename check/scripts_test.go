package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScriptsParse(t *testing.T) {
	write := func(t *testing.T, script string, mode os.FileMode) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "s"), []byte(script), mode); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	if err := scriptsParse(write(t, "#!/bin/sh\nif true; then echo ok; fi\n", 0o755)); err != nil {
		t.Errorf("a valid script: %v", err)
	}
	if err := scriptsParse(write(t, "#!/bin/sh\nif true; then\n", 0o755)); err == nil || !strings.Contains(err.Error(), "sh -n") {
		t.Errorf("a syntax error: %v", err)
	}
	if err := scriptsParse(write(t, "#!/bin/sh\necho ok\n", 0o644)); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Errorf("not executable: %v", err)
	}
	if err := scriptsParse(t.TempDir()); err == nil {
		t.Error("an empty directory: no error")
	}
	if err := scriptsParse(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing directory: no error")
	}
}

// TestKilnUpRefusesInClaudeCode runs the start script inside what looks like
// a Claude Code session, with the upstream and the credential command both in
// the environment and in the settings: it refuses first, and prints neither.
func TestKilnUpRefusesInClaudeCode(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"env": {"ANTHROPIC_BASE_URL": "https://upstream-marker-settings.invalid"}, "apiKeyHelper": "echo command-marker-settings"}`
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "../"+scriptsDir+"/kiln-up")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"CLAUDECODE=1",
		"KILN_CLAUDE_UPSTREAM=https://upstream-marker-env.invalid",
		"KILN_CLAUDE_CREDENTIAL_COMMAND=echo command-marker-env",
	}
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
		t.Errorf("exit: %v, want status 2", err)
	}
	if !strings.Contains(string(out), "inside a Claude Code session") {
		t.Errorf("output %q does not say why", out)
	}
	for _, marker := range []string{"upstream-marker-settings", "command-marker-settings", "upstream-marker-env", "command-marker-env"} {
		if strings.Contains(string(out), marker) {
			t.Errorf("output %q holds %s", out, marker)
		}
	}
}

// TestKilnUpMakesSettingsAbsolute runs the start script up to its source
// check, which fails, and reads the line that names the settings file.
func TestKilnUpMakesSettingsAbsolute(t *testing.T) {
	script, err := filepath.Abs("../" + scriptsDir + "/kiln-up")
	if err != nil {
		t.Fatal(err)
	}
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(t *testing.T, dir string, extra ...string) string {
		t.Helper()
		cmd := exec.Command("sh", script)
		cmd.Dir = dir
		cmd.Env = append([]string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + t.TempDir(),
			"KILN_SOURCE=" + filepath.Join(t.TempDir(), "missing"),
		}, extra...)
		out, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
			t.Errorf("exit: %v, want status 2; output %q", err, out)
		}
		if !strings.Contains(string(out), "is not Kiln's source") {
			t.Errorf("output %q does not stop at the source check", out)
		}
		return string(out)
	}
	lines := func(out string) (n int, got string) {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "kiln-up: the settings file is ") || !strings.HasPrefix(l, "kiln-up: ") && l != "" {
				n++
				got = l
			}
		}
		return
	}

	t.Run("a relative path", func(t *testing.T) {
		w := t.TempDir()
		write(filepath.Join(w, "sub", "settings.json"))
		other := t.TempDir()
		write(filepath.Join(other, "sub", "settings.json"))
		real, err := filepath.EvalSymlinks(w)
		if err != nil {
			t.Fatal(err)
		}
		out := run(t, w, "KILN_SETTINGS=sub/settings.json", "CDPATH="+other)
		want := "kiln-up: the settings file is " + real + "/sub/settings.json"
		if n, got := lines(out); n != 1 || got != want {
			t.Errorf("output %q, want exactly the line %q", out, want)
		}
	})
	t.Run("an absolute path", func(t *testing.T) {
		abs := filepath.Join(t.TempDir(), "settings.json")
		write(abs)
		out := run(t, t.TempDir(), "KILN_SETTINGS="+abs)
		want := "kiln-up: the settings file is " + abs
		if n, got := lines(out); n != 1 || got != want {
			t.Errorf("output %q, want exactly the line %q", out, want)
		}
	})
}
