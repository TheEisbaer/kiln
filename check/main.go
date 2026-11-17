// Command check is Kiln's one check command (ADR 0045). Run it from the repo's
// root, normally through check/in-image.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() {
	if _, err := os.Stat("go.mod"); err != nil {
		fmt.Fprintln(os.Stderr, "check: run it from the repo's root:", err)
		os.Exit(2)
	}

	failed := 0
	for _, s := range steps {
		fmt.Printf("== %s\n", s.name)
		if err := s.run(); err != nil {
			fmt.Printf("FAIL %s: %v\n", s.name, err)
			failed++
		}
	}
	if failed > 0 {
		fmt.Printf("check failed: %d of %d steps\n", failed, len(steps))
		os.Exit(1)
	}
	fmt.Println("check passed")
}

// dockerStep is the name of the step that keeps the docker command in the seal.
const dockerStep = "docker only in internal/seal"

// step is one step of the check.
type step struct {
	name string
	run  func() error
}

// steps are the check's steps, in order.
var steps = []step{
	{"format", formatted},
	{"vet", func() error { return run(nil, "go", "vet", "./...") }},
	// The core is built for the developer's machine and has no C code (ADR 0037).
	{"build for macOS on arm64", func() error {
		return run([]string{"GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=0"}, "go", "build", "-o", os.DevNull, "./...")
	}},
	{"test", func() error { return run(nil, "go", "test", "./...") }},
	{"dependencies", dependencies},
	{dockerStep, func() error { return dockerOnlyInSeal(os.DirFS(".")) }},
}

// run runs a command with the given extra environment, showing its output.
func run(env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// formatted fails when gofmt would change any file.
func formatted() error {
	var out bytes.Buffer
	cmd := exec.Command("gofmt", "-l", ".")
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	if files := strings.TrimSpace(out.String()); files != "" {
		return fmt.Errorf("not formatted, run gofmt -w:\n%s", files)
	}
	return nil
}

// dependencies applies the dependency rule (ADR 0051) to go.mod and
// docs/dependencies.md.
func dependencies() error {
	goMod, err := os.ReadFile("go.mod")
	if err != nil {
		return err
	}
	doc, err := os.ReadFile("docs/dependencies.md")
	if err != nil {
		return err
	}
	if problems := checkDependencies(string(goMod), string(doc)); len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	return nil
}
