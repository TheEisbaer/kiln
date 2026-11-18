package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// sealDir is the one package that runs the docker command (ADR 0047).
const sealDir = "internal/seal"

// dockerCommand is the name looked for. It is put together, so that this file
// does not contain the literal it looks for.
const dockerCommand = "dock" + "er"

// dockerOutsideSeal returns, in a stable order, every Go file in fsys outside
// internal/seal/ that is not a test and contains a string literal that names
// the docker command (namesDocker). It stops a slip, not an evasion: a name put
// together at run time passes.
func dockerOutsideSeal(fsys fs.FS) ([]string, error) {
	var found []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == sealDir || (p != "." && strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(p) != ".go" || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), p, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		named := false
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil && namesDocker(v) {
					named = true
				}
			}
			return !named
		})
		if named {
			found = append(found, p)
		}
		return nil
	})
	sort.Strings(found)
	return found, err
}

// namesDocker reports whether a string literal names the docker command: its
// first word is "docker", as in "docker" or a shell line "docker ps", or a path
// that ends in "/docker", as in "/usr/bin/docker".
func namesDocker(s string) bool {
	words := strings.Fields(s)
	return len(words) > 0 && (words[0] == dockerCommand || strings.HasSuffix(words[0], "/"+dockerCommand))
}

// dockerOnlyInSeal fails when a Go file outside internal/seal/ that is not a
// test contains a string literal that names the docker command.
func dockerOnlyInSeal(fsys fs.FS) error {
	found, err := dockerOutsideSeal(fsys)
	if err != nil {
		return err
	}
	if len(found) > 0 {
		return fmt.Errorf("only %s/ runs the docker command, but these files name it:\n%s", sealDir, strings.Join(found, "\n"))
	}
	return nil
}
