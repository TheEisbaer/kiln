package main

import (
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
)

// scriptsDir holds the scripts that start and stop Kiln (ADR 0047).
const scriptsDir = "scripts"

// scriptsParse fails when a regular file under dir does not parse under
// sh -n or is not executable, and when dir is missing or holds no file, so
// that it cannot pass on nothing.
func scriptsParse(dir string) error {
	var problems []string
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		n++
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o111 == 0 {
			problems = append(problems, p+": not executable")
		}
		if out, err := exec.Command("sh", "-n", p).CombinedOutput(); err != nil {
			problems = append(problems, fmt.Sprintf("%s: sh -n: %v\n%s", p, err, strings.TrimSpace(string(out))))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s holds no file", dir)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	return nil
}
