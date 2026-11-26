package seal

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestCheckLimits(t *testing.T) {
	for _, tc := range []struct {
		memory string
		cpus   int
		want   string // a part of the reason, "" for limits that are taken
	}{
		{"", 0, ""},
		{"6m", 1, ""},
		{"4g", 2, ""},
		{"16384m", 64, ""},
		{"8589934591g", 0, ""}, // the most GiB whose bytes fit in 64 bits
		{"8589934592g", 0, "more bytes"},
		{"5m", 0, "below 6m"},
		{"0m", 0, "not <n>m"},
		{"04g", 0, "not <n>m"},
		{"4G", 0, "not <n>m"},
		{"4k", 0, "not <n>m"},
		{"4", 0, "not <n>m"},
		{"４g", 0, "not <n>m"},
		{"", -1, "the CPUs are -1"},
	} {
		err := CheckLimits(tc.memory, tc.cpus)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%q, %d: refused: %v", tc.memory, tc.cpus, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%q, %d: %v, want a refusal with %q", tc.memory, tc.cpus, err, tc.want)
		}
	}
}

func TestCPUsError(t *testing.T) {
	err := error(cpusError{want: 6, have: 4})
	if !errors.Is(err, ErrCPUs) || err.Error() != "the run asks for 6 CPUs and the engine has 4" {
		t.Errorf("%q, errors.Is ErrCPUs %v", err, errors.Is(err, ErrCPUs))
	}
}

func TestChooseCPUs(t *testing.T) {
	// runs chooses for runs of the sizes one after the other, each counting
	// the ones before.
	runs := func(n int, used []string, sizes ...int) []string {
		var got []string
		for _, size := range sizes {
			set := formatCPUs(chooseCPUs(n, size, used))
			used = append(used, set)
			got = append(got, set)
		}
		return got
	}
	for _, tc := range []struct {
		name  string
		n     int
		used  []string
		sizes []int
		want  []string
	}{
		{"2, 2 and 2 of 4", 4, nil, []int{2, 2, 2}, []string{"0,1", "2,3", "0,1"}},
		{"1, 2 and 1 of 4", 4, nil, []int{1, 2, 1}, []string{"0", "1,2", "3"}},
		{"the least used first", 4, []string{"0,1", "0,1", "2"}, []int{2}, []string{"2,3"}},
		{"labels without a cpuset count for nothing", 4, []string{"", "", ""}, []int{2}, []string{"0,1"}},
		{"labels that do not parse are ignored", 4, []string{"x", "0,", "1-", "-1", "3-2", "+1", " 0", "0 ", "0x1"}, []int{1}, []string{"0"}},
		{"CPUs of n and above are ignored", 2, []string{"2,3", "5-9"}, []int{1}, []string{"0"}},
		{"ranges", 6, []string{"0-3,5"}, []int{1}, []string{"4"}},
		{"a CPU twice in a label counts once", 2, []string{"0,0,0-0", "1"}, []int{1}, []string{"0"}},
		{"a range far above n", 2, []string{"0-99999999999"}, []int{1}, []string{"0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runs(tc.n, tc.used, tc.sizes...); !slices.Equal(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if cpus, ok := parseCPUs("0-3,5", 8); !ok || !slices.Equal(cpus, []int{0, 1, 2, 3, 5}) {
		t.Errorf(`"0-3,5" reads as %v, %v`, cpus, ok)
	}
}

// TestChooseCPUsProperties checks, for random runs, that runs whose CPUs add
// up to no more than the engine's are disjoint, and that after runs of one
// size the use of the CPUs differs by at most 1.
func TestChooseCPUsProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for n := 1; n <= 16; n++ {
		for try := 0; try < 50; try++ {
			var used []string
			taken := make([]bool, n)
			left := n
			for left > 0 {
				size := 1 + r.IntN(left)
				cpus := chooseCPUs(n, size, used)
				if len(cpus) != size {
					t.Fatalf("n=%d: a run of %d got %v", n, size, cpus)
				}
				for _, c := range cpus {
					if taken[c] {
						t.Fatalf("n=%d: %v after %q shares CPU %d", n, cpus, used, c)
					}
					taken[c] = true
				}
				used = append(used, formatCPUs(cpus))
				left -= size
			}

			size := 1 + r.IntN(n)
			count := make([]int, n)
			used = nil
			for k := 0; k < 1+r.IntN(3*n); k++ {
				cpus := chooseCPUs(n, size, used)
				for _, c := range cpus {
					count[c]++
				}
				used = append(used, formatCPUs(cpus))
			}
			if slices.Max(count)-slices.Min(count) > 1 {
				t.Fatalf("n=%d: runs of %d give the use %v", n, size, count)
			}
		}
	}
}

// hostLimits is what docker inspect gives of a container's limits: the
// memory, the memory with swap, the CPUs in billionths, the cpuset, and the
// label kiln.cpuset ("" without it).
type hostLimits struct {
	memory, swap, nanoCPUs, cpuset, label string
}

func inspectLimits(t *testing.T, container string) hostLimits {
	t.Helper()
	got := strings.Split(must(t, "inspect", "--format",
		`{{.HostConfig.Memory}}|{{.HostConfig.MemorySwap}}|{{.HostConfig.NanoCpus}}|{{.HostConfig.CpusetCpus}}|{{index .Config.Labels "`+cpusetLabel+`"}}`,
		container), "|")
	if len(got) != 5 {
		t.Fatalf("docker inspect of %s gives %q", container, got)
	}
	if got[4] == "<no value>" {
		got[4] = ""
	}
	return hostLimits{got[0], got[1], got[2], got[3], got[4]}
}

// assertPinned asserts a run container's limits: the memory in bytes, with
// swap the same, the CPUs, and a cpuset of as many CPUs below the engine's
// count, equal to its label kiln.cpuset. It returns the cpuset.
func assertPinned(t *testing.T, container, memory string, cpus int) string {
	t.Helper()
	n, err := engineCPUs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := inspectLimits(t, container)
	if got.memory != memory || got.swap != memory || got.nanoCPUs != strconv.Itoa(cpus)+"000000000" {
		t.Errorf("%s has the memory %s, with swap %s, and %s billionths of CPUs; want %s, %s and %d CPUs",
			container, got.memory, got.swap, got.nanoCPUs, memory, memory, cpus)
	}
	if set, ok := parseCPUs(got.cpuset, n); !ok || len(set) != cpus || formatCPUs(set) != got.cpuset {
		t.Errorf("%s has the cpuset %q, want %d CPUs below %d as a comma list", container, got.cpuset, cpus, n)
	}
	if got.label != got.cpuset {
		t.Errorf("%s has the label %s=%q, want its cpuset %q", container, cpusetLabel, got.label, got.cpuset)
	}
	return got.cpuset
}

// testLimits creates runs with limits of their own, and asserts what reaches
// their containers and their saves, the refusal of more CPUs than the
// engine's, and the cpusets of runs created side by side.
func testLimits(t *testing.T, f fixture) {
	n, err := engineCPUs(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a run's own memory and CPUs", func(t *testing.T) {
		id := f.runID("nproc")
		createRun(t, Run{ID: id, Image: f.runImage, Proxy: f.proxy.Name, Memory: "64m", CPUs: 1, Command: []string{"nproc"}})
		assertPinned(t, runName(id), "67108864", 1)
		if out, _ := startAndWait(t, id); strings.TrimSpace(out) != "1" {
			t.Errorf("nproc in a run of 1 CPU printed %q", out)
		}

		// The save of that run keeps the fixed limits.
		save, err := createSave(t.Context(), id, f.runImage, []string{"true"})
		if err != nil {
			t.Fatal(err)
		}
		if got := inspectLimits(t, save); got != (hostLimits{"4294967296", "4294967296", "2000000000", "", ""}) {
			t.Errorf("the save of a run of 64m and 1 CPU has %+v, want 4 GiB, 2 CPUs and no cpuset", got)
		}
	})

	t.Run("more CPUs than the engine has", func(t *testing.T) {
		id := f.runID("toomany")
		cleanupRun(t, Run{ID: id, Proxy: f.proxy.Name})
		err := CreateRun(t.Context(), Run{ID: id, Image: f.upstreamImage, Proxy: f.proxy.Name, CPUs: n + 1})
		want := fmt.Sprintf("the run asks for %d CPUs and the engine has %d", n+1, n)
		if !errors.Is(err, ErrCPUs) || err.Error() != want {
			t.Errorf("CreateRun of %d CPUs: %v, want ErrCPUs and %q", n+1, err, want)
		}
		if got := containerID(t, runName(id)); got != "" {
			t.Errorf("the refused run has the container %s", got)
		}
		if got := networkID(t, runName(id)); got != "" {
			t.Errorf("the refused run has the network %s", got)
		}
	})

	t.Run("side by side", func(t *testing.T) {
		if n < 2 {
			t.Skipf("the engine has %d CPU", n)
		}
		var seen []string
		for try := range 3 {
			before, err := usedCPUs(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			first := formatCPUs(chooseCPUs(n, 1, before))
			second := formatCPUs(chooseCPUs(n, 1, append(slices.Clone(before), first)))
			ids := []string{f.runID(fmt.Sprintf("side%d-a", try)), f.runID(fmt.Sprintf("side%d-b", try))}
			errs := make(chan error, len(ids))
			for _, id := range ids {
				cleanupRun(t, Run{ID: id, Proxy: f.proxy.Name})
				go func() {
					errs <- CreateRun(t.Context(), Run{ID: id, Image: f.upstreamImage, Proxy: f.proxy.Name, CPUs: 1})
				}()
			}
			for range ids {
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
			}
			after, err := usedCPUs(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			got := []string{assertPinned(t, runName(ids[0]), "4294967296", 1), assertPinned(t, runName(ids[1]), "4294967296", 1)}
			for _, id := range ids {
				must(t, "rm", "--force", runName(id))
			}
			// Only another process that creates runs on the engine changes
			// what is used beyond the two; then it is taken again.
			wantAfter := append(slices.Clone(before), got...)
			slices.Sort(wantAfter)
			slices.Sort(after)
			if !slices.Equal(after, wantAfter) {
				seen = append(seen, fmt.Sprintf("try %d: used %q before, %q after, the runs got %q", try+1, before, after, got))
				continue
			}
			slices.Sort(got)
			want := []string{first, second}
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("runs side by side got %q, want %q, the next two choices after %q", got, want, before)
			}
			if first != second && got[0] == got[1] {
				t.Errorf("runs side by side share the CPU %s", got[0])
			}
			return
		}
		t.Fatalf("what is used changed under the test three times:\n%s", strings.Join(seen, "\n"))
	})
}
