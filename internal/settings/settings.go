// Package settings holds the instance's repos as the core knows them: for
// each, its name, its bare copy, its publish command, its check result
// command, its spec runs' list of hosts and its limit, and which one is
// Kiln's own (ADR 0025); and the instance's limit (ADR 0014). The repos come from the settings file, which Load reads, or,
// for a core started from its flags, from the one repo the flags make
// (internal/tick).
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Repo is one of the instance's repos, read from the settings file by Load,
// or made from the flags of a core that has no file.
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
	// Limit is the most cards of the repo in progress at once (ADR 0014);
	// 0 is the default, store.DefaultLimit.
	Limit int
}

// MaxName is the longest name a repo can have.
const MaxName = 40

var name = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// Check refuses, with the reason, a list of repos that is empty, has a name
// that is empty, too long or not of ASCII letters, digits and -, has a name
// twice, has a repo with no copy or a limit below 0, or does not mark exactly
// one repo as Kiln's own.
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
		case r.Limit < 0:
			return fmt.Errorf("the limit of the repo %q is %d; it is at least 1", r.Name, r.Limit)
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

// MaxFile is the largest settings file Load reads.
const MaxFile = 1 << 20

// Settings are what the settings file holds: the instance's repos, in order,
// and the instance's limit of cards in progress that are not ready to merge
// (ADR 0014), 0 when the file leaves it out, for the default,
// store.DefaultLimit.
type Settings struct {
	Repos []Repo
	Limit int
}

// file is the shape of the settings file. The top level is an object so that
// the instance's limit is beside repos.
type file struct {
	Repos []fileRepo `json:"repos"`
	// Limit is read as a pointer, so that a missing member and 0 differ.
	Limit *int `json:"limit"`
}

type fileRepo struct {
	Name               string   `json:"name"`
	Copy               string   `json:"copy"`
	Own                bool     `json:"own"`
	PublishCommand     string   `json:"publish_command"`
	CheckResultCommand string   `json:"check_result_command"`
	SpecHosts          []string `json:"spec_hosts"`
	// Limit is read as a pointer, so that a missing member and 0 differ.
	Limit *int `json:"limit"`
}

// Load reads the settings file at path, one JSON object with the member
// repos and the optional member limit, and returns its repos in order, after
// Check, and the instance's limit. It refuses, with the reason, a file over 1
// MiB, a member it does not know, a value of the wrong type, anything after
// the object, a copy that is not an absolute path, and a limit, of a repo or
// of the instance, below 1. A repo without a limit has Limit 0, the default,
// and so has the instance. It only reads.
func Load(path string) (Settings, error) {
	f, err := os.Open(path)
	if err != nil {
		return Settings{}, fmt.Errorf("the settings file %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	if err != nil {
		return Settings{}, fmt.Errorf("the settings file %s: %w", path, err)
	}
	if len(data) > MaxFile {
		return Settings{}, fmt.Errorf("the settings file %s is larger than 1 MiB", path)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var in file
	if err := dec.Decode(&in); err != nil {
		return Settings{}, fmt.Errorf("the settings file %s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Settings{}, fmt.Errorf("the settings file %s: there is more after the object", path)
	}
	instance := 0
	if in.Limit != nil {
		if *in.Limit < 1 {
			return Settings{}, fmt.Errorf("the settings file %s: the instance's limit is %d; it is at least 1", path, *in.Limit)
		}
		instance = *in.Limit
	}
	repos := make([]Repo, 0, len(in.Repos))
	for _, r := range in.Repos {
		if r.Copy != "" && !filepath.IsAbs(r.Copy) {
			return Settings{}, fmt.Errorf("the settings file %s: the copy of the repo %q is not an absolute path: %q", path, r.Name, r.Copy)
		}
		limit := 0
		if r.Limit != nil {
			if *r.Limit < 1 {
				return Settings{}, fmt.Errorf("the settings file %s: the limit of the repo %q is %d; it is at least 1", path, r.Name, *r.Limit)
			}
			limit = *r.Limit
		}
		repos = append(repos, Repo{Name: r.Name, Copy: r.Copy, Own: r.Own, PublishCommand: r.PublishCommand,
			CheckResultCommand: r.CheckResultCommand, SpecHosts: r.SpecHosts, Limit: limit})
	}
	if err := Check(repos); err != nil {
		return Settings{}, fmt.Errorf("the settings file %s: %w", path, err)
	}
	return Settings{Repos: repos, Limit: instance}, nil
}
