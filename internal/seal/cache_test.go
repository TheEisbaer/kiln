package seal

import (
	"slices"
	"strings"
	"testing"
)

// TestCheckCachePath takes the paths a cache may be mounted at, and refuses
// the rest.
func TestCheckCachePath(t *testing.T) {
	for _, p := range []string{"/opt/cache", "/var/cache/nuget", "/opt", "/cache", "/opt/a_b-c.d", "/workspace", "/kilns"} {
		if err := CheckCachePath(p); err != nil {
			t.Errorf("CheckCachePath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{"", "opt/cache", "./opt", "/", "/opt/", "/opt//cache", "/opt/./cache", "/opt/../cache",
		"/opt/ca che", "/opt/ca,che", "/opt/cache=x", "/opt/caché",
		"/work", "/work/cache", "/kiln", "/kiln/out", "/home", "/home/kiln/.npm", "/tmp", "/tmp/cache",
		"/proc", "/sys", "/dev", "/etc", "/etc/nuget", "/run", "/usr", "/usr/local/share", "/bin", "/sbin",
		"/lib", "/lib64"} {
		if err := CheckCachePath(p); err == nil {
			t.Errorf("CheckCachePath(%q) = nil, want a refusal", p)
		}
	}
}

// TestCacheMount gives a run with a cache exactly one --mount, read-only and
// without the engine's copy, and a run without one none; a volume or path
// that the seal would not mount is refused before docker is run.
func TestCacheMount(t *testing.T) {
	if got := cacheMount(nil); got != nil {
		t.Errorf("a run without a cache has the options %q", got)
	}
	volume := CacheVolume("work", strings.Repeat("ab", 32))
	got := cacheMount(&RunCache{Volume: volume, Path: "/opt/cache"})
	want := []string{"--mount", "type=volume,source=" + volume + ",target=/opt/cache,readonly,volume-nocopy"}
	if !slices.Equal(got, want) {
		t.Errorf("the cache's options are %q, want %q", got, want)
	}
	if repo, err := checkRunCache(RunCache{Volume: volume, Path: "/opt/cache"}); err != nil || repo != "work" {
		t.Errorf("checkRunCache of %s = %q, %v", volume, repo, err)
	}

	// Without docker on the path, a refusal that came after a docker command
	// would say that docker is missing instead.
	t.Setenv("PATH", t.TempDir())
	for _, c := range []struct {
		cache RunCache
		want  string
	}{
		{RunCache{Volume: "kiln-cache-work", Path: "/opt/cache"}, "is not named kiln-cache-<repo>-<digest>"},
		{RunCache{Volume: "kiln-cache-work-" + strings.Repeat("ab", 31), Path: "/opt/cache"}, "is not named"},
		{RunCache{Volume: "kiln-cache-Work-" + strings.Repeat("ab", 32), Path: "/opt/cache"}, "is not named"},
		{RunCache{Volume: "other-" + strings.Repeat("ab", 32), Path: "/opt/cache"}, "is not named"},
		{RunCache{Volume: volume + ",readonly=false", Path: "/opt/cache"}, "is not named"},
		{RunCache{Volume: volume, Path: "/kiln"}, "which is the run's"},
		{RunCache{Volume: volume, Path: "/opt/cache,readonly=false"}, "component"},
	} {
		err := CreateRun(t.Context(), Run{ID: "cachetest", Image: "kiln-run", Proxy: "kiln-proxy", Cache: &c.cache})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("CreateRun with the cache %+v: %v, want an error with %q", c.cache, err, c.want)
		}
	}
}

// TestFillArgs gives the fill container the repo's limits, the default
// network and its volume mounted read-write with the engine's copy.
func TestFillArgs(t *testing.T) {
	digest := strings.Repeat("0f", 32)
	f := Fill{Repo: "work", Digest: digest, Path: "/opt/cache", Memory: "6g", CPUs: 3}
	memory, cpus, err := checkFill(f)
	if err != nil || memory != "6g" || cpus != 3 {
		t.Fatalf("checkFill = %q, %d, %v", memory, cpus, err)
	}
	args := strings.Join(fillArgs(f, memory, cpus), " ")
	want := "--pull never --name kiln-fill-work --label kiln.fill=work --label kiln.digest=" + digest +
		" --network bridge --memory 6g --memory-swap 6g --cpus 3 --pids-limit 1024 --security-opt no-new-privileges" +
		" --log-driver json-file --mount type=volume,source=kiln-cache-work-" + digest + ",target=/opt/cache -- kiln-cache-work"
	if args != want {
		t.Errorf("the fill's arguments are\n%s\nwant\n%s", args, want)
	}
	if memory, cpus, err := checkFill(Fill{Repo: "work", Digest: digest, Path: "/opt/cache"}); err != nil || memory != "4g" || cpus != 2 {
		t.Errorf("checkFill without limits = %q, %d, %v; want 4g and 2", memory, cpus, err)
	}
	for _, bad := range []Fill{
		{Repo: "Work", Digest: digest, Path: "/opt/cache"},
		{Repo: "work", Digest: digest[:63], Path: "/opt/cache"},
		{Repo: "work", Digest: digest, Path: "/work/cache"},
		{Repo: "work", Digest: digest, Path: "/opt/cache", Memory: "1k"},
	} {
		if _, _, err := checkFill(bad); err == nil {
			t.Errorf("checkFill(%+v) = nil, want a refusal", bad)
		}
	}
}
