// Package settings holds the instance's repos as the core knows them: for
// each, its name, its bare copy, its publish command, its check result
// command and its spec runs' list of hosts, and which one is Kiln's own
// (ADR 0025).
package settings

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Repo is one of the instance's repos.
type Repo struct {
	// Name is the repo's name: ASCII letters, digits and -, at most MaxName
	// characters, unique in the instance.
	Name string
	// Copy is the path of the instance's bare copy of the repo.
	Copy               string
	PublishCommand     string
	CheckResultCommand string
	// SpecHosts are the hosts the repo's spec runs may reach (ADR 0017).
	SpecHosts []string
	// Own is true for Kiln's own repo, whose merges update the core
	// (ADR 0019, ADR 0037).
	Own bool
}

// MaxName is the longest name a repo can have.
const MaxName = 40

var name = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// Check refuses, with the reason, a list of repos that is empty, has a name
// that is empty, too long or not of ASCII letters, digits and -, has a name
// twice, has a repo with no copy, or does not mark exactly one repo as Kiln's
// own.
func Check(repos []Repo) error {
	if len(repos) == 0 {
		return errors.New("the instance has no repo")
	}
	seen := map[string]bool{}
	var own []string
	for i, r := range repos {
		switch {
		case r.Name == "":
			return fmt.Errorf("repo %d has no name", i+1)
		case len(r.Name) > MaxName:
			return fmt.Errorf("the repo name %q is longer than %d characters", r.Name, MaxName)
		case !name.MatchString(r.Name):
			return fmt.Errorf("the repo name %q is not only ASCII letters, digits and -", r.Name)
		case seen[r.Name]:
			return fmt.Errorf("the repo name %q is given twice", r.Name)
		case r.Copy == "":
			return fmt.Errorf("the repo %q has no copy", r.Name)
		}
		seen[r.Name] = true
		if r.Own {
			own = append(own, r.Name)
		}
	}
	switch len(own) {
	case 0:
		return errors.New("no repo is marked as Kiln's own")
	case 1:
		return nil
	default:
		return fmt.Errorf("more than one repo is marked as Kiln's own: %s", strings.Join(own, ", "))
	}
}
