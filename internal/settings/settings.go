// Package settings holds the instance's repos as the core knows them: for
// each, its name, its bare copy, its publish command, its check result
// command, its spec runs' list of hosts, its limit, its own toolchain, if
// any, the memory and CPUs of its runs, and which one is Kiln's own
// (ADR 0025); and the instance's limit (ADR 0014). The repos come from the
// settings file, which Load reads, or, for a core started from its flags,
// from the one repo the flags make (internal/tick).
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/TheEisbaer/kiln/internal/seal"
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
	// NeedsWorkItem is true when the repo's pull requests cannot merge
	// without a linked work item: a card of it is not published without a
	// work item number.
	NeedsWorkItem bool
	// Toolchain is the repo's own toolchain (ADR 0039), nil for a repo whose
	// runs use Kiln's images.
	Toolchain *Toolchain
	// Memory and CPUs are the limits of the repo's run containers, as
	// seal.CheckLimits takes them: "" and 0 are the seal's defaults.
	Memory string
	CPUs   int
}

// Toolchain is a repo's own toolchain: the Dockerfile in the instance, never
// in a repo, that its toolchain image is built from, with the repo's main as
// the context, and the paths in the repo whose change makes the image due
// again (internal/tick).
type Toolchain struct {
	// Dockerfile is the Dockerfile's absolute path.
	Dockerfile string
	// Inputs are paths in the repo, files or directories, in clean form.
	Inputs []string
	// Cache is the repo's package caches, nil for a repo without one.
	Cache *Cache
}

// Cache is a repo's package caches: the Dockerfile in the instance that the
// image kiln-cache-<repo> is built from, with the repo's main as the
// context, whose command fills the cache's volume; the paths in the repo
// whose change makes the cache due again; and the path where the repo's runs
// see it, read-only (internal/tick, internal/seal).
type Cache struct {
	// Dockerfile is the Dockerfile's absolute path.
	Dockerfile string
	// Inputs are paths in the repo, files or directories, in clean form.
	Inputs []string
	// Path is an absolute path that seal.CheckCachePath takes.
	Path string
}

// MaxName is the longest name a repo can have.
const MaxName = 40

var name = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// imageRepoName is the name of a repo with a toolchain, which becomes part of
// its images' names, kiln-toolchain-<repo> and kiln-run-<repo>: lower case,
// and not ending in a separator.
var imageRepoName = regexp.MustCompile(`^[a-z0-9-]*[a-z0-9]$`)

// Check refuses, with the reason, a list of repos that is empty, has a name
// that is empty, too long or not of ASCII letters, digits and -, has a name
// twice, has a repo with no copy or a limit below 0, has a toolchain on
// Kiln's own repo, whose toolchain is defined in its repo (ADR 0039, rule 1),
// or on a repo whose name is not lower-case letters, digits and - ending in a
// letter or digit, or does not mark exactly one repo as Kiln's own.
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
		case r.Toolchain != nil && r.Own:
			return fmt.Errorf("the repo %q is Kiln's own and has a toolchain; Kiln's own toolchain is defined in its repo", r.Name)
		case r.Toolchain != nil && !imageRepoName.MatchString(r.Name):
			return fmt.Errorf("the repo %q has a toolchain, so its name is lower-case letters, digits and - ending in a letter or digit", r.Name)
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
	// WorkItem is "required" or left out.
	WorkItem string `json:"work_item"`
	// Limit is read as a pointer, so that a missing member and 0 differ.
	Limit     *int           `json:"limit"`
	Toolchain *fileToolchain `json:"toolchain"`
	// Memory and CPUs are read as they are written, so that a missing member
	// and "" or 0 differ, and a value of the wrong type is refused with the
	// repo's name (limitsOf).
	Memory json.RawMessage `json:"memory"`
	CPUs   json.RawMessage `json:"cpus"`
}

type fileToolchain struct {
	Dockerfile string   `json:"dockerfile"`
	Inputs     []string `json:"inputs"`
	// Cache is read on its own (toolchainOf), so that a member it does not
	// know is refused with the repo's name.
	Cache json.RawMessage `json:"cache"`
}

type fileCache struct {
	Dockerfile string   `json:"dockerfile"`
	Inputs     []string `json:"inputs"`
	Path       string   `json:"path"`
}

// toolchainOf is a repo's toolchain as the file gives it, or nil, refused
// with the reason when its cache has a member it does not know or a value of
// the wrong type, when its Dockerfile, or its cache's, is empty or not an
// absolute path, an input of either is empty, absolute, not in clean form, .,
// or .. or below it, or seal.CheckCachePath refuses the cache's path.
func toolchainOf(repo string, t *fileToolchain) (*Toolchain, error) {
	if t == nil {
		return nil, nil
	}
	if err := checkBuilt(repo, "toolchain", t.Dockerfile, t.Inputs); err != nil {
		return nil, err
	}
	tc := &Toolchain{Dockerfile: t.Dockerfile, Inputs: t.Inputs}
	if given(t.Cache) {
		dec := json.NewDecoder(bytes.NewReader(t.Cache))
		dec.DisallowUnknownFields()
		var c fileCache
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("the cache of the repo %q: %w", repo, err)
		}
		if err := checkBuilt(repo, "cache", c.Dockerfile, c.Inputs); err != nil {
			return nil, err
		}
		if err := seal.CheckCachePath(c.Path); err != nil {
			return nil, fmt.Errorf("the repo %q: %w", repo, err)
		}
		tc.Cache = &Cache{Dockerfile: c.Dockerfile, Inputs: c.Inputs, Path: c.Path}
	}
	return tc, nil
}

// checkBuilt refuses the Dockerfile and inputs of a repo's toolchain or
// cache, what, as toolchainOf says.
func checkBuilt(repo, what, dockerfile string, inputs []string) error {
	if dockerfile == "" || !filepath.IsAbs(dockerfile) {
		return fmt.Errorf("the %s's Dockerfile of the repo %q is not an absolute path: %q", what, repo, dockerfile)
	}
	for _, in := range inputs {
		if in == "" || path.IsAbs(in) || path.Clean(in) != in || in == "." || in == ".." || strings.HasPrefix(in, "../") {
			return fmt.Errorf("the %s's input %q of the repo %q is not a path in the repo in clean form", what, in, repo)
		}
	}
	return nil
}

// limitsOf is a repo's memory and CPUs as the file gives them, "" and 0 when
// it leaves them out or gives null, refused with the reason when they are not
// a string and a whole number, when they are written out as "" or 0, or when
// seal.CheckLimits refuses them.
func limitsOf(r fileRepo) (string, int, error) {
	memory, cpus := "", 0
	if given(r.Memory) {
		if err := json.Unmarshal(r.Memory, &memory); err != nil {
			return "", 0, fmt.Errorf("the memory is %s; it is a string, <n>m or <n>g", r.Memory)
		}
		if memory == "" {
			return "", 0, errors.New(`the memory is ""; it is <n>m or <n>g, or left out`)
		}
	}
	if given(r.CPUs) {
		if err := json.Unmarshal(r.CPUs, &cpus); err != nil {
			return "", 0, fmt.Errorf("the CPUs are %s; they are a whole number from 1", r.CPUs)
		}
		if cpus == 0 {
			return "", 0, errors.New("the CPUs are 0; they are a whole number from 1, or left out")
		}
	}
	return memory, cpus, seal.CheckLimits(memory, cpus)
}

// given is false for a member that is missing or null.
func given(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

// Load reads the settings file at path, one JSON object with the member
// repos and the optional member limit, and returns its repos in order, after
// Check, and the instance's limit. It refuses, with the reason, a file over 1
// MiB, a member it does not know, a value of the wrong type, anything after
// the object, a copy that is not an absolute path, and a limit, of a repo or
// of the instance, below 1, a work item other than "required", a toolchain or
// a cache whose Dockerfile is not an absolute path or whose input is not a
// path in the repo in clean form, and a cache's path that
// seal.CheckCachePath refuses; it does not read the Dockerfiles. It refuses a memory or
// CPUs that seal.CheckLimits refuses, and also "" and 0 written out. A repo
// without a limit has Limit 0, the default, and so has the instance; one
// without a memory or CPUs has "" or 0, the seal's defaults. It only reads.
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
		if r.WorkItem != "" && r.WorkItem != "required" {
			return Settings{}, fmt.Errorf("the settings file %s: the work item of the repo %q is %q; it is \"required\" or left out", path, r.Name, r.WorkItem)
		}
		toolchain, err := toolchainOf(r.Name, r.Toolchain)
		if err != nil {
			return Settings{}, fmt.Errorf("the settings file %s: %w", path, err)
		}
		memory, cpus, err := limitsOf(r)
		if err != nil {
			return Settings{}, fmt.Errorf("the settings file %s: the repo %q: %w", path, r.Name, err)
		}
		repos = append(repos, Repo{Name: r.Name, Copy: r.Copy, Own: r.Own, PublishCommand: r.PublishCommand,
			CheckResultCommand: r.CheckResultCommand, SpecHosts: r.SpecHosts, Limit: limit,
			NeedsWorkItem: r.WorkItem == "required", Toolchain: toolchain, Memory: memory, CPUs: cpus})
	}
	if err := Check(repos); err != nil {
		return Settings{}, fmt.Errorf("the settings file %s: %w", path, err)
	}
	return Settings{Repos: repos, Limit: instance}, nil
}
