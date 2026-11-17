package seal

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Run is what the caller says about a run's container. Its network, mounts,
// environment, user and limits are the seal's and cannot be given.
type Run struct {
	ID      string            // names the run's network and container, kiln-run-<ID>
	Image   string            // the run image (ADR 0039)
	Proxy   string            // the proxy container's name
	Command []string          // the container's arguments, after the image
	Labels  map[string]string // extra labels; kiln.run cannot be among them
}

// runLabel carries a run's ID on its container (ADR 0035, point 2), and marks
// the container as Kiln's.
const runLabel = "kiln.run"

var runID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// runName is the name of a run's network and of its container.
func runName(id string) string { return "kiln-run-" + id }

// CreateRun creates a run's network, with the proxy attached, and then its
// container. The container is created, not started: the caller places files in
// it and starts it.
//
// Before it creates anything it looks at what is there, so that it can be
// repeated after a failure or a kill at any step (ADR 0035):
//
//   - A container of the run's name with the label kiln.run=<ID> that has never
//     started is removed, and its network with it.
//   - A container of the name that has started, or that lacks the label, is
//     refused, and nothing is touched.
//   - A network of the name without a container is removed. A network left
//     behind is never reused, since its options cannot be trusted.
//
// When a step fails, everything this call made is removed, also when the
// context was cancelled.
func CreateRun(ctx context.Context, r Run) error {
	if !runID.MatchString(r.ID) {
		return fmt.Errorf("the run ID %q does not match %s", r.ID, runID)
	}
	keys := make([]string, 0, len(r.Labels))
	for k := range r.Labels {
		if k == "" || k == runLabel || strings.Contains(k, "=") {
			return fmt.Errorf("the label %q cannot be given for a run", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	name := runName(r.ID)

	if err := removeLeftovers(ctx, name, r.ID, r.Proxy); err != nil {
		return err
	}

	if err := CreateRunNetwork(ctx, name, r.Proxy); err != nil {
		return err // CreateRunNetwork leaves nothing behind
	}

	args := []string{
		"--name", name,
		"--label", runLabel + "=" + r.ID,
	}
	for _, k := range keys {
		args = append(args, "--label", k+"="+r.Labels[k])
	}
	args = append(args,
		"--network", name,
		// Not root, no capabilities, nothing gained by setuid programs (ADR 0041).
		"--user", "1000:1000",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--init",
		// 4 GiB without swap, 2 CPUs, 1024 processes (ADR 0017).
		"--memory", "4g",
		"--memory-swap", "4g",
		"--cpus", "2",
		"--pids-limit", "1024",
		"--log-driver", "json-file",
		// Not a credential: the proxy replaces it (ADR 0017).
		"--env", "ANTHROPIC_BASE_URL=http://"+r.Proxy+":"+proxyPort,
		"--env", "ANTHROPIC_API_KEY=kiln-placeholder",
		"--env", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", // ADR 0041, point 3
		// Whatever follows is the image and its arguments, never an option.
		"--", r.Image)
	args = append(args, r.Command...)

	if err := createContainer(ctx, nil, "create", args...); err != nil {
		// createContainer removed the container; the network must go as well,
		// also when the context is what failed the create.
		if rmErr := RemoveRunNetwork(context.WithoutCancel(ctx), name, r.Proxy); rmErr != nil {
			err = fmt.Errorf("%w (and removing the network failed: %v)", err, rmErr)
		}
		return err
	}
	return nil
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
			return fmt.Errorf("the container %s has no label %s=%s, so Kiln did not make it; it is left alone", name, runLabel, id)
		case c.state != "created":
			return fmt.Errorf("the container %s has started (it is %s); it is left alone", name, c.state)
		}
		// By ID, so that a container that took the name in between is not
		// touched, and without --force, so that one that started in between is
		// refused.
		if _, err := docker(ctx, nil, "rm", c.id); err != nil {
			return err
		}
	}

	exists, err := networkExists(ctx, name)
	if err != nil || !exists {
		return err
	}
	return RemoveRunNetwork(ctx, name, proxy)
}
