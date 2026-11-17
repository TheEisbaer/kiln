package main

import (
	"slices"
	"testing"
	"testing/fstest"
)

func TestDockerOnlyInSeal(t *testing.T) {
	const runs = "package p\n\nimport \"os/exec\"\n\nvar c = exec.Command(\"docker\", \"ps\")\n"
	src := func(lit string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("package p\n\nconst c = " + lit + "\n")}
	}
	fsys := fstest.MapFS{
		"internal/seal/seal.go":         {Data: []byte(runs)},
		"internal/seal/sub/x.go":        {Data: []byte(runs)},
		"internal/run/run.go":           {Data: []byte(runs)},
		"internal/run/run_test.go":      {Data: []byte(runs)},
		"internal/run/raw.go":           src("`docker`"),
		"internal/run/testdata/x/a.go":  {Data: []byte(runs)},
		"cmd/core/comment.go":           {Data: []byte("package main\n\n// docker ps is named in a comment only.\nconst c = 1\n")},
		"internal/sealed/x.go":          {Data: []byte(runs)},
		"internal/run/notgo.sh":         {Data: []byte("docker ps\n")},
		".git/hooks/x.go":               {Data: []byte(runs)},
		"internal/run/twice.go":         {Data: []byte("package p\n\nvar a, b = \"docker\", \"docker\"\n")},
		"internal/seal/testdata/p/m.go": {Data: []byte(runs)},
		// A shell line, a path, and both together.
		"internal/run/line.go":      src(`"docker ps --all"`),
		"internal/run/spaced.go":    src(`"  docker\tps"`),
		"internal/run/path.go":      src(`"/usr/bin/docker"`),
		"internal/run/pathline.go":  src(`"/usr/local/bin/docker rm x"`),
		"internal/run/relative.go":  src(`"bin/docker"`),
		"internal/run/notfirst.go":  src(`"the docker command"`),
		"internal/run/longer.go":    src(`"dockerd"`),
		"internal/run/pathlong.go":  src(`"/usr/bin/docker-compose"`),
		"internal/run/dockerish.go": src(`"Dockerfile"`),
		"internal/run/empty.go":     src(`""`),
	}
	got, err := dockerOutsideSeal(fsys)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"internal/run/line.go",
		"internal/run/path.go",
		"internal/run/pathline.go",
		"internal/run/raw.go",
		"internal/run/relative.go",
		"internal/run/run.go",
		"internal/run/spaced.go",
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
