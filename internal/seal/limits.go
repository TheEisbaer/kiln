package seal

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// The limits of a run whose caller gives none: 4 GiB without swap and 2 CPUs
// (ADR 0017).
const (
	defaultMemory = "4g"
	defaultCPUs   = 2
)

// fixedLimits are the limits of a save container and of the core's build,
// whatever the run's: 4 GiB without swap, 2 CPUs, 1024 processes, and no
// cpuset (ADR 0017, ADR 0038).
var fixedLimits = []string{
	"--memory", defaultMemory,
	"--memory-swap", defaultMemory,
	"--cpus", strconv.Itoa(defaultCPUs),
	"--pids-limit", "1024",
}

// runLimits are the limits of a run's container: its memory without swap, its
// CPUs as the quota and as the cpuset, and 1024 processes. Its memory and CPUs
// are the caller's, checked by the seal (CheckLimits), or the defaults.
func runLimits(memory string, cpus int, cpuset string) []string {
	return []string{
		"--memory", memory,
		"--memory-swap", memory,
		"--cpus", strconv.Itoa(cpus),
		"--cpuset-cpus", cpuset,
		"--pids-limit", "1024",
	}
}

// cpusetLabel carries the CPUs of a run's container, the same list as its
// --cpuset-cpus, by which later runs count which CPUs are in use.
const cpusetLabel = "kiln.cpuset"

// minMemory is the least memory the engine accepts for a container, in bytes
// (moby's linuxMinMemory).
const minMemory = 6 << 20

var memoryForm = regexp.MustCompile(`^([1-9][0-9]*)([mg])$`)

// CheckLimits refuses, with the reason, a run's memory and CPUs that the seal
// would not give a run. The memory is "" for the default, or <n>m or <n>g with
// n a whole number from 1 in ASCII digits without a leading zero, MiB or GiB,
// at least 6m and no more bytes than a signed 64-bit number holds. The CPUs
// are 0 for the default, or a whole number from 1.
func CheckLimits(memory string, cpus int) error {
	if cpus < 0 {
		return fmt.Errorf("the CPUs are %d; they are a whole number from 1", cpus)
	}
	if memory == "" {
		return nil
	}
	m := memoryForm.FindStringSubmatch(memory)
	if m == nil {
		return fmt.Errorf("the memory %q is not <n>m or <n>g, with n a whole number from 1 without a leading zero", memory)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	shift := 20
	if m[2] == "g" {
		shift = 30
	}
	if err != nil || n > math.MaxInt64>>shift {
		return fmt.Errorf("the memory %q is more bytes than the engine can be given", memory)
	}
	if n<<shift < minMemory {
		return fmt.Errorf("the memory %q is below 6m, the least the engine accepts", memory)
	}
	return nil
}

// ErrCPUs is what an error wraps when a run asks for more CPUs than the
// engine has.
var ErrCPUs = errors.New("the run asks for more CPUs than the engine has")

// cpusError names the CPUs a run asks for and the engine's.
type cpusError struct{ want, have int }

func (e cpusError) Error() string {
	return fmt.Sprintf("the run asks for %d CPUs and the engine has %d", e.want, e.have)
}

func (e cpusError) Is(target error) bool { return target == ErrCPUs }

// engineCPUs is the engine's count of CPUs, its NCPU, against which the
// engine checks --cpus.
func engineCPUs(ctx context.Context) (int, error) {
	out, err := docker(ctx, nil, "info", "--format", "{{.NCPU}}")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("the engine gives %q as its count of CPUs", out)
	}
	return n, nil
}

// cpusetMu is held from the listing of the CPUs in use to the create of the
// run's container, so that the runs this process creates side by side choose
// from what the others took. Runs of other processes are counted, but not
// waited for.
var cpusetMu sync.Mutex

// usedCPUs lists the label kiln.cpuset of every container labelled kiln.run
// that is created or running, of every instance and of none: one line each,
// empty for a container without it, such as a way out or a save.
func usedCPUs(ctx context.Context) ([]string, error) {
	out, err := docker(ctx, nil, "ps", "--all", "--no-trunc", "--filter", "label="+runLabel,
		"--filter", "status=created", "--filter", "status=running",
		"--format", `{{.Label "`+cpusetLabel+`"}}`)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// chooseCPUs chooses want of the CPUs 0 to n-1, the least used first and a
// tie to the lower number, and returns them in order. A CPU's use is the
// number of lists in used that name it; a list that does not parse is
// ignored, and so is a CPU of n or above.
func chooseCPUs(n, want int, used []string) []int {
	count := make([]int, n)
	for _, l := range used {
		cpus, ok := parseCPUs(l, n)
		if !ok {
			continue
		}
		for _, c := range cpus {
			count[c]++
		}
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return count[a] - count[b] })
	chosen := order[:min(want, n)]
	slices.Sort(chosen)
	return chosen
}

// parseCPUs reads a list of CPUs as --cpuset-cpus takes it, numbers and
// ranges a-b separated by commas, and returns the CPUs below n it names, each
// once. ok is false for a list that does not parse, also an empty one.
func parseCPUs(list string, n int) (cpus []int, ok bool) {
	if list == "" {
		return nil, false
	}
	seen := make([]bool, n)
	for _, part := range strings.Split(list, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, okA := cpuNumber(lo)
		b := a
		okB := true
		if isRange {
			b, okB = cpuNumber(hi)
		}
		if !okA || !okB || b < a {
			return nil, false
		}
		for c := a; c <= b && c < n; c++ {
			if !seen[c] {
				seen[c] = true
				cpus = append(cpus, c)
			}
		}
	}
	return cpus, true
}

// cpuNumber reads one CPU's number, ASCII digits only.
func cpuNumber(s string) (int, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// formatCPUs writes CPUs as --cpuset-cpus takes them, a comma list.
func formatCPUs(cpus []int) string {
	s := make([]string, len(cpus))
	for i, c := range cpus {
		s[i] = strconv.Itoa(c)
	}
	return strings.Join(s, ",")
}
