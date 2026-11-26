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
	"slices"
	"strings"
)

// upstreamVar is the variable the proxy reads its upstream from (cmd/proxy).
const upstreamVar = "KILN_PROXY_UPSTREAM"

// proxyPort is where the proxy listens (cmd/proxy).
const proxyPort = "8080"

// proxyLabel marks a proxy container that Kiln made, with proxyVersion as its
// value for the proxy StartProxy starts now. KeepProxy keeps only a proxy of
// this version, and replaces any other of Kiln's. The version moves whenever
// what StartProxy sets for the seal changes, such as the sysctls, the tmpfs
// of the push socket, the user or how the credential reaches the proxy, so
// that an older, weaker proxy is never kept. Version 1 took its credential
// from its environment; version 2 takes it by push.
const (
	proxyLabel   = "kiln.proxy"
	proxyVersion = "2"
)

// proxyUser is the user the proxy runs as (images/proxy), and pushDir the
// directory of its push socket (cmd/proxy).
const (
	proxyUser = "65532"
	pushDir   = "/run/kiln"
)

// ErrProxyDown is what PushCredential and ProxyUp wrap when the proxy is down:
// there is no container of its name, or the one of Kiln's is not running, as
// after a restart of the container engine. KeepProxy starts it again.
var ErrProxyDown = errors.New("the proxy is down")

// downError is a message that is ErrProxyDown.
type downError string

func (e downError) Error() string        { return string(e) }
func (e downError) Is(target error) bool { return target == ErrProxyDown }

// Proxy is a proxy container.
type Proxy struct {
	Name     string // the container's name, which runs reach it by
	Image    string
	Network  string // an ordinary network, which the proxy reaches its upstream on
	Upstream string // the URL every request is forwarded to
}

// StartProxy starts the proxy on its ordinary network, with no credential:
// PushCredential gives it one (ADR 0040). Packet forwarding is switched off in
// both families, so that it cannot be used as a router between networks
// (ADR 0041, point 4). IPv6 forwarding is off by default; it is set anyway, so
// that the seal does not rest on a default.
//
// The directory of the push socket is a tmpfs of the proxy's user with the
// mode 0700, so that only that user, in the proxy's own container, reaches
// the socket. A path in the container's own mounts is used, and not an
// abstract socket, because an abstract socket belongs to the network
// namespace, which another container can join (`--network container:`), and
// has no permissions at all.
//
// The container carries the label kiln.proxy, by which KeepProxy knows it as
// Kiln's. The image is never pulled: one that is not there locally is an
// error. On failure nothing is left behind.
func StartProxy(ctx context.Context, p Proxy) error {
	return runDetached(ctx, nil,
		"--pull", "never", // a missing image is an error, never a download
		"--name", p.Name,
		"--label", proxyLabel+"="+proxyVersion,
		"--network", p.Network,
		"--sysctl", "net.ipv4.ip_forward=0",
		"--sysctl", "net.ipv6.conf.all.forwarding=0",
		"--tmpfs", pushDir+":rw,noexec,nosuid,size=64k,uid="+proxyUser+",gid="+proxyUser+",mode=0700",
		"--env", upstreamVar+"="+p.Upstream,
		p.Image)
}

// KeepProxy makes sure the proxy runs, and reports whether it kept the one
// that was there. A proxy of proxyVersion that runs with the image of that
// name as it is now and with the same upstream is kept, since replacing it cuts the runs in
// flight from it; any other is replaced, and a missing one is started. A new
// proxy holds no credential until PushCredential. Each step can be repeated
// (ADR 0035):
//
//  1. It looks for a container of the proxy's name. One without the label
//     kiln.proxy was not made by Kiln: it is refused, and nothing is touched.
//  2. It keeps one of proxyVersion that runs with the image and the upstream.
//  3. It creates the proxy's ordinary network if it is missing.
//  4. It removes the container found in step 1, by its ID, so that a container
//     that took the name in between is not touched.
//  5. It starts the proxy with StartProxy.
//
// A replaced proxy is no longer attached to the run networks of earlier runs.
func KeepProxy(ctx context.Context, p Proxy) (bool, error) {
	old, err := findContainer(ctx, p.Name, proxyLabel)
	if err != nil {
		return false, err
	}
	if old != nil && old.label == "" {
		return false, fmt.Errorf("the container %s has no label %s, so Kiln did not make it; it is left alone", p.Name, proxyLabel)
	}
	if old != nil && old.state == "running" && old.label == proxyVersion {
		same, err := sameProxy(ctx, old.id, p)
		if err != nil || same {
			return same, err
		}
	}

	exists, err := networkExists(ctx, p.Network)
	if err != nil {
		return false, err
	}
	if !exists {
		if _, err := docker(ctx, nil, "network", "create", p.Network); err != nil {
			return false, err
		}
	}

	if old != nil {
		// --force, since the old proxy may run; docker answers a container that is
		// already gone with success.
		if _, err := docker(ctx, nil, "rm", "--force", old.id); err != nil {
			return false, err
		}
	}
	return false, StartProxy(ctx, p)
}

// sameProxy reports whether a proxy container was made from the image of the
// proxy's name as it is now, and forwards to its upstream.
func sameProxy(ctx context.Context, id string, p Proxy) (bool, error) {
	image, err := docker(ctx, nil, "image", "inspect", "--format", "{{.Id}}", p.Image)
	if err != nil {
		return false, err
	}
	out, err := docker(ctx, nil, "inspect", "--type", "container", "--format", "{{json .Image}}\t{{json .Config.Env}}", id)
	if err != nil {
		return false, err
	}
	imageJSON, envJSON, _ := strings.Cut(out, "\t")
	var got string
	var env []string
	if err := json.Unmarshal([]byte(imageJSON), &got); err != nil {
		return false, fmt.Errorf("the image of %s: %w", p.Name, err)
	}
	if err := json.Unmarshal([]byte(envJSON), &env); err != nil {
		return false, fmt.Errorf("the environment of %s: %w", p.Name, err)
	}
	return got == image && slices.Contains(env, upstreamVar+"="+p.Upstream), nil
}

// PushCredential gives the running proxy a credential, which replaces the one
// it holds (ADR 0040). The seal runs the proxy's own program in the proxy's
// container, `docker exec --interactive <proxy> /proxy push`, with the
// credential on its standard input; that program hands it to the proxy over
// the push socket (cmd/proxy). So the credential is never an argument, never
// in an environment and never in a file, and nothing outside the proxy's
// container can push one. A proxy without the label kiln.proxy is refused. A
// proxy that is not there, or does not run, gives an error that is
// ErrProxyDown, before anything is run in it. An error never carries the
// credential.
func PushCredential(ctx context.Context, proxy, credential string) error {
	return pushFrom(ctx, proxy, strings.NewReader(credential))
}

// pushFrom is PushCredential with the credential read from r.
func pushFrom(ctx context.Context, proxy string, r io.Reader) error {
	p, err := upProxy(ctx, proxy)
	if err != nil {
		return err
	}
	// By ID, so that a container that took the name in between gets nothing.
	return dockerIO(ctx, r, nil, "exec", "--interactive", p.id, "/proxy", "push")
}

// ProxyUp returns nil when the proxy of the name runs. A proxy that is not
// there, or is not running, gives an error that is ErrProxyDown; one without
// the label kiln.proxy is refused, and any other failure is returned as it is.
func ProxyUp(ctx context.Context, proxy string) error {
	_, err := upProxy(ctx, proxy)
	return err
}

// upProxy finds the running proxy container of a name. kilnProxy, which
// CreateRunNetwork and AttachProxy use, does not look at the state.
func upProxy(ctx context.Context, proxy string) (*foundContainer, error) {
	p, err := findContainer(ctx, proxy, proxyLabel)
	switch {
	case err != nil:
		return nil, err
	case p == nil:
		return nil, downError("there is no proxy " + proxy)
	case p.label == "":
		return nil, notOurs("container", proxy, proxyLabel)
	case p.state != "running":
		return nil, downError(fmt.Sprintf("the proxy %s is %s", proxy, p.state))
	}
	return p, nil
}

// dirKey is the key of the directory InDir puts in a context.
type dirKey struct{}

// InDir is a context under which every docker command the seal runs runs in
// dir, a directory its caller owns, and not in the directory the process was
// started in, which may have been removed since.
func InDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, dirKey{}, dir)
}

// commandIn is a command that runs in the context's directory, if InDir gave it
// one.
func commandIn(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir, ok := ctx.Value(dirKey{}).(string); ok {
		cmd.Dir = dir
	}
	return cmd
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
// The proxy must carry the label kiln.proxy. One without it, or none at all,
// is refused before anything is created, and nothing is touched: a mistyped
// proxy would otherwise put another container on the run's network.
//
// On failure nothing is left behind.
func CreateRunNetwork(ctx context.Context, id, proxy string) error {
	return createRunNetwork(ctx, id, proxy, "")
}

// createRunNetwork is CreateRunNetwork, with the label
// kiln.instance=<instance> on the network when an instance is given.
func createRunNetwork(ctx context.Context, id, proxy, instance string) error {
	if err := checkRunID(id); err != nil {
		return err
	}
	p, err := kilnProxy(ctx, proxy)
	if err != nil {
		return err
	}
	name := runName(id)
	args := []string{
		"network", "create",
		"--driver", "bridge",
		"--internal",
		"--ipv6",
		"--opt", "com.docker.network.bridge.gateway_mode_ipv4=isolated",
		"--opt", "com.docker.network.bridge.gateway_mode_ipv6=isolated",
		"--label", runLabel + "=" + id,
	}
	if instance != "" {
		args = append(args, "--label", instanceLabel+"="+instance)
	}
	_, err = docker(ctx, nil, append(args, name)...)
	if err != nil {
		return err
	}
	// By ID, so that a container that took the proxy's name in between is not
	// attached.
	if _, err := docker(ctx, nil, "network", "connect", name, p.id); err != nil {
		// The context may be what failed the connect, and the network must go anyway.
		if _, rmErr := docker(context.WithoutCancel(ctx), nil, "network", "rm", name); rmErr != nil {
			err = fmt.Errorf("%w (and removing the network failed: %v)", err, rmErr)
		}
		return err
	}
	return nil
}

// kilnProxy finds the proxy container of a name and refuses one without the
// label kiln.proxy, or none.
func kilnProxy(ctx context.Context, proxy string) (*foundContainer, error) {
	p, err := findContainer(ctx, proxy, proxyLabel)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("there is no proxy %s", proxy)
	}
	if p.label == "" {
		return nil, notOurs("container", proxy, proxyLabel)
	}
	return p, nil
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
	cmd := commandIn(ctx, "docker", args...)
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
	cmd := commandIn(ctx, "docker", args...)
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
