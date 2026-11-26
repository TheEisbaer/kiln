package seal

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A repo's package caches (ADR 0038, ADR 0039). A repo with a toolchain may
// name a cache: a Dockerfile, built from main into the image
// kiln-cache-<repo> (internal/tick), whose command fills the volume
// kiln-cache-<repo>-<digest> in the fill container kiln-fill-<repo>, with the
// network and the repo's limits. Every run of the repo then mounts the volume
// read-only. The engine cannot rename a volume, so a volume's name is there
// before its content: a volume counts as filled only while kiln-fill-<repo>
// carries its digest and has exited 0, and that container is kept as the
// proof until the next fill.

// RunCache is a repo's filled cache that a run mounts read-only: the volume
// kiln-cache-<repo>-<digest> at Path.
type RunCache struct {
	Volume string
	Path   string
}

// Fill is what FillCache fills: the volume of the repo's cache of a digest,
// from the image kiln-cache-<repo>, mounted at Path, with the repo's memory
// and CPUs, as CheckLimits takes them.
type Fill struct {
	Repo   string
	Digest string
	Path   string
	Memory string
	CPUs   int
}

// The labels of a cache volume, which carries the repo, and of a fill
// container, which carries the repo too. Both also carry kiln.digest.
const (
	cacheLabel = "kiln.cache"
	fillLabel  = "kiln.fill"
)

// cacheRepo is the name of a repo with a toolchain (settings), and
// cacheDigest a cache's digest.
var (
	cacheRepo     = regexp.MustCompile(`^[a-z0-9-]{0,39}[a-z0-9]$`)
	cacheDigest   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	cacheVolume   = regexp.MustCompile(`^kiln-cache-([a-z0-9-]*[a-z0-9])-([0-9a-f]{64})$`)
	pathComponent = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// CacheImage is the image a repo's cache is filled from.
func CacheImage(repo string) string { return "kiln-cache-" + repo }

// CacheVolume is the volume of a repo's cache of a digest.
func CacheVolume(repo, digest string) string { return "kiln-cache-" + repo + "-" + digest }

// fillName is the name of a repo's fill container.
func fillName(repo string) string { return "kiln-fill-" + repo }

// reservedPaths are the places of a run that a cache may neither be, be
// inside, nor hold.
var reservedPaths = []string{"/work", "/kiln", "/home", "/tmp", "/proc", "/sys", "/dev", "/etc", "/run",
	"/usr", "/bin", "/sbin", "/lib", "/lib64"}

// CheckCachePath refuses, with the reason, a path in a run that a cache may
// not be mounted at: one that is not absolute or not in clean form, has a
// component that is not ASCII letters, digits, ., _ and -, or is . or .., is
// /, or is, is inside or holds one of reservedPaths. It serves the settings
// file and CreateRun.
func CheckCachePath(p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p {
		return fmt.Errorf("the cache's path %q is not an absolute path in clean form", p)
	}
	if p == "/" {
		return fmt.Errorf("the cache's path is /")
	}
	for _, c := range strings.Split(p[1:], "/") {
		if !pathComponent.MatchString(c) || c == "." || c == ".." {
			return fmt.Errorf("the cache's path %q has the component %q, which is not ASCII letters, digits, ., _ and -", p, c)
		}
	}
	for _, r := range reservedPaths {
		if p == r || strings.HasPrefix(p, r+"/") || strings.HasPrefix(r, p+"/") {
			return fmt.Errorf("the cache's path %q is, is inside or holds %s, which is the run's", p, r)
		}
	}
	return nil
}

// checkRunCache refuses a cache of a run whose volume is not named
// kiln-cache-<repo>-<64 hex digits>, or whose path CheckCachePath refuses.
// It returns the volume's repo.
func checkRunCache(c RunCache) (string, error) {
	m := cacheVolume.FindStringSubmatch(c.Volume)
	if m == nil || !cacheRepo.MatchString(m[1]) {
		return "", fmt.Errorf("the cache volume %q is not named kiln-cache-<repo>-<digest>", c.Volume)
	}
	return m[1], CheckCachePath(c.Path)
}

// cacheMount is the option of a run's create that mounts its cache, if it has
// one: read-only, and with no copy of the image's content into the volume.
func cacheMount(c *RunCache) []string {
	if c == nil {
		return nil
	}
	return []string{"--mount", "type=volume,source=" + c.Volume + ",target=" + c.Path + ",readonly,volume-nocopy"}
}

// cacheThere refuses a run's cache volume that is not there, or lacks the
// label kiln.cache=<repo>: a --mount of a missing named volume would make an
// empty one.
func cacheThere(ctx context.Context, volume, repo string) error {
	v, err := findVolume(ctx, volume, cacheLabel)
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("there is no cache volume %s", volume)
	}
	if v.label != repo {
		return fmt.Errorf("the volume %s has no label %s=%s, so Kiln did not make it; it is left alone", volume, cacheLabel, repo)
	}
	return nil
}

// foundVolume is what the seal reads of a volume it looks for by name.
type foundVolume struct {
	label string // the value of the label asked for, empty without it
}

// findVolume looks for the volume of a name and reads one of its labels. It
// returns nil when there is none.
func findVolume(ctx context.Context, name, label string) (*foundVolume, error) {
	// The filter matches parts of names, so the name is compared below.
	out, err := docker(ctx, nil, "volume", "ls", "--filter", "name="+name,
		"--format", "{{.Name}}\t{{.Label \""+label+"\"}}")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 2 && f[0] == name {
			return &foundVolume{label: f[1]}, nil
		}
	}
	return nil, nil
}

// checkFill refuses a fill whose repo, digest, path or limits are not ones
// the seal would fill, and returns its memory and CPUs, the defaults for ""
// and 0.
func checkFill(f Fill) (string, int, error) {
	if !cacheRepo.MatchString(f.Repo) {
		return "", 0, fmt.Errorf("the repo %q cannot name a cache", f.Repo)
	}
	if !cacheDigest.MatchString(f.Digest) {
		return "", 0, fmt.Errorf("the cache's digest %q is not 64 hex digits", f.Digest)
	}
	if err := CheckCachePath(f.Path); err != nil {
		return "", 0, err
	}
	if err := CheckLimits(f.Memory, f.CPUs); err != nil {
		return "", 0, err
	}
	memory, cpus := f.Memory, f.CPUs
	if memory == "" {
		memory = defaultMemory
	}
	if cpus == 0 {
		cpus = defaultCPUs
	}
	return memory, cpus, nil
}

// fillArgs are the arguments of the fill container's create: on the engine's
// default network, with the repo's memory, without swap, and CPUs, 1024
// processes and no cpuset, nothing gained by setuid programs, and the volume
// mounted read-write at the cache's path, with the engine's copy of the
// image's content into it; then the image, with its own user and command.
func fillArgs(f Fill, memory string, cpus int) []string {
	volume := CacheVolume(f.Repo, f.Digest)
	return []string{
		"--pull", "never", // a missing image is an error, never a download
		"--name", fillName(f.Repo),
		"--label", fillLabel + "=" + f.Repo,
		"--label", DigestLabel + "=" + f.Digest,
		"--network", "bridge",
		"--memory", memory,
		"--memory-swap", memory,
		"--cpus", strconv.Itoa(cpus),
		"--pids-limit", "1024",
		"--security-opt", "no-new-privileges",
		"--log-driver", "json-file",
		"--mount", "type=volume,source=" + volume + ",target=" + f.Path,
		"--", CacheImage(f.Repo),
	}
}

// FillCache fills the volume kiln-cache-<repo>-<digest> from the image
// kiln-cache-<repo> in the container kiln-fill-<repo>, which it keeps as the
// proof that the volume is whole (CacheFilled). Each step looks at what is
// there first, so that it can be repeated after a failure or a kill at any
// step (ADR 0035):
//
//  1. A container kiln-fill-<repo> with the label kiln.fill=<repo> is removed
//     by its ID, with force. One without the label is refused.
//  2. A volume of the name with the label kiln.cache=<repo> is removed, by its
//     name and without force. One without the label is refused. Both are
//     looked at before either is removed, so that a refusal touches nothing.
//  3. The volume is created with the labels kiln.cache and kiln.digest.
//  4. The container is created (fillArgs).
//  5. It is started and waited for. An exit status other than 0 is an error
//     with the status and the last 4 KiB of its output; the container and the
//     volume stay until the next fill, so that they can be read.
//
// A fill whose context ends while the container runs leaves it running.
func FillCache(ctx context.Context, f Fill) error {
	memory, cpus, err := checkFill(f)
	if err != nil {
		return err
	}
	name, volume := fillName(f.Repo), CacheVolume(f.Repo, f.Digest)
	c, err := findContainer(ctx, name, fillLabel)
	if err != nil {
		return err
	}
	if c != nil && c.label != f.Repo {
		return fmt.Errorf("the container %s has no label %s=%s, so Kiln did not make it; it is left alone", name, fillLabel, f.Repo)
	}
	v, err := findVolume(ctx, volume, cacheLabel)
	if err != nil {
		return err
	}
	if v != nil && v.label != f.Repo {
		return fmt.Errorf("the volume %s has no label %s=%s, so Kiln did not make it; it is left alone", volume, cacheLabel, f.Repo)
	}
	if c != nil {
		if _, err := docker(ctx, nil, "rm", "--force", c.id); err != nil {
			return err
		}
	}
	if v != nil {
		if _, err := docker(ctx, nil, "volume", "rm", volume); err != nil {
			return err
		}
	}
	if _, err := docker(ctx, nil, "volume", "create", "--label", cacheLabel+"="+f.Repo,
		"--label", DigestLabel+"="+f.Digest, volume); err != nil {
		return err
	}
	id, err := createContainer(ctx, nil, "create", fillArgs(f, memory, cpus)...)
	if err != nil {
		return err
	}
	if _, err := docker(ctx, nil, "start", id); err != nil {
		return err
	}
	out, err := docker(ctx, nil, "wait", id)
	if err != nil {
		return err
	}
	if status := strings.TrimSpace(out); status != "0" {
		logs := &lastBytes{max: maxBuildErr}
		cmd := commandIn(ctx, "docker", "logs", id)
		cmd.Stdout, cmd.Stderr = logs, logs
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("the fill %s exited with status %s, and its output could not be read: %v", name, status, err)
		}
		return fmt.Errorf("the fill %s exited with status %s: %s", name, status, strings.TrimSpace(string(logs.data)))
	}
	return nil
}

// CacheFilled reports whether the volume kiln-cache-<repo>-<digest> is whole:
// it is there with the label kiln.cache=<repo>, and the container
// kiln-fill-<repo> carries kiln.fill=<repo> and kiln.digest=<digest>, has
// exited with status 0 and mounts that volume.
func CacheFilled(ctx context.Context, repo, digest string) (bool, error) {
	if !cacheRepo.MatchString(repo) || !cacheDigest.MatchString(digest) {
		return false, fmt.Errorf("the repo %q and digest %q name no cache", repo, digest)
	}
	volume := CacheVolume(repo, digest)
	v, err := findVolume(ctx, volume, cacheLabel)
	if err != nil || v == nil || v.label != repo {
		return false, err
	}
	c, err := findContainer(ctx, fillName(repo), fillLabel)
	if err != nil || c == nil || c.label != repo || c.state != "exited" {
		return false, err
	}
	out, err := docker(ctx, nil, "inspect", "--type", "container", "--format",
		`{{index .Config.Labels "`+DigestLabel+`"}}`+"\t{{.State.ExitCode}}\t{{json .Mounts}}", c.id)
	if err != nil {
		return false, err
	}
	f := strings.SplitN(out, "\t", 3)
	if len(f) != 3 || f[0] != digest || f[1] != "0" {
		return false, nil
	}
	var mounts []struct{ Type, Name string }
	if err := json.Unmarshal([]byte(f[2]), &mounts); err != nil {
		return false, fmt.Errorf("the mounts of %s: %w", fillName(repo), err)
	}
	return slices.ContainsFunc(mounts, func(m struct{ Type, Name string }) bool {
		return m.Type == "volume" && m.Name == volume
	}), nil
}

// CacheRemoval is the outcome of the removal of one old cache volume: Err is
// nil when it was removed.
type CacheRemoval struct {
	Volume string
	Err    error
}

// oldCaches are the names of a listing of `docker volume ls --format
// '{{.Name}}\t{{.Label "kiln.cache"}}'` that are old cache volumes of the
// repo: the label is exactly repo, and the name is
// kiln-cache-<repo>-<digest> with that repo and a digest other than digest.
func oldCaches(listing, repo, digest string) []string {
	var old []string
	for _, line := range strings.Split(listing, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 2 || f[1] != repo {
			continue
		}
		m := cacheVolume.FindStringSubmatch(f[0])
		if m == nil || m[1] != repo || m[2] == digest {
			continue
		}
		old = append(old, f[0])
	}
	return old
}

// RemoveOldCaches removes the repo's cache volumes other than the one of
// digest, by name, and returns the outcome of each removal. Its rules:
//
//  1. repo and digest are checked before any docker command.
//  2. Only volumes that `docker volume ls --filter label=kiln.cache=<repo>`
//     lists, with exactly that label and named kiln-cache-<repo>-<digest>
//     with that repo, are candidates; the volume of digest never is.
//  3. A volume that `docker ps --all --filter volume=<name>` names a
//     container for is skipped, and does not appear in the result: a run in
//     flight, a stopped container and kiln-fill-<repo> keep theirs. A failure
//     of ps is the volume's error, and it is not removed.
//  4. Each other volume is removed with `docker volume rm`, without force,
//     so the engine refuses one that a container took since.
//  5. A failure of the listing is returned, and nothing is removed.
//
// It never touches a container or an image.
func RemoveOldCaches(ctx context.Context, repo, digest string) ([]CacheRemoval, error) {
	if !cacheRepo.MatchString(repo) || !cacheDigest.MatchString(digest) {
		return nil, fmt.Errorf("the repo %q and digest %q name no cache", repo, digest)
	}
	out, err := docker(ctx, nil, "volume", "ls", "--filter", "label="+cacheLabel+"="+repo,
		"--format", "{{.Name}}\t{{.Label \""+cacheLabel+"\"}}")
	if err != nil {
		return nil, err
	}
	var done []CacheRemoval
	for _, name := range oldCaches(out, repo, digest) {
		users, err := docker(ctx, nil, "ps", "--all", "--quiet", "--filter", "volume="+name)
		if err != nil {
			done = append(done, CacheRemoval{Volume: name, Err: err})
			continue
		}
		if strings.TrimSpace(users) != "" {
			continue
		}
		_, err = docker(ctx, nil, "volume", "rm", name)
		done = append(done, CacheRemoval{Volume: name, Err: err})
	}
	return done, nil
}
