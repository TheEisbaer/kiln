// Package seal sets up what keeps a run's container from reaching anything but
// the proxy (ADR 0017, ADR 0041). It drives the container engine through the
// docker command.
package seal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// The names of the variables the proxy starts from (cmd/proxy).
const (
	upstreamVar   = "KILN_PROXY_UPSTREAM"
	credentialVar = "KILN_PROXY_CREDENTIAL"
)

// Proxy is a proxy container.
type Proxy struct {
	Name     string // the container's name, which runs reach it by
	Image    string
	Network  string // an ordinary network, which the proxy reaches its upstream on
	Upstream string // the URL every request is forwarded to
}

// StartProxy starts the proxy on its ordinary network with packet forwarding
// switched off, so that it cannot be used as a router between networks (ADR
// 0041, point 4). The credential reaches docker by the name of a variable and
// not as an argument, where `ps` would show it.
func StartProxy(ctx context.Context, p Proxy, credential string) error {
	_, err := docker(ctx, []string{credentialVar + "=" + credential},
		"run", "--detach",
		"--name", p.Name,
		"--network", p.Network,
		"--sysctl", "net.ipv4.ip_forward=0",
		"--env", upstreamVar+"="+p.Upstream,
		"--env", credentialVar,
		p.Image)
	return err
}

// CreateRunNetwork creates a network for one run, with no outside connection, no
// gateway and no IPv6, and attaches the proxy to it (ADR 0041). On failure
// nothing is left behind.
func CreateRunNetwork(ctx context.Context, name, proxy string) error {
	_, err := docker(ctx, nil,
		"network", "create",
		"--driver", "bridge",
		"--internal",
		"--opt", "com.docker.network.bridge.gateway_mode_ipv4=isolated",
		"--ipv6=false",
		name)
	if err != nil {
		return err
	}
	if _, err := docker(ctx, nil, "network", "connect", name, proxy); err != nil {
		if _, rmErr := docker(ctx, nil, "network", "rm", name); rmErr != nil {
			err = fmt.Errorf("%w (and removing the network failed: %v)", err, rmErr)
		}
		return err
	}
	return nil
}

// RemoveRunNetwork detaches the proxy from a run's network and removes the
// network. Containers of the run must be gone already.
func RemoveRunNetwork(ctx context.Context, name, proxy string) error {
	if _, err := docker(ctx, nil, "network", "disconnect", name, proxy); err != nil {
		return err
	}
	_, err := docker(ctx, nil, "network", "rm", name)
	return err
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
		what := args[0]
		if what == "network" {
			what += " " + args[1]
		}
		return "", fmt.Errorf("docker %s: %w: %s", what, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(string(out), "\n"), nil
}
