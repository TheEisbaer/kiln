package seal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

const (
	proxyPort    = "8080" // cmd/proxy
	upstreamPort = "8081" // testdata/upstream
)

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

	run1, run2 := runNetwork(t, name("run1"), proxy), runNetwork(t, name("run2"), proxy)

	// The probe: the toolchain image with default capabilities, as on a run.
	probe := name("probe")
	start(t, probe, run1, toolchainImage, "--entrypoint", "sleep", "--", "infinity")
	other := name("other")
	start(t, other, run2, upstreamImage)

	proxyURL := "http://" + proxy + ":" + proxyPort

	t.Run("through the proxy", func(t *testing.T) {
		var got map[string]string
		eventually(t, func() error {
			out, err := execIn(t, probe, "curl", "--silent", "--show-error", "--max-time", "5",
				"--header", "Authorization: Bearer the-probes-own",
				"--header", "X-Api-Key: the-probes-own",
				proxyURL+"/hello")
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
			noConnection(t, gateway(t, ordinary), hostPort)
		})
		t.Run("the published port at the default bridge", func(t *testing.T) {
			noConnection(t, gateway(t, "bridge"), hostPort)
		})
		t.Run("an internet address", func(t *testing.T) {
			noConnection(t, "1.1.1.1", "80")
		})
		t.Run("the container on the second run network", func(t *testing.T) {
			noConnection(t, ipOn(t, other, run2), upstreamPort)
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
		if gw := gateway(t, run1); gw != "" {
			t.Errorf("the run network has the gateway %q", gw)
		}

		table := must(t, "exec", probe, "cat", "/proc/net/route")
		routes := strings.Split(strings.TrimSpace(table), "\n")[1:] // after the header
		if len(routes) != 1 {
			t.Fatalf("the probe's routing table has %d routes, want only its own subnet:\n%s", len(routes), table)
		}
		cidr, _ := ipam(t, run1)
		_, subnet, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(routes[0])
		if len(f) < 8 || routeIP(f[1]).String() != subnet.IP.String() || f[2] != "00000000" ||
			routeIP(f[7]).String() != net.IP(subnet.Mask).String() {
			t.Errorf("the probe's only route is not its own subnet %s:\n%s", subnet, table)
		}

		// Read in the proxy's own network namespace, from a container that joins it.
		got := must(t, "run", "--rm", "--network", "container:"+proxy, "--entrypoint", "cat",
			toolchainImage, "/proc/sys/net/ipv4/ip_forward")
		if got != "0" {
			t.Errorf("ip_forward in the proxy's namespace is %q, want 0", got)
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
		run3 := runNetwork(t, name("run3"), real)
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

// runNetwork creates a run network with the proxy attached, and removes it
// afterwards.
func runNetwork(t *testing.T, name, proxy string) string {
	t.Helper()
	if err := CreateRunNetwork(t.Context(), name, proxy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := RemoveRunNetwork(context.Background(), name, proxy); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return name
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

// ipam returns the subnet and the gateway address of a network; the gateway is
// empty when it has none. The template's own output for a missing one differs
// between engine versions, so the JSON is read.
func ipam(t *testing.T, network string) (subnet, gateway string) {
	t.Helper()
	var config []struct{ Subnet, Gateway string }
	if err := json.Unmarshal([]byte(must(t, "network", "inspect", network, "--format", "{{json .IPAM.Config}}")), &config); err != nil || len(config) != 1 {
		t.Fatalf("the IPAM configuration of %s: %v, %d entries", network, err, len(config))
	}
	return config[0].Subnet, config[0].Gateway
}

func gateway(t *testing.T, network string) string {
	t.Helper()
	_, gw := ipam(t, network)
	return gw
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
