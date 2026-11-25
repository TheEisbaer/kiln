package main

import (
	"bytes"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// engineTest is a test of the shape the engine tests have.
func engineTest(name string) string {
	return "func " + name + "(t *testing.T) {\n\tif os.Getenv(\"" + engineVariable + "\") != \"1\" {\n\t\tt.Skip(\"needs the engine\")\n\t}\n}\n"
}

// testFile is a Go test file of package p with the given functions.
func testFile(funcs ...string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("package p\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" + strings.Join(funcs, "\n"))}
}

const (
	goodInImage  = "#!/bin/sh\n    for pkg in internal/seal internal/run internal/tick; do\n    done\n"
	goodWorkflow = "jobs:\n  engine:\n    strategy:\n      matrix:\n        package: [internal/seal, internal/run, internal/tick]\n"
)

// engineRepo is a repo whose engine tests are those of Kiln's repo, in their
// shape, plus the given files.
func engineRepo(extra fstest.MapFS) fstest.MapFS {
	fsys := fstest.MapFS{
		"internal/seal/build_test.go": testFile(engineTest("TestBuild"), "func TestHelper(t *testing.T) {}\n"),
		"internal/seal/image_test.go": testFile(engineTest("TestImage")),
		"internal/seal/run_test.go":   testFile(engineTest("TestRun")),
		"internal/seal/seal_test.go":  testFile(engineTest("TestSeal"), "func TestMain(m *testing.M) { os.Exit(m.Run()) }\n"),
		"internal/run/run_test.go":    testFile(engineTest("TestSteps")),
		"internal/tick/engine_test.go": testFile(engineTest("TestTick"),
			// A unit test that names the variable only in a longer string.
			"func TestRestore(t *testing.T) { _ = \"set "+engineVariable+"=1\" }\n"),
		"internal/tick/tick.go":  {Data: []byte("package p\n\nconst c = \"internal/tick\"\n")},
		inImageFile:              {Data: []byte(goodInImage)},
		workflowFile:             {Data: []byte(goodWorkflow)},
		".git/hooks/x.go":        {Data: []byte("package p\n\nconst c = \"" + engineVariable + "\"\n")},
		"internal/seal/notgo.sh": {Data: []byte(engineVariable + "=1 go test\n")},
	}
	maps.Copy(fsys, extra)
	return fsys
}

func TestEngineTestsFound(t *testing.T) {
	scan, err := scanEngineTests(engineRepo(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"internal/seal": {"TestBuild", "TestImage", "TestRun", "TestSeal"},
		"internal/run":  {"TestSteps"},
		"internal/tick": {"TestTick"},
	}
	if !maps.EqualFunc(scan.tests, want, slices.Equal) || len(scan.stray) > 0 {
		t.Errorf("found %q and stray %q, want %q and none", scan.tests, scan.stray, want)
	}
	if err := engineTestsFound(engineRepo(nil)); err != nil {
		t.Errorf("engineTestsFound on the six tests: %v", err)
	}

	for _, c := range []struct {
		name  string
		extra fstest.MapFS
		want  string // a part of the error
	}{
		{"the literal in a helper", fstest.MapFS{
			"internal/seal/helper_test.go": testFile("func needsEngine(t *testing.T) bool {\n\treturn os.Getenv(\"" + engineVariable + "\") == \"1\"\n}\n"),
		}, "internal/seal/helper_test.go:9"},
		{"the literal in a method named Test", fstest.MapFS{
			"internal/seal/method_test.go": testFile("type s struct{}\n\nfunc (s) TestX(t *testing.T) { _ = `" + engineVariable + "` }\n"),
		}, "internal/seal/method_test.go:"},
		{"the literal in TestMain", fstest.MapFS{
			"internal/run/main_test.go": testFile("func TestMain(m *testing.M) { _ = \"" + engineVariable + "\" }\n"),
		}, "internal/run/main_test.go:"},
		{"the literal outside a function", fstest.MapFS{
			"internal/run/var_test.go": testFile("var v = os.Getenv(\"" + engineVariable + "\")\n"),
		}, "internal/run/var_test.go:"},
		{"the literal in a Test of another package", fstest.MapFS{
			"internal/web/web_test.go": testFile(engineTest("TestWeb")),
		}, "internal/web/web_test.go:"},
		{"the literal in a subpackage", fstest.MapFS{
			"internal/seal/sub/x_test.go": testFile(engineTest("TestSub")),
		}, "internal/seal/sub/x_test.go:"},
		{"the literal in a file that is not a test", fstest.MapFS{
			"internal/seal/seal.go": {Data: []byte("package p\n\nimport \"os\"\n\nvar v = os.Getenv(\"" + engineVariable + "\")\n")},
		}, "internal/seal/seal.go:5"},
		{"a package without an engine test", fstest.MapFS{
			"internal/run/run_test.go": testFile("func TestSteps(t *testing.T) {}\n"),
		}, "internal/run has no engine test"},
		{"a matrix line without internal/tick", fstest.MapFS{
			workflowFile: {Data: []byte(strings.Replace(goodWorkflow, ", internal/tick]", "]", 1))},
		}, workflowFile + " names the engine packages internal/seal internal/run, but the engine tests are in internal/seal internal/run internal/tick"},
		{"an in-image line with a fourth package", fstest.MapFS{
			inImageFile: {Data: []byte(strings.Replace(goodInImage, "internal/tick;", "internal/tick internal/web;", 1))},
		}, inImageFile + " names the engine packages internal/seal internal/run internal/tick internal/web"},
		{"no matrix line", fstest.MapFS{
			workflowFile: {Data: []byte("jobs:\n")},
		}, workflowFile + " has no line"},
		{"an in-image line that does not end as it should", fstest.MapFS{
			inImageFile: {Data: []byte("for pkg in internal/seal internal/run internal/tick\n")},
		}, "does not end in"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := engineTestsFound(engineRepo(c.extra))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("engineTestsFound: %v, want an error with %q", err, c.want)
			}
		})
	}

	// The same packages in another order and with other spacing pass.
	if err := engineTestsFound(engineRepo(fstest.MapFS{
		workflowFile: {Data: []byte("        package: [internal/tick,internal/seal,  internal/run]\n")},
		inImageFile:  {Data: []byte("for pkg in internal/run  internal/tick internal/seal; do\n")},
	})); err != nil {
		t.Errorf("engineTestsFound with the packages reordered: %v", err)
	}
}

func TestPrintEngineTests(t *testing.T) {
	var out bytes.Buffer
	if err := printEngineTests(&out, engineRepo(nil), []string{"./internal/seal", "internal/tick", "internal/web"}); err != nil {
		t.Fatal(err)
	}
	want := "internal/seal ^(TestBuild|TestImage|TestRun|TestSeal)$\ninternal/tick ^(TestTick)$\n"
	if out.String() != want {
		t.Errorf("printed %q, want %q", out.String(), want)
	}
}

// The step is one of the check's steps, so removing it fails here.
func TestEngineStepIsAStep(t *testing.T) {
	if !slices.ContainsFunc(steps, func(s step) bool { return s.name == engineStep && s.run != nil }) {
		t.Errorf("the check has no step %q", engineStep)
	}
}
