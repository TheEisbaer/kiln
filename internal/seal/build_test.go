package seal

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestBuild is a large test (ADR 0031, ADR 0048): it needs the container
// engine and the toolchain image. It builds a small module's cmd/core from a
// tar on standard input, and the program prints the version it was given. A
// container of the build's name without the label is refused and left. No
// build container is left afterwards.
//
// It runs only with KILN_ENGINE_TESTS=1, and then fails, not skips, when the
// engine cannot be reached.
func TestBuild(t *testing.T) {
	if os.Getenv("KILN_ENGINE_TESTS") != "1" {
		t.Skip("needs the container engine; set KILN_ENGINE_TESTS=1")
	}
	if _, err := docker(t.Context(), nil, "info", "--format", "{{.ServerVersion}}"); err != nil {
		t.Fatalf("the container engine is not reachable: %v", err)
	}
	failIfStopped(t)
	instance := randomID(t)
	t.Cleanup(func() {
		if out, err := docker(context.Background(), nil, "ps", "--all", "--format", "{{.Names}}"); err != nil {
			t.Error(err)
		} else if strings.Contains(out, buildName(instance)) {
			t.Errorf("a build container is left: %s", out)
		}
	})
	version := strings.Repeat("0123456789abcdef", 3)[:40]
	p := Program{Instance: instance, Image: toolchainImage, Version: version, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}

	t.Run("built", func(t *testing.T) {
		var out bytes.Buffer
		if err := Build(t.Context(), p, bytes.NewReader(sourceTar(t)), &out); err != nil {
			t.Fatal(err)
		}
		program := filepath.Join(t.TempDir(), "kiln")
		if err := os.WriteFile(program, out.Bytes(), 0o700); err != nil {
			t.Fatal(err)
		}
		got, err := exec.Command(program).Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != version {
			t.Errorf("the program printed %q, want %q", got, version)
		}
	})

	t.Run("not ours", func(t *testing.T) {
		name := buildName(instance)
		must(t, "run", "--detach", "--name", name, toolchainImage, "sleep", "infinity")
		cleanup(t, "rm", "--force", name)
		err := Build(t.Context(), p, bytes.NewReader(sourceTar(t)), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "has no label kiln.build=") {
			t.Errorf("a build beside a container that is not Kiln's: %v", err)
		}
		if state := must(t, "inspect", "--format", "{{.State.Status}}", name); state != "running" {
			t.Errorf("the container that is not Kiln's is %s", state)
		}
	})
}

// sourceTar is a tar of a module with a cmd/core that prints its version.
func sourceTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, text := range map[string]string{
		"go.mod":           "module example.com/v\n\ngo 1.27\n",
		"cmd/core/main.go": "package main\n\nimport \"fmt\"\n\nvar version string\n\nfunc main() { fmt.Println(version) }\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(text))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
