package main

import (
	"slices"
	"testing"
	"testing/fstest"
)

func TestDockerOnlyInSeal(t *testing.T) {
	const runs = "package p\n\nimport \"os/exec\"\n\nvar c = exec.Command(\"docker\", \"ps\")\n"
	fsys := fstest.MapFS{
		"internal/seal/seal.go":         {Data: []byte(runs)},
		"internal/seal/sub/x.go":        {Data: []byte(runs)},
		"internal/run/run.go":           {Data: []byte(runs)},
		"internal/run/run_test.go":      {Data: []byte(runs)},
		"internal/run/raw.go":           {Data: []byte("package p\n\nconst c = `docker`\n")},
		"internal/run/testdata/x/a.go":  {Data: []byte(runs)},
		"cmd/core/main.go":              {Data: []byte("package main\n\n// docker is named in a comment only.\nconst c = \"docker ps\"\n")},
		"internal/sealed/x.go":          {Data: []byte(runs)},
		"internal/run/notgo.sh":         {Data: []byte("docker ps\n")},
		".git/hooks/x.go":               {Data: []byte(runs)},
		"internal/run/twice.go":         {Data: []byte("package p\n\nvar a, b = \"docker\", \"docker\"\n")},
		"internal/seal/testdata/p/m.go": {Data: []byte(runs)},
	}
	got, err := dockerOutsideSeal(fsys)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"internal/run/raw.go",
		"internal/run/run.go",
		"internal/run/testdata/x/a.go",
		"internal/run/twice.go",
		"internal/sealed/x.go",
	}
	if !slices.Equal(got, want) {
		t.Errorf("found %q, want %q", got, want)
	}
	if err := dockerOnlyInSeal(fsys); err == nil {
		t.Error("dockerOnlyInSeal: no error")
	}
	if err := dockerOnlyInSeal(fstest.MapFS{"internal/seal/seal.go": {Data: []byte(runs)}}); err != nil {
		t.Errorf("dockerOnlyInSeal with the seal alone: %v", err)
	}
}

// The step is one of the check's steps, so removing it fails here.
func TestDockerStepIsAStep(t *testing.T) {
	if !slices.ContainsFunc(steps, func(s step) bool { return s.name == dockerStep && s.run != nil }) {
		t.Errorf("the check has no step %q", dockerStep)
	}
}
