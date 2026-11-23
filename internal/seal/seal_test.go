package seal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
	proxyImage := build(t, "proxy", "images/proxy/Dockerfile")
	upstreamImage := build(t, "upstream", "internal/seal/testdata/upstream/Dockerfile")

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

	// The proxy, which holds no credential until the push, and two run
	// networks.
	proxy := name("proxy")
	proxyConfig := Proxy{Name: proxy, Image: proxyImage, Network: ordinary,
		Upstream: "http://" + upstream + ":" + upstreamPort}
	if err := StartProxy(t.Context(), proxyConfig); err != nil {
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

	t.Run("no credential before the first push", func(t *testing.T) {
		assertNoCredential(t, probe, proxyURL)
	})

	t.Run("the push is in no inspect and in no ps line", func(t *testing.T) {
		testPushUnseen(t, proxy, credential)
	})

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

	// The push has no address: a run cannot push or read a credential, and
	// the socket is reached by none but the proxy's user in its container.
	t.Run("no push from outside the proxy", func(t *testing.T) {
		testNoPush(t, probe, proxy, proxyURL, credential)
	})

	// What a run reaches of the proxy: the proxy's network namespace listens
	// on the API's port and on nothing else, in TCP or UDP and in either
	// family, but for the engine's resolver on its loopback; the push socket
	// is a file, not an address.
	t.Run("the proxy listens on its API port and nothing else", func(t *testing.T) {
		api := false
		for _, table := range []string{"tcp", "tcp6", "udp", "udp6"} {
			out := must(t, "run", "--rm", "--network", "container:"+proxy, "--entrypoint", "cat",
				toolchainImage, "/proc/net/"+table)
			for _, line := range strings.Split(out, "\n")[1:] {
				f := strings.Fields(line)
				if len(f) < 4 {
					continue
				}
				_, port, _ := strings.Cut(f[1], ":")
				listens := f[3] == "0A" || strings.HasPrefix(table, "udp") // TCP_LISTEN, or a bound UDP socket
				addr, _, _ := strings.Cut(f[1], ":")
				switch {
				case !listens:
				// The engine's own resolver, which it puts on 127.0.0.11 in
				// every container on a network of its own, on the loopback
				// only, where no other container reaches it.
				case addr == "0B00007F":
				case strings.HasPrefix(table, "tcp") && port == fmt.Sprintf("%04X", 8080):
					api = true
				default:
					t.Errorf("the proxy's namespace listens on %s %s", table, f[1])
				}
			}
		}
		if !api {
			t.Error("the proxy's namespace does not list its API port, so this proves nothing")
		}
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
		// Any other failure is still returned: run2 still has a container.
		if err := RemoveRunNetwork(t.Context(), runID("run2"), proxy); err == nil {
			t.Error("with a container still attached: no error")
		}
		if got := must(t, "inspect", "--format", `{{json .NetworkSettings.Networks}}`, other); !strings.Contains(got, `"`+run2+`"`) {
			t.Errorf("the container on run2 was detached: %s", got)
		}
	})

	// Last, since it replaces the proxy that the cases above use.
	t.Run("a replaced proxy has no credential until the next push", func(t *testing.T) {
		if kept, err := KeepProxy(t.Context(), proxyConfig); err != nil || !kept {
			t.Fatalf("KeepProxy of the proxy that runs: kept %v, %v", kept, err)
		}
		before := must(t, "inspect", "--format", "{{.Id}}", proxy)
		must(t, "stop", proxy)
		if kept, err := KeepProxy(t.Context(), proxyConfig); err != nil || kept {
			t.Fatalf("KeepProxy of a stopped proxy: kept %v, %v", kept, err)
		}
		if got := must(t, "inspect", "--format", "{{.Id}}", proxy); got == before {
			t.Fatal("the stopped proxy was not replaced")
		}
		if err := AttachProxy(t.Context(), runID("run1"), proxy); err != nil {
			t.Fatal(err)
		}
		assertNoCredential(t, probe, proxyURL)
		if err := PushCredential(t.Context(), proxy, credential+"-again"); err != nil {
			t.Fatal(err)
		}
		if got := seenKey(t, probe, proxyURL); got != credential+"-again" {
			t.Errorf("after the push the upstream saw %q", got)
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
		if err := StartProxy(t.Context(), Proxy{Name: real, Image: proxyImage, Network: ordinary, Upstream: claudeUpstream}); err != nil {
			t.Fatal(err)
		}
		cleanup(t, "rm", "--force", real)
		if err := PushCredential(t.Context(), real, strings.TrimSpace(string(out))); err != nil {
			t.Fatal(err)
		}
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

// seenKey is the X-Api-Key the upstream saw on a request of the probe's
// through the proxy, tried for a while for a proxy that has only just
// started.
func seenKey(t *testing.T, probe, proxyURL string) string {
	t.Helper()
	var got map[string]string
	eventually(t, func() error {
		out, err := execIn(t, probe, "curl", "--silent", "--show-error", "--fail", "--max-time", "5", proxyURL+"/hello")
		if err != nil {
			return errors.New(out)
		}
		return json.Unmarshal([]byte(out), &got)
	})
	return got["x-api-key"]
}

// assertNoCredential asserts that the proxy answers the probe's request with
// its own 503, which names the cause, and forwards nothing: the upstream
// would answer 200 with what it saw.
func assertNoCredential(t *testing.T, probe, proxyURL string) {
	t.Helper()
	var out string
	eventually(t, func() error {
		var err error
		out, err = execIn(t, probe, "curl", "--silent", "--show-error", "--max-time", "5",
			"--write-out", "\n%{http_code}", proxyURL+"/hello")
		return err
	})
	if !strings.HasSuffix(out, "\n503") || !strings.Contains(out, "holds no Claude credential") {
		t.Errorf("want the proxy's own 503, got %s", out)
	}
}

// testPushUnseen pushes a credential from a pipe that it holds open while it
// reads every docker inspect of the proxy, the processes in the proxy's
// container and the processes on this machine, and asserts that none of them
// carries the credential. Then it lets the push end.
func testPushUnseen(t *testing.T, proxy, credential string) {
	// A variable of this process, which the docker command inherits, shows
	// that the environments below are read at all.
	canary := "kiln-sealtest-canary-" + randomID(t)
	t.Setenv("KILN_SEALTEST_CANARY", canary)
	since := time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- pushFrom(t.Context(), proxy, pr) }()
	if _, err := io.WriteString(pw, credential); err != nil {
		t.Fatal(err)
	}
	// The push is under way: `proxy push` runs in the container, waiting for
	// the end of its standard input.
	eventually(t, func() error {
		if top := must(t, "top", proxy, "-o", "pid,args"); !strings.Contains(top, "/proxy push") {
			return fmt.Errorf("no push in the proxy's container yet:\n%s", top)
		}
		return nil
	})
	seen := map[string]string{
		"docker inspect of the proxy":            must(t, "inspect", proxy),
		"the processes in the proxy's container": must(t, "top", proxy, "-o", "pid,args"),
	}
	// The arguments and the environments of the processes on this machine,
	// the docker command's included.
	if out, err := exec.CommandContext(t.Context(), "ps", "axeww", "-o", "args").Output(); err != nil {
		t.Errorf("ps: %v", err)
	} else if !strings.Contains(string(out), "docker exec --interactive") || !strings.Contains(string(out), canary) {
		t.Errorf("ps shows no docker exec with its environment, so it proves nothing:\n%s", out)
	} else {
		seen["the processes on this machine, with their environments"] = string(out)
	}
	// The environment of the push in the proxy's container, read from a
	// container on the proxy's process namespace, which needs SYS_PTRACE to
	// read another user's environment.
	environ := must(t, "run", "--rm", "--pid", "container:"+proxy, "--cap-add", "SYS_PTRACE", "--network", "none",
		"--entrypoint", "sh", toolchainImage, "-c",
		`for p in /proc/[0-9]*; do if [ "$(tr '\0' ' ' <"$p/cmdline" 2>/dev/null)" = "/proxy push " ]; then tr '\0' '\n' <"$p/environ"; fi; done`)
	if !strings.Contains(environ, upstreamVar+"=") {
		t.Errorf("no environment of the push was read, so it proves nothing: %q", environ)
	}
	seen["the environment of the push in the proxy's container"] = environ
	// What the engine records of the exec.
	events := must(t, "events", "--since", since, "--until", time.Now().UTC().Format(time.RFC3339Nano),
		"--filter", "container="+proxy, "--filter", "event=exec_create", "--format", "{{json .}}")
	if !strings.Contains(events, "/proxy push") {
		t.Errorf("the engine recorded no exec of the push, so it proves nothing: %s", events)
	}
	seen["the engine's record of the exec"] = events
	for what, text := range seen {
		if strings.Contains(text, credential) {
			t.Errorf("the credential is in %s", what)
		}
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if inspect := must(t, "inspect", proxy); strings.Contains(inspect, credential) {
		t.Error("the credential is in docker inspect of the proxy after the push")
	}
}

// testNoPush asserts that the probe, a run on its sealed network, reaches the
// proxy's API and cannot push or read a credential; that a container on the
// proxy's own network namespace does not see the push socket; and that
// another user in the proxy's container cannot push.
func testNoPush(t *testing.T, probe, proxy, proxyURL, credential string) {
	t.Run("a run reaches the API and pushes nothing", func(t *testing.T) {
		for _, path := range []string{"/push", "/run/kiln/push.sock", "/credential"} {
			out, err := execIn(t, probe, "curl", "--silent", "--show-error", "--max-time", "5",
				"--data", "pushed-by-a-run", "--dump-header", "/dev/stdout", proxyURL+path)
			if err != nil {
				t.Fatalf("%s: %v: %s", path, err, out)
			}
			// The fake upstream echoes the headers it saw in its body; the
			// proxy itself adds none to an answer.
			head, _, _ := strings.Cut(out, "\r\n\r\n")
			if strings.Contains(head, credential) {
				t.Errorf("%s: the proxy's answer carries the credential in its headers:\n%s", path, head)
			}
		}
		if got := seenKey(t, probe, proxyURL); got != credential {
			t.Errorf("after the run's requests the upstream saw %q, want the credential pushed", got)
		}
		if out, err := execIn(t, probe, "ls", "-a", "/run/kiln"); err == nil {
			t.Errorf("the run has /run/kiln: %s", out)
		}
	})

	// The namespace's /proc/net/unix lists the socket by its path, so an
	// abstract socket would be reached from here; a path in the proxy's own
	// mounts is not there at all.
	t.Run("a container on the proxy's network namespace", func(t *testing.T) {
		listed := must(t, "run", "--rm", "--network", "container:"+proxy, "--entrypoint", "cat",
			toolchainImage, "/proc/net/unix")
		if !strings.Contains(listed, pushDir+"/push.sock") {
			t.Fatalf("the namespace lists no push socket, so the check below proves nothing:\n%s", listed)
		}
		out, err := docker(t.Context(), nil, "run", "--rm", "--network", "container:"+proxy, "--entrypoint", "ls",
			toolchainImage, "-a", pushDir)
		if err == nil {
			t.Errorf("it has the push socket's directory: %s", out)
		}
	})

	t.Run("another user in the proxy's container", func(t *testing.T) {
		_, err := dockerIOString(t, "pushed-by-another-user", "exec", "--interactive", "--user", "1000", proxy, "/proxy", "push")
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("want the push refused for want of permission, got %v", err)
		}
		if got := seenKey(t, probe, proxyURL); got != credential {
			t.Errorf("after the other user's push the upstream saw %q", got)
		}
	})
}

// dockerIOString runs docker with stdin and returns what it printed.
func dockerIOString(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	err := dockerIO(t.Context(), strings.NewReader(stdin), &out, args...)
	return out.String(), err
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

// images are the images the package's tests build, by Dockerfile. Each is
// built once per run of the tests, and TestMain removes them at the end.
var images struct {
	sync.Mutex
	id   string            // random; names the images
	tags map[string]string // by Dockerfile
}

func TestMain(m *testing.M) {
	notifyStop()
	code := m.Run()
	for _, tag := range images.tags {
		if _, err := docker(context.Background(), nil, "rmi", tag); err != nil {
			fmt.Fprintf(os.Stderr, "removing the image %s: %v\n", tag, err)
			if code == 0 {
				code = 1
			}
		}
	}
	os.Exit(code)
}

// build returns the image of a Dockerfile, which it builds with the repo's
// root as the context the first time it is asked for it. Its name is
// kiln-sealtest-<random id>-<what>.
func build(t *testing.T, what, dockerfile string) string {
	t.Helper()
	images.Lock()
	defer images.Unlock()
	if tag, ok := images.tags[dockerfile]; ok {
		return tag
	}
	if images.tags == nil {
		images.id, images.tags = randomID(t), map[string]string{}
	}
	tag := "kiln-sealtest-" + images.id + "-" + what
	images.tags[dockerfile] = tag // removed by TestMain even if the build fails halfway
	must(t, "build", "--quiet", "--tag", tag, "--file", "../../"+dockerfile, "../..")
	return tag
}

// start starts a detached container on a network, with extra options for docker
// run before the image's arguments, and removes it afterwards. A start that fails
// leaves nothing behind (runDetached).
func start(t *testing.T, name, network, image string, more ...string) {
	t.Helper()
	failIfStopped(t)
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
