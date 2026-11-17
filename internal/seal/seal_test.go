package seal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// toolchainImage is the toolchain image (ADR 0039), which check/in-image builds
// under this name. The probes run in it, and the proxy and the upstream are built
// from it.
const toolchainImage = "kiln-toolchain"

// upstreamPort is where testdata/upstream listens. The proxy's is proxyPort.
const upstreamPort = "8081"

// TestSeal is a large test (ADR 0031, ADR 0048): it needs the container engine.
// It builds the proxy, puts a probe on a run's network and asserts that the
// probe reaches the fake upstream through the proxy and nothing else.
//
// It runs only with KILN_ENGINE_TESTS=1, and then fails, not skips, when the
// engine cannot be reached. Every name it uses carries a random id that no
// instance uses, and everything it creates is removed, even on failure.
func TestSeal(t *testing.T) {
	if os.Getenv("KILN_ENGINE_TESTS") != "1" {
		t.Skip("needs the container engine; set KILN_ENGINE_TESTS=1")
	}
	if _, err := docker(t.Context(), nil, "info", "--format", "{{.ServerVersion}}"); err != nil {
		t.Fatalf("the container engine is not reachable: %v", err)
	}

	id := randomID(t)
	name := func(what string) string { return "kiln-sealtest-" + id + "-" + what }
	runID := func(what string) string { return "sealtest-" + id + "-" + what }
	credential := "fake-credential-" + id

	// Images. The repo's root is the build context.
	proxyImage, upstreamImage := name("proxy"), name("upstream")
	build(t, proxyImage, "images/proxy/Dockerfile")
	build(t, upstreamImage, "internal/seal/testdata/upstream/Dockerfile")

	// The ordinary network, with the fake upstream and a container that publishes
	// a port on every host address.
	ordinary := name("ordinary")
	must(t, "network", "create", ordinary)
	cleanup(t, "network", "rm", ordinary)

	upstream := name("upstream")
	start(t, upstream, ordinary, upstreamImage)
	published := name("published")
	start(t, published, ordinary, upstreamImage, "--publish", upstreamPort)
	hostPort := lastField(must(t, "port", published, upstreamPort+"/tcp"), ":")

	// The proxy, and two run networks.
	proxy := name("proxy")
	if err := StartProxy(t.Context(), Proxy{
		Name: proxy, Image: proxyImage, Network: ordinary,
		Upstream: "http://" + upstream + ":" + upstreamPort,
	}, credential); err != nil {
		t.Fatal(err)
	}
	cleanup(t, "rm", "--force", proxy)

	run1, run2 := runNetwork(t, runID("run1"), proxy), runNetwork(t, runID("run2"), proxy)

	// The probe: the toolchain image with default capabilities, as on a run.
	probe := name("probe")
	start(t, probe, run1, toolchainImage, "--entrypoint", "sleep", "--", "infinity")
	other := name("other")
	start(t, other, run2, upstreamImage)

	proxyURL := "http://" + proxy + ":" + proxyPort

	t.Run("through the proxy", func(t *testing.T) {
		for _, target := range []struct{ what, host string }{
			{"by its name", proxy},
			{"at its IPv4 address", ipOn(t, proxy, run1)},
			{"at its IPv6 address", ip6On(t, proxy, run1)},
		} {
			t.Run(target.what, func(t *testing.T) {
				var got map[string]string
				eventually(t, func() error {
					out, err := execIn(t, probe, "curl", "--silent", "--show-error", "--max-time", "5",
						"--header", "Authorization: Bearer the-probes-own",
						"--header", "X-Api-Key: the-probes-own",
						"http://"+net.JoinHostPort(target.host, proxyPort)+"/hello")
					if err != nil {
						return errors.New(out)
					}
					return json.Unmarshal([]byte(out), &got)
				})
				if got["path"] != "/hello" {
					t.Errorf("the upstream saw path %q", got["path"])
				}
				if got["authorization"] != "Bearer "+credential || got["x-api-key"] != credential {
					t.Errorf("the upstream saw Authorization %q and X-Api-Key %q, want the credential in both",
						got["authorization"], got["x-api-key"])
				}
			})
		}
	})

	t.Run("a request for another host lands at the upstream", func(t *testing.T) {
		out, err := execIn(t, probe, "curl", "--silent", "--show-error", "--max-time", "5",
			"--proxy", proxyURL, "http://elsewhere.example/other/path")
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(out), &got); err != nil || got["path"] != "/other/path" {
			t.Errorf("the upstream saw %q (%v)", out, err)
		}
	})

	t.Run("CONNECT gets 405", func(t *testing.T) {
		out, err := execIn(t, probe, "curl", "--silent", "--show-error", "--max-time", "5",
			"--proxy", proxyURL, "https://example.com/")
		if err == nil || !strings.Contains(out, "405") {
			t.Errorf("want a CONNECT refused with 405, got %v: %s", err, out)
		}
	})

	// A connection that fails at once, with "unreachable", not "refused": a
	// refusal would mean something answered at that address.
	noConnection := func(t *testing.T, host, port string) {
		t.Helper()
		out, err := execIn(t, probe, "curl", "--silent", "--show-error",
			"--connect-timeout", "3", "--max-time", "5", "http://"+net.JoinHostPort(host, port)+"/")
		var exit *exec.ExitError
		if !errors.As(err, &exit) || (exit.ExitCode() != 7 && exit.ExitCode() != 28) {
			t.Errorf("%s:%s: want no connection, got %v: %s", host, port, err, out)
		}
		if strings.Contains(strings.ToLower(out), "refused") {
			t.Errorf("%s:%s: connection refused, so something answered there: %s", host, port, out)
		}
	}

	t.Run("no connection at all", func(t *testing.T) {
		t.Run("the upstream's own address", func(t *testing.T) {
			noConnection(t, ipOn(t, upstream, ordinary), upstreamPort)
		})
		t.Run("the published port at the ordinary network's gateway", func(t *testing.T) {
			noConnection(t, gateway4(t, ordinary), hostPort)
		})
		t.Run("the published port at the default bridge", func(t *testing.T) {
			noConnection(t, gateway4(t, "bridge"), hostPort)
		})
		t.Run("an internet address over IPv4", func(t *testing.T) {
			noConnection(t, "1.1.1.1", "80")
		})
		t.Run("an internet address over IPv6", func(t *testing.T) {
			noConnection(t, "2606:4700:4700::1111", "80")
		})
		t.Run("the container on the second run network at its IPv4 address", func(t *testing.T) {
			noConnection(t, ipOn(t, other, run2), upstreamPort)
		})
		t.Run("the container on the second run network at its IPv6 address", func(t *testing.T) {
			noConnection(t, ip6On(t, other, run2), upstreamPort)
		})
	})

	t.Run("no name resolves", func(t *testing.T) {
		if out, err := execIn(t, probe, "getent", "hosts", proxy); err != nil {
			t.Fatalf("the proxy's name does not resolve, so the checks below prove nothing: %v: %s", err, out)
		}
		for _, host := range []string{"example.com", other} {
			if out, err := execIn(t, probe, "getent", "hosts", host); err == nil {
				t.Errorf("%s resolved: %s", host, out)
			}
		}
	})

	t.Run("no way out", func(t *testing.T) {
		subnets := ipam(t, run1)
		for _, c := range subnets {
			if c.Gateway != "" {
				t.Errorf("the run network has the gateway %q on %s", c.Gateway, c.Subnet)
			}
		}

		// The network's bridge, read on the engine's host from a container that
		// only reads. With IPv6 off on the network, the bridge keeps IPv6 on with
		// a link-local address, which a run with NET_RAW reaches.
		bridge := "br-" + must(t, "network", "inspect", run1, "--format", "{{.Id}}")[:12]
		onHost := func(file string) string {
			return must(t, "run", "--rm", "--network", "host", "--entrypoint", "cat", toolchainImage, file)
		}
		if got := onHost("/proc/sys/net/ipv6/conf/" + bridge + "/disable_ipv6"); got != "1" {
			t.Errorf("disable_ipv6 of the run network's bridge %s is %q, want 1", bridge, got)
		}
		for _, line := range strings.Split(onHost("/proc/net/if_inet6"), "\n") {
			if f := strings.Fields(line); len(f) == 6 && f[5] == bridge {
				t.Errorf("the run network's bridge %s has the IPv6 address %s", bridge, f[0])
			}
		}

		table := must(t, "exec", probe, "cat", "/proc/net/route")
		routes := strings.Split(strings.TrimSpace(table), "\n")[1:] // after the header
		if len(routes) != 1 {
			t.Fatalf("the probe's routing table has %d routes, want only its own subnet:\n%s", len(routes), table)
		}
		subnet := subnetOf(t, subnets, false)
		f := strings.Fields(routes[0])
		if len(f) < 8 || routeIP(f[1]).String() != subnet.IP.String() || f[2] != "00000000" ||
			routeIP(f[7]).String() != net.IP(subnet.Mask).String() {
			t.Errorf("the probe's only route is not its own subnet %s:\n%s", subnet, table)
		}

		table6 := must(t, "exec", probe, "cat", "/proc/net/ipv6_route")
		for _, problem := range ipv6RouteProblems(table6, subnetOf(t, subnets, true)) {
			t.Errorf("the probe's IPv6 routes: %s\n%s", problem, table6)
		}

		// Read in the proxy's own network namespace, from a container that joins it.
		for _, setting := range []string{"net/ipv4/ip_forward", "net/ipv6/conf/all/forwarding"} {
			got := must(t, "run", "--rm", "--network", "container:"+proxy, "--entrypoint", "cat",
				toolchainImage, "/proc/sys/"+setting)
			if got != "0" {
				t.Errorf("%s in the proxy's namespace is %q, want 0", strings.ReplaceAll(setting, "/", "."), got)
			}
		}
	})

	// Removing a run network is safe to repeat (ADR 0035): after a crash between
	// the disconnect and the removal, and after a create that never attached the
	// proxy.
	t.Run("removing a run network can be repeated", func(t *testing.T) {
		againID := runID("again")
		again := runName(againID)
		if err := CreateRunNetwork(t.Context(), againID, proxy); err != nil {
			t.Fatal(err)
		}
		cleanup(t, "network", "rm", "--force", again)
		if got := must(t, "network", "inspect", "--format", `{{index .Labels "`+runLabel+`"}}`, again); got != againID {
			t.Errorf("the run network has %s=%q, want %q", runLabel, got, againID)
		}

		must(t, "network", "disconnect", again, proxy)
		if err := RemoveRunNetwork(t.Context(), againID, proxy); err != nil {
			t.Errorf("with the proxy already detached: %v", err)
		}
		if out, err := docker(t.Context(), nil, "network", "inspect", again); err == nil {
			t.Errorf("the network is still there: %s", out)
		}
		if err := RemoveRunNetwork(t.Context(), againID, proxy); err != nil {
			t.Errorf("with the network already gone: %v", err)
		}
		// Any other failure is still returned.
		if err := RemoveRunNetwork(t.Context(), runID("run2"), name("no-such-proxy")); err == nil {
			t.Error("with a proxy that does not exist: no error")
		}
	})

	// Needs the developer's credential, so it is run by hand (ADR 0049).
	t.Run("Claude answers through the proxy", func(t *testing.T) {
		claudeUpstream, command := os.Getenv("KILN_CLAUDE_UPSTREAM"), os.Getenv("KILN_CLAUDE_CREDENTIAL_COMMAND")
		if claudeUpstream == "" || command == "" {
			t.Skip("set KILN_CLAUDE_UPSTREAM and KILN_CLAUDE_CREDENTIAL_COMMAND to run it")
		}
		out, err := exec.CommandContext(t.Context(), "sh", "-c", command).Output() // ADR 0040
		if err != nil {
			t.Fatalf("the credential command failed: %v", err)
		}

		real := name("claude-proxy")
		if err := StartProxy(t.Context(), Proxy{Name: real, Image: proxyImage, Network: ordinary, Upstream: claudeUpstream},
			strings.TrimSpace(string(out))); err != nil {
			t.Fatal(err)
		}
		cleanup(t, "rm", "--force", real)
		run3 := runNetwork(t, runID("run3"), real)
		probe3 := name("probe3")
		start(t, probe3, run3, toolchainImage, "--entrypoint", "sleep", "--", "infinity")

		var status string
		eventually(t, func() error {
			var err error
			status, err = execIn(t, probe3, "curl", "--silent", "--show-error", "--max-time", "60",
				"--output", "/dev/stderr", "--write-out", "%{http_code}",
				"--header", "content-type: application/json", "--header", "anthropic-version: 2023-06-01",
				"--data", `{"model":"claude-haiku-5-5","max_tokens":1,"messages":[{"role":"user","content":"Hi"}]}`,
				"http://"+real+":"+proxyPort+"/v1/messages")
			return err
		})
		if !strings.HasSuffix(status, "200") {
			t.Errorf("Claude answered: %s", status)
		}
	})
}

func randomID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// must runs docker and fails the test when it fails.
func must(t *testing.T, args ...string) string {
	t.Helper()
	out, err := docker(t.Context(), nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// cleanup runs docker when the test is over, whatever happened.
func cleanup(t *testing.T, args ...string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := docker(context.Background(), nil, args...); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
}

// build builds an image from the repo's root and removes it afterwards.
func build(t *testing.T, tag, dockerfile string) {
	t.Helper()
	must(t, "build", "--quiet", "--tag", tag, "--file", "../../"+dockerfile, "../..")
	cleanup(t, "rmi", tag)
}

// start starts a detached container on a network, with extra options for docker
// run before the image's arguments, and removes it afterwards. A start that fails
// leaves nothing behind (runDetached).
func start(t *testing.T, name, network, image string, more ...string) {
	t.Helper()
	// Options go before the image; whatever follows "--" goes after it.
	var options, command []string
	for i, m := range more {
		if m == "--" {
			command = more[i+1:]
			break
		}
		options = append(options, m)
	}
	args := append([]string{"--name", name, "--network", network}, options...)
	if err := runDetached(t.Context(), nil, append(append(args, image), command...)...); err != nil {
		t.Fatal(err)
	}
	cleanup(t, "rm", "--force", name)
}

// runNetwork creates the network of a run with the proxy attached, removes it
// afterwards, and returns its name.
func runNetwork(t *testing.T, id, proxy string) string {
	t.Helper()
	if err := CreateRunNetwork(t.Context(), id, proxy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := RemoveRunNetwork(context.Background(), id, proxy); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return runName(id)
}

// execIn runs a command in a container and returns its output, standard error
// included.
func execIn(t *testing.T, container string, args ...string) (string, error) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "docker", append([]string{"exec", container}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// eventually tries f for up to ten seconds, for a container that has only just
// started.
func eventually(t *testing.T, f func() error) {
	t.Helper()
	var err error
	for range 40 {
		if err = f(); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal(err)
}

func ipOn(t *testing.T, container, network string) string {
	t.Helper()
	return must(t, "inspect", container, "--format", `{{(index .NetworkSettings.Networks "`+network+`").IPAddress}}`)
}

func ip6On(t *testing.T, container, network string) string {
	t.Helper()
	ip := must(t, "inspect", container, "--format", `{{(index .NetworkSettings.Networks "`+network+`").GlobalIPv6Address}}`)
	// Without IPv6 the template prints "invalid IP" or nothing, depending on the
	// engine's version.
	if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() != nil {
		t.Fatalf("%s has no IPv6 address on %s (docker printed %q)", container, network, ip)
	}
	return ip
}

// subnetConfig is one family's entry in a network's IPAM configuration.
type subnetConfig struct{ Subnet, Gateway string }

// ipam returns a network's IPAM configuration, one entry per family. A gateway
// is empty when the family has none. The template's own output for a missing
// one differs between engine versions, so the JSON is read.
func ipam(t *testing.T, network string) []subnetConfig {
	t.Helper()
	var config []subnetConfig
	if err := json.Unmarshal([]byte(must(t, "network", "inspect", network, "--format", "{{json .IPAM.Config}}")), &config); err != nil {
		t.Fatalf("the IPAM configuration of %s: %v", network, err)
	}
	return config
}

// subnetOf returns the subnet of one family.
func subnetOf(t *testing.T, config []subnetConfig, ipv6 bool) *net.IPNet {
	t.Helper()
	for _, c := range config {
		_, subnet, err := net.ParseCIDR(c.Subnet)
		if err == nil && (subnet.IP.To4() == nil) == ipv6 {
			return subnet
		}
	}
	t.Fatalf("no %s subnet in %v", map[bool]string{false: "IPv4", true: "IPv6"}[ipv6], config)
	return nil
}

// gateway4 returns the IPv4 gateway address of a network.
func gateway4(t *testing.T, network string) string {
	t.Helper()
	for _, c := range ipam(t, network) {
		if ip := net.ParseIP(c.Gateway); ip != nil && ip.To4() != nil {
			return c.Gateway
		}
	}
	t.Fatalf("%s has no IPv4 gateway", network)
	return ""
}

// The flags of /proc/net/ipv6_route that the probe's routes are judged by.
const (
	rtfReject = 0x0200
	rtfLocal  = 0x80000000
)

// ipv6RouteProblems judges a routing table as /proc/net/ipv6_route prints it.
// It allows routes within the run's subnet, link-local and multicast, none with
// a next hop. On lo it allows only the kernel's own reject entries and the
// local route of the loopback address.
func ipv6RouteProblems(table string, subnet *net.IPNet) []string {
	linkLocal := &net.IPNet{IP: net.ParseIP("fe80::"), Mask: net.CIDRMask(10, 128)}
	multicast := &net.IPNet{IP: net.ParseIP("ff00::"), Mask: net.CIDRMask(8, 128)}
	within := func(dest net.IP, plen int, n *net.IPNet) bool {
		ones, _ := n.Mask.Size()
		return n.Contains(dest) && plen >= ones
	}

	var problems []string
	for _, line := range strings.Split(strings.TrimSpace(table), "\n") {
		f := strings.Fields(line)
		if len(f) != 10 {
			problems = append(problems, fmt.Sprintf("cannot read the line %q", line))
			continue
		}
		dest, destErr := hex.DecodeString(f[0])
		plen, plenErr := strconv.ParseUint(f[1], 16, 8)
		flags, flagsErr := strconv.ParseUint(f[8], 16, 32)
		if destErr != nil || len(dest) != 16 || plenErr != nil || flagsErr != nil {
			problems = append(problems, fmt.Sprintf("cannot read the line %q", line))
			continue
		}
		route := fmt.Sprintf("%s/%d on %s", net.IP(dest), plen, f[9])
		switch {
		case strings.Trim(f[4], "0") != "":
			problems = append(problems, route+" has a next hop")
		case f[9] == "lo":
			if flags&rtfReject == 0 && !(net.IP(dest).Equal(net.IPv6loopback) && plen == 128 && flags&rtfLocal != 0) {
				problems = append(problems, route+" is neither a reject entry nor the loopback address")
			}
		case !within(dest, int(plen), subnet) && !within(dest, int(plen), linkLocal) && !within(dest, int(plen), multicast):
			problems = append(problems, route+" is outside the run's subnet, link-local and multicast")
		}
	}
	return problems
}

func lastField(s, sep string) string {
	first, _, _ := strings.Cut(s, "\n")
	return first[strings.LastIndex(first, sep)+1:]
}

// routeIP reads an address as /proc/net/route prints it: hexadecimal, in the
// byte order of the machine, which is little-endian where Kiln runs.
func routeIP(hex string) net.IP {
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return nil
	}
	return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}
