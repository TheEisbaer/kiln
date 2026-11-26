package seal

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Run is what the caller says about a run's container. Its network, mounts,
// environment and user are the seal's and cannot be given. Its memory and
// CPUs are the caller's, checked by the seal; its other limits and its cpuset
// are the seal's.
type Run struct {
	ID      string            // names the run's network and container, kiln-run-<ID>
	Image   string            // the run image (ADR 0039)
	Proxy   string            // the proxy container's name
	Command []string          // the container's arguments, after the image
	Labels  map[string]string // extra labels; kiln.run, kiln.instance, kiln.hosts and kiln.cpuset cannot be among them
	// The instance the run belongs to, carried by the label kiln.instance on
	// its network and container, by which ListRuns finds it. A run started by
	// hand has none, so no core lists it.
	Instance string
	// The list of hosts the run may reach (ADR 0059): `*` alone, or names.
	// Empty, the run is sealed as before and gets no way out. With a list,
	// the run gets a way out of its own from WayOutImage, which is also on
	// the outward network WayOutNetwork.
	Hosts         []string
	WayOutImage   string
	WayOutNetwork string
	// Memory is the run's memory, <n>m or <n>g, without swap, and CPUs its
	// CPUs, a whole number; "" and 0 are 4g and 2 (CheckLimits).
	Memory string
	CPUs   int
}

// runLabel carries a run's ID on its container (ADR 0035, point 2), and marks
// the container as Kiln's.
const runLabel = "kiln.run"

// instanceLabel carries the instance a run belongs to.
const instanceLabel = "kiln.instance"

// instanceID is what an instance's identity may be.
var instanceID = regexp.MustCompile(`^[a-z0-9]{1,40}$`)

// checkInstance refuses an identity that cannot name an instance.
func checkInstance(instance string) error {
	if !instanceID.MatchString(instance) {
		return fmt.Errorf("the instance %q does not match %s", instance, instanceID)
	}
	return nil
}

var runID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// checkRunID refuses an ID that cannot name a run.
func checkRunID(id string) error {
	if !runID.MatchString(id) {
		return fmt.Errorf("the run ID %q does not match %s", id, runID)
	}
	return nil
}

// CheckRunID refuses an ID that cannot name a run, so that a caller can judge
// an ID before it uses it for anything else.
func CheckRunID(id string) error { return checkRunID(id) }

// runName is the name of a run's network and of its container.
func runName(id string) string { return "kiln-run-" + id }

// CreateRun creates a run's network, with the proxy attached, and then its
// container. With an instance, both carry the label kiln.instance=<instance>.
// The container is created, not started: the caller places files in it and
// starts it.
//
// Its memory and CPUs are the caller's, checked by the seal (CheckLimits), or
// 4g and 2; its memory is also its memory with swap. A run that asks for more
// CPUs than the engine's count, docker info's NCPU, is refused with an error
// that wraps ErrCPUs, before anything is created or removed. The container
// gets --cpuset-cpus with exactly its CPUs, so that nproc inside it shows
// them, and the label kiln.cpuset with the same list. They are chosen from 0
// to NCPU-1 by chooseCPUs, the least used by the created and running
// containers labelled kiln.run first, under a lock of the process held from
// the listing until the container's create has returned, so that runs created
// side by side get disjoint sets while the CPUs last.
//
// With a list of hosts, the list is checked first, and the outward network is
// created if it is missing; one of its name without the label kiln.web is
// refused. After the run's network the way out kiln-wayout-<ID> is created on
// it, connected to the outward network and started (createWayOut). The run's
// container then gets HTTPS_PROXY and https_proxy, which name the way out,
// NO_PROXY and no_proxy, which name the proxy so that the model traffic does
// not take the web's path, and the label kiln.hosts with the list. Without a
// list none of this is done.
//
// Before it creates anything it looks at what is there, so that it can be
// repeated after a failure or a kill at any step (ADR 0035):
//
//   - A container of the run's name with the label kiln.run=<ID> that has never
//     started is removed, and its network with it.
//   - A container of the name that has started, or that lacks the label, is
//     refused, and nothing is touched.
//   - A way out of the run with the label kiln.run=<ID> is removed with such
//     a container, or alone. One without the label is refused, and nothing is
//     touched.
//   - A network of the name without a container is removed. A network left
//     behind is never reused, since its options cannot be trusted. One without
//     the label kiln.run=<ID> is refused, and nothing is touched.
//
// The proxy must carry the label kiln.proxy; one without it, or none, is
// refused first, and nothing is touched. The image is never pulled: one that is
// not there locally is an error. When a step fails, everything this call made
// is removed, also when the context was cancelled.
func CreateRun(ctx context.Context, r Run) error {
	if err := checkRunID(r.ID); err != nil {
		return err
	}
	if err := CheckLimits(r.Memory, r.CPUs); err != nil {
		return err
	}
	memory, cpus := r.Memory, r.CPUs
	if memory == "" {
		memory = defaultMemory
	}
	if cpus == 0 {
		cpus = defaultCPUs
	}
	if r.Instance != "" {
		if err := checkInstance(r.Instance); err != nil {
			return err
		}
	}
	if len(r.Hosts) > 0 {
		if err := checkHosts(r.Hosts); err != nil {
			return err
		}
		if r.WayOutImage == "" || r.WayOutNetwork == "" {
			return fmt.Errorf("a run with a list of hosts needs the way out's image and network")
		}
	}
	if _, err := kilnProxy(ctx, r.Proxy); err != nil {
		return err
	}
	keys := make([]string, 0, len(r.Labels))
	for k := range r.Labels {
		if k == "" || k == runLabel || k == instanceLabel || k == hostsLabel || k == cpusetLabel || strings.Contains(k, "=") {
			return fmt.Errorf("the label %q cannot be given for a run", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	name := runName(r.ID)
	engine, err := engineCPUs(ctx)
	if err != nil {
		return err
	}
	if cpus > engine {
		return cpusError{want: cpus, have: engine}
	}

	if err := removeLeftovers(ctx, name, r.ID, r.Proxy); err != nil {
		return err
	}
	if len(r.Hosts) > 0 {
		if err := webNetwork(ctx, r.WayOutNetwork); err != nil {
			return err
		}
	}

	if err := createRunNetwork(ctx, r.ID, r.Proxy, r.Instance); err != nil {
		return err // createRunNetwork leaves nothing behind
	}
	// undo removes what this call made after the network, by IDs, also when
	// the context is what failed the step.
	wayOut := ""
	undo := func(err error) error {
		ctx := context.WithoutCancel(ctx)
		if wayOut != "" {
			if _, rmErr := docker(ctx, nil, "rm", "--force", wayOut); rmErr != nil {
				return fmt.Errorf("%w (and removing the way out failed: %v)", err, rmErr)
			}
		}
		if rmErr := RemoveRunNetwork(ctx, r.ID, r.Proxy); rmErr != nil {
			err = fmt.Errorf("%w (and removing the network failed: %v)", err, rmErr)
		}
		return err
	}
	if len(r.Hosts) > 0 {
		var err error
		if wayOut, err = createWayOut(ctx, r); err != nil {
			return undo(err) // createWayOut leaves no way out behind
		}
	}

	args := []string{
		"--pull", "never", // a missing image is an error, never a download
		"--name", name,
		"--label", runLabel + "=" + r.ID,
	}
	if r.Instance != "" {
		args = append(args, "--label", instanceLabel+"="+r.Instance)
	}
	for _, k := range keys {
		args = append(args, "--label", k+"="+r.Labels[k])
	}
	if len(r.Hosts) > 0 {
		args = append(args, "--label", hostsLabel+"="+strings.Join(r.Hosts, ","))
	}
	args = append(args,
		"--network", name,
		// Not root, no capabilities, nothing gained by setuid programs (ADR 0041).
		"--user", "1000:1000",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--init")
	// The limits come in createPinned, between these and the rest.
	rest := []string{
		"--log-driver", "json-file",
		// Not a credential: the proxy replaces it (ADR 0017).
		"--env", "ANTHROPIC_BASE_URL=http://" + r.Proxy + ":" + proxyPort,
		"--env", "ANTHROPIC_API_KEY=kiln-placeholder",
		"--env", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", // ADR 0041, point 3
	}
	if len(r.Hosts) > 0 {
		// Claude Code takes the first of these for all its traffic, the model's
		// included, unless NO_PROXY names the proxy (ADR 0059). HTTP_PROXY is
		// not set: the way out answers only CONNECT.
		wayOutURL := "http://" + wayOutName(r.ID) + ":" + wayOutPort
		rest = append(rest,
			"--env", "HTTPS_PROXY="+wayOutURL,
			"--env", "https_proxy="+wayOutURL,
			"--env", "NO_PROXY="+r.Proxy,
			"--env", "no_proxy="+r.Proxy)
	}
	// Whatever follows is the image and its arguments, never an option.
	rest = append(rest, "--", r.Image)
	rest = append(rest, r.Command...)

	if err := createPinned(ctx, args, rest, memory, engine, cpus); err != nil {
		// createPinned left no container; the way out and the network
		// must go as well.
		return undo(err)
	}
	return nil
}

// createPinned creates a run's container from its arguments, head and rest,
// with its limits between them: its memory, its CPUs, and a cpuset of as many
// CPUs, chosen of the engine's, which it carries as the label kiln.cpuset as
// well. It holds cpusetMu from the listing of the CPUs in use until the
// create has returned.
func createPinned(ctx context.Context, head, rest []string, memory string, engine, cpus int) error {
	cpusetMu.Lock()
	defer cpusetMu.Unlock()
	used, err := usedCPUs(ctx)
	if err != nil {
		return err
	}
	cpuset := formatCPUs(chooseCPUs(engine, cpus, used))
	args := slices.Concat(head, []string{"--label", cpusetLabel + "=" + cpuset}, runLimits(memory, cpus, cpuset), rest)
	_, err = createContainer(ctx, nil, "create", args...)
	return err
}

// removeLeftovers removes what an earlier CreateRun of the same run left behind
// when it failed or was killed, and refuses a container that is not such a
// leftover.
func removeLeftovers(ctx context.Context, name, id, proxy string) error {
	c, err := findContainer(ctx, name, runLabel)
	if err != nil {
		return err
	}
	if c != nil {
		switch {
		case c.label != id:
			return notOurs("container", name, runLabel+"="+id)
		case c.state != "created":
			return fmt.Errorf("the container %s has started (it is %s); it is left alone", name, c.state)
		}
	}
	// A way out is looked at before anything is removed, so that one without
	// the label leaves everything as it was.
	w, err := findWayOut(ctx, id)
	if err != nil {
		return err
	}
	if c != nil {
		// By ID, so that a container that took the name in between is not
		// touched, and without --force, so that one that started in between is
		// refused.
		if _, err := docker(ctx, nil, "rm", c.id); err != nil {
			return err
		}
	}
	if w != nil {
		if _, err := docker(ctx, nil, "rm", "--force", w.id); err != nil {
			return err
		}
	}

	// RemoveRunNetwork refuses a network without the label, and succeeds when
	// there is none.
	return RemoveRunNetwork(ctx, id, proxy)
}

// ListRuns returns the IDs of the runs of an instance that have a container,
// a way out or a network on the engine: those named kiln-run-<id> or
// kiln-wayout-<id> that carry both the label kiln.run=<id> and
// kiln.instance=<instance>. Nothing without both labels is
// listed, so a run started by hand, which has no instance, and another
// instance's runs are never listed. Each ID is listed once, in order.
func ListRuns(ctx context.Context, instance string) ([]string, error) {
	if err := checkInstance(instance); err != nil {
		return nil, err
	}
	filter := "label=" + instanceLabel + "=" + instance
	format := "\t{{.Label \"" + runLabel + "\"}}"
	containers, err := docker(ctx, nil, "ps", "--all", "--no-trunc", "--filter", filter, "--format", "{{.Names}}"+format)
	if err != nil {
		return nil, err
	}
	networks, err := docker(ctx, nil, "network", "ls", "--no-trunc", "--filter", filter, "--format", "{{.Name}}"+format)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ids []string
	for _, line := range strings.Split(containers+"\n"+networks, "\n") {
		name, id, ok := strings.Cut(line, "\t")
		if !ok || id == "" || (name != runName(id) && name != wayOutName(id)) || checkRunID(id) != nil || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}
