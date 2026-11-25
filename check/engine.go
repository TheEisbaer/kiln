package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// enginePackages are the packages whose tests may need the container engine
// (ADR 0049).
var enginePackages = []string{"internal/seal", "internal/run", "internal/tick"}

// engineVariable is the variable an engine test reads before it runs. It is
// put together, so that this file does not contain the literal it looks for.
const engineVariable = "KILN_ENGINE" + "_TESTS"

// engineStep is the name of the step that keeps the engine tests findable.
const engineStep = "the engine tests are found"

// The lines that name the engine packages outside the check program, each
// found by its prefix once trimmed.
const (
	inImageFile     = "check/in-image"
	inImagePrefix   = "for pkg in "
	inImageSuffix   = "; do"
	workflowFile    = ".github/workflows/check.yml"
	workflowPrefix  = "package: ["
	workflowSuffix  = "]"
	enginePkgsShown = "internal/seal, internal/run or internal/tick"
)

// engineScan is what scanEngineTests finds.
type engineScan struct {
	// tests holds, for each engine package, its engine tests in order.
	tests map[string][]string
	// stray holds, in order, file:line of every literal engineVariable that is
	// not in the body of a Test function of an engine package's test file.
	stray []string
}

// scanEngineTests walks fsys as dockerOutsideSeal does. An engine test is a
// top-level Test function in a _test.go file of an engine package whose body
// holds the string literal engineVariable. It stops a slip, not an evasion: a
// name put together at run time passes.
func scanEngineTests(fsys fs.FS) (engineScan, error) {
	scan := engineScan{tests: map[string][]string{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(p) != ".go" {
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		pkg := path.Dir(p)
		var allowed []*ast.FuncDecl
		if strings.HasSuffix(p, "_test.go") && slices.Contains(enginePackages, pkg) {
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Body != nil && isTestName(fn.Name.Name) {
					allowed = append(allowed, fn)
				}
			}
		}
		found := map[*ast.FuncDecl]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !namesEngineVariable(lit.Value) {
				return true
			}
			for _, fn := range allowed {
				if fn.Body.Pos() <= lit.Pos() && lit.End() <= fn.Body.End() {
					found[fn] = true
					return true
				}
			}
			scan.stray = append(scan.stray, fmt.Sprintf("%s:%d", p, fset.Position(lit.Pos()).Line))
			return true
		})
		for fn := range found {
			scan.tests[pkg] = append(scan.tests[pkg], fn.Name.Name)
		}
		return nil
	})
	for _, tests := range scan.tests {
		sort.Strings(tests)
	}
	sort.Strings(scan.stray)
	return scan, err
}

// namesEngineVariable reports whether a Go string literal, as written, is
// engineVariable.
func namesEngineVariable(lit string) bool {
	return lit == `"`+engineVariable+`"` || lit == "`"+engineVariable+"`"
}

// isTestName reports whether go test runs a function of this name as a test:
// Test, or Test followed by anything but a lower-case letter. TestMain is not
// a test.
func isTestName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Test")
	if !ok || name == "TestMain" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return rest == "" || !unicode.IsLower(r)
}

// engineTestsFound fails when the literal engineVariable is out of place,
// when an engine package has no engine test, and when check/in-image or the
// workflow's matrix names other packages than those with engine tests.
func engineTestsFound(fsys fs.FS) error {
	scan, err := scanEngineTests(fsys)
	if err != nil {
		return err
	}
	var problems []string
	if len(scan.stray) > 0 {
		problems = append(problems, fmt.Sprintf("%s is read only in the body of a Test function of %s, but appears in:\n%s",
			engineVariable, enginePkgsShown, strings.Join(scan.stray, "\n")))
	}
	var withTests []string
	for _, pkg := range enginePackages {
		if len(scan.tests[pkg]) == 0 {
			problems = append(problems, pkg+" has no engine test")
		} else {
			withTests = append(withTests, pkg)
		}
	}
	for _, l := range []struct{ file, prefix, suffix, sep string }{
		{inImageFile, inImagePrefix, inImageSuffix, " "},
		{workflowFile, workflowPrefix, workflowSuffix, ","},
	} {
		named, err := namedPackages(fsys, l.file, l.prefix, l.suffix, l.sep)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !slices.Equal(named, withTests) {
			problems = append(problems, fmt.Sprintf("%s names the engine packages %s, but the engine tests are in %s",
				l.file, strings.Join(named, " "), strings.Join(withTests, " ")))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	return nil
}

// namedPackages returns, sorted as enginePackages is, the packages on the
// first line of file that starts with prefix once trimmed.
func namedPackages(fsys fs.FS, file, prefix, suffix, sep string) ([]string, error) {
	src, err := fs.ReadFile(fsys, file)
	if err != nil {
		return nil, err
	}
	for line := range strings.SplitSeq(string(src), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !ok {
			continue
		}
		rest, ok = strings.CutSuffix(rest, suffix)
		if !ok {
			return nil, fmt.Errorf("%s: the line %q does not end in %q", file, strings.TrimSpace(line), suffix)
		}
		var named []string
		for f := range strings.SplitSeq(rest, sep) {
			if f = strings.TrimSpace(f); f != "" && !slices.Contains(named, f) {
				named = append(named, f)
			}
		}
		sortAsEnginePackages(named)
		return named, nil
	}
	return nil, fmt.Errorf("%s has no line starting with %q", file, prefix)
}

// sortAsEnginePackages puts the engine packages first, in their order, and
// anything else after them, in sorted order.
func sortAsEnginePackages(pkgs []string) {
	rank := func(p string) int {
		if i := slices.Index(enginePackages, p); i >= 0 {
			return i
		}
		return len(enginePackages)
	}
	slices.SortStableFunc(pkgs, func(a, b string) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(a, b)
	})
}

// printEngineTests writes, for each of pkgs that has engine tests, one line:
// the package and the -test.run pattern of its engine tests. A package may
// be written with a leading "./"; one without engine tests gets no line.
func printEngineTests(w io.Writer, fsys fs.FS, pkgs []string) error {
	scan, err := scanEngineTests(fsys)
	if err != nil {
		return err
	}
	for _, pkg := range pkgs {
		pkg = path.Clean(strings.TrimPrefix(pkg, "./"))
		if tests := scan.tests[pkg]; len(tests) > 0 {
			if _, err := fmt.Fprintf(w, "%s ^(%s)$\n", pkg, strings.Join(tests, "|")); err != nil {
				return err
			}
		}
	}
	return nil
}
