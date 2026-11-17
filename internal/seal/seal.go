// Package seal sets up what keeps a run's container from reaching anything but
// the proxy (ADR 0017, ADR 0041). It drives the container engine through the
// docker command.
package seal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The names of the variables the proxy starts from (cmd/proxy).
const (
	upstreamVar   = "KILN_PROXY_UPSTREAM"
	credentialVar = "KILN_PROXY_CREDENTIAL"
)

// proxyPort is where the proxy listens (cmd/proxy).
const proxyPort = "8080"

// proxyLabel marks a proxy container that Kiln made.
const proxyLabel = "kiln.proxy"

// Proxy is a proxy container.
type Proxy struct {
	Name     string // the container's name, which runs reach it by
	Image    string
	Network  string // an ordinary network, which the proxy reaches its upstream on
	Upstream string // the URL every request is forwarded to
}

// StartProxy starts the proxy on its ordinary network with packet forwarding
// switched off in both families, so that it cannot be used as a router between
// networks (ADR 0041, point 4). IPv6 forwarding is off by default; it is set
// anyway, so that the seal does not rest on a default. The credential reaches docker by the name of a variable and
// not as an argument, where `ps` would show it. The container carries the label
// kiln.proxy, by which ReplaceProxy knows it as Kiln's. The image is never
// pulled: one that is not there locally is an error. On failure nothing is left
// behind.
func StartProxy(ctx context.Context, p Proxy, credential string) error {
	return runDetached(ctx, []string{credentialVar + "=" + credential},
		"--pull", "never", // a missing image is an error, never a download
		"--name", p.Name,
		"--label", proxyLabel+"=1",
		"--network", p.Network,
		"--sysctl", "net.ipv4.ip_forward=0",
		"--sysctl", "net.ipv6.conf.all.forwarding=0",
		"--env", upstreamVar+"="+p.Upstream,
		"--env", credentialVar,
		p.Image)
}

// ReplaceProxy starts the proxy with a fresh credential from the instance's
// credential command (ADR 0040), replacing the proxy of that name if there is
// one. Each step can be repeated (ADR 0035):
//
//  1. It looks for a container of the proxy's name. One without the label
//     kiln.proxy was not made by Kiln: it is refused, and nothing is touched.
//  2. It runs the command with `sh -c` and takes its standard output, trimmed. A
//     failure or an empty output means the credential is unavailable; the error
//     does not carry the output, and nothing is touched.
//  3. It creates the proxy's ordinary network if it is missing.
//  4. It removes the container found in step 1, by its ID, so that a container
//     that took the name in between is not touched.
//  5. It starts the proxy with StartProxy.
//
// A replaced proxy is no longer attached to the run networks of earlier runs.
func ReplaceProxy(ctx context.Context, p Proxy, credentialCommand string) error {
	old, err := findContainer(ctx, p.Name, proxyLabel)
	if err != nil {
		return err
	}
	if old != nil && old.label == "" {
		return fmt.Errorf("the container %s has no label %s, so Kiln did not make it; it is left alone", p.Name, proxyLabel)
	}

	credential, err := runCredentialCommand(ctx, credentialCommand)
	if err != nil {
		return err
	}

	exists, err := networkExists(ctx, p.Network)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := docker(ctx, nil, "network", "create", p.Network); err != nil {
			return err
		}
	}

	if old != nil {
		// --force, since the old proxy runs; docker answers a container that is
		// already gone with success.
		if _, err := docker(ctx, nil, "rm", "--force", old.id); err != nil {
			return err
		}
	}
	return StartProxy(ctx, p, credential)
}

// runCredentialCommand runs the instance's credential command (ADR 0040) and
// returns what it printed to standard output, trimmed. Its standard error is
// discarded, and an error never carries what the command printed.
func runCredentialCommand(ctx context.Context, command string) (string, error) {
	out, err := exec.CommandContext(ctx, "sh", "-c", command).Output()
	if err != nil {
		// The text of an exit error is the exit status alone; Output keeps the
		// standard error in a field that is not part of it.
		return "", fmt.Errorf("credential unavailable: the credential command failed: %v", err)
	}
	credential := strings.TrimSpace(string(out))
	if credential == "" {
		return "", errors.New("credential unavailable: the credential command printed nothing")
	}
	return credential, nil
}

// runDetached runs `docker run --detach` with the given arguments, through
// createContainer.
func runDetached(ctx context.Context, env []string, args ...string) error {
	_, err := createContainer(ctx, env, "run", append([]string{"--detach"}, args...)...)
	return err
}

// createContainer runs a docker command that creates a container, `run` or
// `create`, with the given arguments, and returns the container's ID. When
// docker created the container but failed afterwards, for instance to start it,
// it leaves the container behind; createContainer then removes that container,
// found by the ID docker wrote to its cidfile, so that a container of the same
// name that docker refused to replace is never touched.
func createContainer(ctx context.Context, env []string, verb string, args ...string) (string, error) {
	dir, err := os.MkdirTemp("", "kiln-seal-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	cidfile := filepath.Join(dir, "cid")

	_, err = docker(ctx, env, append([]string{verb, "--cidfile", cidfile}, args...)...)
	data, readErr := os.ReadFile(cidfile)
	id := strings.TrimSpace(string(data))
	if err == nil {
		if readErr != nil || id == "" {
			return "", fmt.Errorf("docker %s wrote no container ID: %v", verb, readErr)
		}
		return id, nil
	}
	if readErr != nil || id == "" {
		return "", err // docker created nothing
	}
	// The context may be what failed the command, and the container must go anyway.
	if _, rmErr := docker(context.WithoutCancel(ctx), nil, "rm", "--force", id); rmErr != nil {
		err = fmt.Errorf("%w (and removing the container failed: %v)", err, rmErr)
	}
	return "", err
}

// CreateRunNetwork creates the network of one run, kiln-run-<id>, with no
// outside connection and no gateway in either family, and attaches the proxy to
// it (ADR 0041). The network carries the label kiln.run=<id>, by which the seal
// knows it as that run's.
//
// IPv6 is on on purpose. With it off, the network's bridge on the engine's host
// keeps IPv6 on with a link-local address, which a container with NET_RAW
// reaches by raw frames, and through it the host's services. With IPv6 on and
// its gateway mode isolated, the bridge has IPv6 off and no address.
//
// On failure nothing is left behind.
func CreateRunNetwork(ctx context.Context, id, proxy string) error {
	if err := checkRunID(id); err != nil {
		return err
	}
	name := runName(id)
	_, err := docker(ctx, nil,
		"network", "create",
		"--driver", "bridge",
		"--internal",
		"--ipv6",
		"--opt", "com.docker.network.bridge.gateway_mode_ipv4=isolated",
		"--opt", "com.docker.network.bridge.gateway_mode_ipv6=isolated",
		"--label", runLabel+"="+id,
		name)
	if err != nil {
		return err
	}
	if _, err := docker(ctx, nil, "network", "connect", name, proxy); err != nil {
		// The context may be what failed the connect, and the network must go anyway.
		if _, rmErr := docker(context.WithoutCancel(ctx), nil, "network", "rm", name); rmErr != nil {
			err = fmt.Errorf("%w (and removing the network failed: %v)", err, rmErr)
		}
		return err
	}
	return nil
}

// RemoveRunNetwork detaches the proxy from the network of a run and removes the
// network. Containers of the run must be gone already. A network of the name
// without the label kiln.run=<id> was not made for this run: it is refused, and
// nothing is touched.
//
// It can be repeated (ADR 0035): it succeeds when the proxy is already detached,
// when there is no proxy at all, and when the network is already gone. Whether
// the proxy exists is asked of the engine first; a proxy that does not exist is
// not attached, so the network is removed without a disconnect. docker answers
// a failed disconnect with the same exit status as any other failure, so it is
// judged by asking whether the proxy is still attached. The network is removed by its ID, so that a
// network that took the name in between is not touched, and `network rm
// --force` leaves out only the engine's "not found". Every other failure is
// returned.
func RemoveRunNetwork(ctx context.Context, id, proxy string) error {
	if err := checkRunID(id); err != nil {
		return err
	}
	name := runName(id)
	n, err := findNetwork(ctx, name, runLabel)
	if err != nil || n == nil {
		return err
	}
	if n.label != id {
		return notOurs("network", name, runLabel+"="+id)
	}
	p, err := findContainer(ctx, proxy, proxyLabel)
	if err != nil {
		return err
	}
	if p != nil {
		if _, err := docker(ctx, nil, "network", "disconnect", n.id, p.id); err != nil {
			attached, inspectErr := attachedTo(ctx, p.id, name)
			if inspectErr != nil || attached {
				return err
			}
		}
	}
	_, err = docker(ctx, nil, "network", "rm", "--force", n.id)
	return err
}

// attachedTo reports whether a container is attached to a network, by the
// network's name.
func attachedTo(ctx context.Context, container, network string) (bool, error) {
	out, err := docker(ctx, nil, "inspect", "--type", "container", "--format", "{{json .NetworkSettings.Networks}}", container)
	if err != nil {
		return false, err
	}
	var networks map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &networks); err != nil {
		return false, fmt.Errorf("the networks of %s: %w", container, err)
	}
	_, ok := networks[network]
	return ok, nil
}

// foundContainer is what the seal reads of a container it looks for by name.
type foundContainer struct {
	id    string
	state string // created, running, exited, ...
	label string // the value of the label asked for, empty without it
}

// findContainer looks for the container of a name, in any state, and reads one
// of its labels. It returns nil when there is none.
func findContainer(ctx context.Context, name, label string) (*foundContainer, error) {
	// The filter matches parts of names, so the name is compared below.
	out, err := docker(ctx, nil, "ps", "--all", "--no-trunc", "--filter", "name="+name,
		"--format", "{{.ID}}\t{{.Names}}\t{{.State}}\t{{.Label \""+label+"\"}}")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 4 && f[1] == name {
			return &foundContainer{id: f[0], state: f[2], label: f[3]}, nil
		}
	}
	return nil, nil
}

// foundNetwork is what the seal reads of a network it looks for by name.
type foundNetwork struct {
	id    string
	label string // the value of the label asked for, empty without it
}

// findNetwork looks for the network of a name and reads one of its labels. It
// returns nil when there is none.
func findNetwork(ctx context.Context, name, label string) (*foundNetwork, error) {
	// The filter matches parts of names, so the name is compared below.
	out, err := docker(ctx, nil, "network", "ls", "--no-trunc", "--filter", "name="+name,
		"--format", "{{.ID}}\t{{.Name}}\t{{.Label \""+label+"\"}}")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 3 && f[1] == name {
			return &foundNetwork{id: f[0], label: f[2]}, nil
		}
	}
	return nil, nil
}

// notOurs is the error for a container or network of a run's name that lacks
// the run's label.
func notOurs(what, name, label string) error {
	return fmt.Errorf("the %s %s has no label %s, so Kiln did not make it for this run; it is left alone", what, name, label)
}

// networkExists reports whether there is a network of a name.
func networkExists(ctx context.Context, name string) (bool, error) {
	// The filter matches parts of names, so the name is compared below.
	out, err := docker(ctx, nil, "network", "ls", "--filter", "name="+name, "--format", "{{.Name}}")
	if err != nil {
		return false, err
	}
	for _, n := range strings.Split(out, "\n") {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}

// docker runs the docker command with extra environment and returns what it
// printed to standard output, without the final newline. A failure carries what
// it printed to standard error.
func docker(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", dockerError(args, err, stderr.String())
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// dockerIO runs the docker command with the given standard input and output,
// either of which may be nil, as the bytes come. A failure carries what it
// printed to standard error.
func dockerIO(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin, cmd.Stdout = stdin, stdout
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return dockerError(args, err, stderr.String())
	}
	return nil
}

// dockerError names the docker command that failed and carries what it printed
// to standard error.
func dockerError(args []string, err error, stderr string) error {
	what := args[0]
	if what == "network" {
		what += " " + args[1]
	}
	return fmt.Errorf("docker %s: %w: %s", what, err, strings.TrimSpace(stderr))
}
