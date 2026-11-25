package seal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// A run's way out (ADR 0059): the container kiln-wayout-<id>, made for a run
// that has a list of hosts, on the run's network and on the outward network.
// It carries the labels kiln.run=<id> and kiln.wayout=1, and kiln.instance
// with an instance. Every function here acts only on a way out with the label
// kiln.run=<id>; anything else of the name is refused and left as it is.

// What a way out is started with (cmd/wayout).
const (
	wayOutHostsVar = "KILN_WAYOUT_HOSTS"
	wayOutPort     = "3128"
)

// The labels of a way out, of the outward network, and of a run's container
// that has a list.
const (
	wayOutLabel = "kiln.wayout"
	webLabel    = "kiln.web"
	hostsLabel  = "kiln.hosts"
)

// wayOutName is the name of a run's way out.
func wayOutName(id string) string { return "kiln-wayout-" + id }

// ErrNoWayOut is ReadWayOut's answer for a run that has no way out.
var ErrNoWayOut = errors.New("there is no way out")

// ErrWayOutGone is StartWayOut's answer for a run with a list whose way out is
// gone. The run goes on without the web.
var ErrWayOutGone = errors.New("the way out is gone; the run goes on without the web")

// checkHosts refuses a list that is not `*` alone or names as the way out
// reads them: lower-case letters, digits, hyphens and dots, with a dot, no
// empty label, and a last label that is not written as a number.
func checkHosts(hosts []string) error {
	if len(hosts) == 1 && hosts[0] == "*" {
		return nil
	}
	for _, h := range hosts {
		if !isHostName(h) {
			return fmt.Errorf("the list of hosts %q: %q is not a name in lower case with a dot, or * alone", hosts, h)
		}
	}
	return nil
}

// isHostName is cmd/wayout's isName.
func isHostName(h string) bool {
	if !strings.Contains(h, ".") || strings.Contains(h, "..") || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return false
	}
	last := h[strings.LastIndex(h, ".")+1:]
	if strings.HasPrefix(last, "0x") || strings.Trim(last, "0123456789") == "" {
		return false
	}
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// webNetwork creates the outward network if it is missing: an ordinary bridge
// with no traffic between its containers and the label kiln.web=1. One of the
// name without the label is refused, so that a way out is never put on a
// network Kiln did not make. It is never removed.
func webNetwork(ctx context.Context, name string) error {
	n, err := findNetwork(ctx, name, webLabel)
	if err != nil {
		return err
	}
	if n != nil {
		if n.label == "" {
			return fmt.Errorf("the network %s has no label %s, so Kiln did not make it; it is left alone", name, webLabel)
		}
		return nil
	}
	_, err = docker(ctx, nil, "network", "create", "--driver", "bridge",
		"--opt", "com.docker.network.bridge.enable_icc=false",
		"--label", webLabel+"=1", name)
	if err != nil {
		// Runs that start at once may each find it missing: one that is
		// refused finds it made by another (card 17).
		if n, findErr := findNetwork(ctx, name, webLabel); findErr == nil && n != nil && n.label != "" {
			return nil
		}
	}
	return err
}

// createWayOut creates a run's way out on the run's network, connects it to
// the outward network and starts it, and returns its ID. The program starts
// only once it is on both. With forwarding off in both families and no
// capabilities it cannot route between the networks (ADR 0041). The image is
// never pulled. On failure the way out is removed, by its ID.
func createWayOut(ctx context.Context, r Run) (string, error) {
	args := []string{
		"--pull", "never", // a missing image is an error, never a download
		"--name", wayOutName(r.ID),
		"--label", runLabel + "=" + r.ID,
		"--label", wayOutLabel + "=1",
	}
	if r.Instance != "" {
		args = append(args, "--label", instanceLabel+"="+r.Instance)
	}
	args = append(args,
		"--network", runName(r.ID),
		"--sysctl", "net.ipv4.ip_forward=0",
		"--sysctl", "net.ipv6.conf.all.forwarding=0",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--env", wayOutHostsVar+"="+strings.Join(r.Hosts, ","),
		"--", r.WayOutImage)
	id, err := createContainer(ctx, nil, "create", args...)
	if err != nil {
		return "", err // createContainer leaves nothing behind
	}
	if _, err = docker(ctx, nil, "network", "connect", r.WayOutNetwork, id); err == nil {
		_, err = docker(ctx, nil, "start", id)
	}
	if err != nil {
		// The context may be what failed the step, and the way out must go anyway.
		if _, rmErr := docker(context.WithoutCancel(ctx), nil, "rm", "--force", id); rmErr != nil {
			err = fmt.Errorf("%w (and removing the way out failed: %v)", err, rmErr)
		}
		return "", err
	}
	return id, nil
}

// findWayOut finds the way out of a run and refuses one without the label
// kiln.run=<id>. It returns nil when there is none.
func findWayOut(ctx context.Context, id string) (*foundContainer, error) {
	if err := checkRunID(id); err != nil {
		return nil, err
	}
	w, err := findContainer(ctx, wayOutName(id), runLabel)
	if err != nil || w == nil {
		return nil, err
	}
	if w.label != id {
		return nil, notOurs("container", wayOutName(id), runLabel+"="+id)
	}
	return w, nil
}

// removeWayOut removes the way out of a run, by its ID and with --force. One
// that is missing is fine; one without the label kiln.run=<id> is refused.
func removeWayOut(ctx context.Context, id string) error {
	w, err := findWayOut(ctx, id)
	if err != nil || w == nil {
		return err
	}
	_, err = docker(ctx, nil, "rm", "--force", w.id)
	return err
}

// hasHosts reports whether a run's container, by its ID, carries the label
// kiln.hosts, which it has when it was created with a list.
func hasHosts(ctx context.Context, container string) (bool, error) {
	out, err := docker(ctx, nil, "inspect", "--type", "container", "--format", "{{json .Config.Labels}}", container)
	if err != nil {
		return false, err
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(out), &labels); err != nil {
		return false, fmt.Errorf("the labels of %s: %w", container, err)
	}
	return labels[hostsLabel] != "", nil
}

// startWayOut starts a stopped way out, one that is created or exited, by its
// ID. One that runs is left as it is. It returns false when there is none.
func startWayOut(ctx context.Context, id string) (bool, error) {
	w, err := findWayOut(ctx, id)
	if err != nil || w == nil {
		return false, err
	}
	switch w.state {
	case "running":
		return true, nil
	case "created", "exited":
		_, err := docker(ctx, nil, "start", w.id)
		return true, err
	default:
		return true, fmt.Errorf("the way out %s is %s; it is left alone", wayOutName(id), w.state)
	}
}

// StartWayOut starts the stopped way out of a run whose container is already
// running, as after a restart of the engine or of the core. A way out that
// runs is left as it is, and a run without a list has nothing to start. A run
// with a list whose way out is gone gets an error that says so; the run goes
// on without the web, whose requests then fail: the error is ErrWayOutGone.
func StartWayOut(ctx context.Context, id string) error {
	c, err := existingRun(ctx, id)
	if err != nil {
		return err
	}
	found, err := startWayOut(ctx, id)
	if err != nil || found {
		return err
	}
	withHosts, err := hasHosts(ctx, c.id)
	if err != nil || !withHosts {
		return err
	}
	return fmt.Errorf("%s: %w", wayOutName(id), ErrWayOutGone)
}

// wayOutStopTimeout is docker stop's bound before it kills a way out, in
// seconds. It exceeds the way out's own wait for its connections' lines
// (cmd/wayout's stopWait).
const wayOutStopTimeout = "5"

// StopWayOut stops a run's way out with SIGTERM, so that it writes the line of
// every connection still open (cmd/wayout), and kills it after
// wayOutStopTimeout seconds. One that is missing has nothing to stop, one that
// has stopped is left as it is, and one without the label kiln.run=<id> is
// refused. It can be repeated.
func StopWayOut(ctx context.Context, id string) error {
	w, err := findWayOut(ctx, id)
	if err != nil || w == nil {
		return err
	}
	if w.state != "running" {
		return nil
	}
	_, err = docker(ctx, nil, "stop", "--timeout", wayOutStopTimeout, w.id)
	return err
}

// ReadWayOut writes the way out's output, one line of JSON per request and one
// more per connection at its end, as `docker logs` gives it, without what it
// wrote to standard error. It returns ErrNoWayOut when the run has none.
func ReadWayOut(ctx context.Context, id string, w io.Writer) error {
	c, err := findWayOut(ctx, id)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNoWayOut
	}
	cmd := commandIn(ctx, "docker", "logs", c.id)
	cmd.Stdout = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker logs of %s: %w", wayOutName(id), err)
	}
	return nil
}
