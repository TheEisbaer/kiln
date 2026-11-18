package seal

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Linux's error numbers, the same on arm64 and amd64. The test runs on macOS as
// well, where the syscall package has other numbers, so they are written out.
const (
	linuxEPERM        = 1
	linuxENETUNREACH  = 101
	linuxECONNREFUSED = 111
	linuxEHOSTUNREACH = 113
)

// probeConfig and probeReport are testdata/probe's Config and Report.
type probeConfig struct {
	HTTP, Dial, Lookup, Files, List []string
}

type probeReport struct {
	HTTP, Dial, Lookup, Sockets, Files []probeResult
	Entries                            []probeEntry
	Init                               string
	Env                                []string
}

type probeEntry struct {
	Path       string
	Uid, Gid   uint32
	Mode, Link string
	Error      string
}

type probeResult struct {
	What, Value, Error string
	Errno              int
}

// The probe's path in a run's container, and its configuration's. PlaceRun puts
// them under /work.
const (
	probePath       = "/work/probe"
	probeConfigPath = "/work/probe.json"
)

// TestRun is a large test (ADR 0031, ADR 0048): it needs the container engine.
// It starts a container exactly as a run is started, through CreateRun and
// PlaceRun, with the probe as its command, and asserts what the probe finds from
// inside. It does the same for the save container. It also repeats and fails
// the seal's functions for a run at each step, and checks the save.
//
// It runs only with KILN_ENGINE_TESTS=1, and then fails, not skips, when the
// engine cannot be reached. Every name it uses carries a random id that no
// instance uses, and everything it creates is removed, even on failure.
func TestRun(t *testing.T) {
	if os.Getenv("KILN_ENGINE_TESTS") != "1" {
		t.Skip("needs the container engine; set KILN_ENGINE_TESTS=1")
	}
	if _, err := docker(t.Context(), nil, "info", "--format", "{{.ServerVersion}}"); err != nil {
		t.Fatalf("the container engine is not reachable: %v", err)
	}

	id := randomID(t)
	name := func(what string) string { return "kiln-runtest-" + id + "-" + what }
	runID := func(what string) string { return "runtest-" + id + "-" + what }
	credential1, credential2 := "fake-credential-1-"+id, "fake-credential-2-"+id

	// Images. The repo's root is the build context.
	proxyImage, upstreamImage, runImage := name("proxy"), name("upstream"), name("run")
	build(t, proxyImage, "images/proxy/Dockerfile")
	build(t, upstreamImage, "internal/seal/testdata/upstream/Dockerfile")
	build(t, runImage, "images/run/Dockerfile")
	probe := buildProbe(t)

	// The proxy, which creates its ordinary network, then the fake upstream and
	// a container that publishes a port on every host address.
	ordinary := name("ordinary")
	cleanup(t, "network", "rm", "--force", ordinary)
	upstream := name("upstream")
	proxy := Proxy{Name: name("proxy"), Image: proxyImage, Network: ordinary,
		Upstream: "http://" + upstream + ":" + upstreamPort}
	cleanup(t, "rm", "--force", proxy.Name)
	if err := ReplaceProxy(t.Context(), proxy, "echo "+credential1); err != nil {
		t.Fatal(err)
	}
	start(t, upstream, ordinary, upstreamImage)
	published := name("published")
	start(t, published, ordinary, upstreamImage, "--publish", upstreamPort)
	hostPort := lastField(must(t, "port", published, upstreamPort+"/tcp"), ":")

	// Another run, which the probe must not reach.
	other := runName(runID("other"))
	createRun(t, Run{ID: runID("other"), Image: upstreamImage, Proxy: proxy.Name})
	must(t, "start", other)

	// The probe run.
	probeRun := runName(runID("probe"))
	createRun(t, Run{ID: runID("probe"), Image: runImage, Proxy: proxy.Name,
		Command: []string{probePath, probeConfigPath}})
	f := fixture{runImage: runImage, upstreamImage: upstreamImage, proxyImage: proxyImage,
		proxy: proxy, probe: probe, name: name, runID: runID}
	files := []string{"/proc/self/status", "/proc/self/mountinfo"}
	for _, f := range cgroupFiles {
		files = append(files, "/sys/fs/cgroup/"+f)
	}
	proxyURL := func(host string) string { return "http://" + net.JoinHostPort(host, proxyPort) + "/hello" }
	noConnection := []string{
		net.JoinHostPort(ipOn(t, upstream, ordinary), upstreamPort),
		net.JoinHostPort(gateway4(t, ordinary), hostPort),
		net.JoinHostPort(gateway4(t, "bridge"), hostPort),
		"1.1.1.1:80",
		"[2606:4700:4700::1111]:80",
		net.JoinHostPort(ipOn(t, other, other), upstreamPort),
		net.JoinHostPort(ip6On(t, other, other), upstreamPort),
	}
	proxyIPs := []string{ipOn(t, proxy.Name, probeRun), ip6On(t, proxy.Name, probeRun)}
	report, logs, diff := runProbe(t, probe, runID("probe"), probeConfig{
		HTTP:   []string{proxyURL(proxy.Name), proxyURL(proxyIPs[0]), proxyURL(proxyIPs[1])},
		Dial:   noConnection,
		Lookup: []string{proxy.Name, "example.com", other},
		Files:  files,
		List:   []string{"/work", "/kiln"},
	}, placedExtra...)

	t.Run("placing", func(t *testing.T) { testPlacing(t, report, diff) })

	t.Run("the save container", func(t *testing.T) {
		testSaveContainer(t, f, []string{
			net.JoinHostPort(proxyIPs[0], proxyPort),
			net.JoinHostPort(proxyIPs[1], proxyPort),
			"1.1.1.1:80",
			"[2606:4700:4700::1111]:80",
		})
	})

	t.Run("identity and privileges", func(t *testing.T) {
		status := statusFields(t, fileIn(t, report, "/proc/self/status"))
		for _, field := range []string{"Uid", "Gid"} {
			if got := strings.Fields(status[field]); len(got) != 4 || slices.ContainsFunc(got, func(v string) bool { return v != "1000" }) {
				t.Errorf("%s is %q, want 1000 throughout", field, status[field])
			}
		}
		for _, field := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
			if v, err := strconv.ParseUint(status[field], 16, 64); err != nil || v != 0 {
				t.Errorf("%s is %q, want 0", field, status[field])
			}
		}
		// The run image's own user is uid 1000 as well, so the uid alone does not
		// show that CreateRun sets it.
		if got := must(t, "inspect", "--format", "{{.Config.User}}", probeRun); got != "1000:1000" {
			t.Errorf("the run's container has the user %q, want 1000:1000", got)
		}
		if status["NoNewPrivs"] != "1" {
			t.Errorf("NoNewPrivs is %q, want 1", status["NoNewPrivs"])
		}
		if len(report.Sockets) != 3 {
			t.Fatalf("the probe tried %d raw sockets, want 3", len(report.Sockets))
		}
		for _, s := range report.Sockets {
			if s.Errno != linuxEPERM {
				t.Errorf("socket %s: want EPERM (%d), got %q (error number %d)", s.What, linuxEPERM, s.Value+s.Error, s.Errno)
			}
		}
	})

	t.Run("through the proxy", func(t *testing.T) {
		assertCredential(t, report, credential1)
	})

	t.Run("no connection", func(t *testing.T) {
		if len(report.Dial) != len(noConnection) {
			t.Fatalf("the probe tried %d addresses, want %d", len(report.Dial), len(noConnection))
		}
		for _, d := range report.Dial {
			switch d.Errno {
			case linuxENETUNREACH, linuxEHOSTUNREACH:
			case linuxECONNREFUSED:
				t.Errorf("%s: connection refused, so something answered there: %s", d.What, d.Error)
			default:
				t.Errorf("%s: want a failure at once as unreachable, got %q (error number %d)", d.What, d.Value+d.Error, d.Errno)
			}
		}
	})

	t.Run("names", func(t *testing.T) {
		if len(report.Lookup) != 3 {
			t.Fatalf("the probe resolved %d names, want 3", len(report.Lookup))
		}
		if l := report.Lookup[0]; l.Error != "" || l.Value == "" {
			t.Fatalf("the proxy's name does not resolve, so the checks below prove nothing: %s", l.Error)
		}
		for _, l := range report.Lookup[1:] {
			if l.Error == "" {
				t.Errorf("%s resolved to %s", l.What, l.Value)
			}
		}
	})

	t.Run("limits", func(t *testing.T) { assertLimits(t, report) })

	t.Run("mounts", func(t *testing.T) {
		if report.Init == "" {
			t.Fatal("the probe found no init program as process 1")
		}
		for _, problem := range mountProblems(fileIn(t, report, "/proc/self/mountinfo"), report.Init) {
			t.Error(problem)
		}
	})

	t.Run("credential", func(t *testing.T) {
		for _, env := range []string{
			"ANTHROPIC_BASE_URL=http://" + proxy.Name + ":" + proxyPort,
			"ANTHROPIC_API_KEY=kiln-placeholder",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		} {
			if !slices.Contains(report.Env, env) {
				t.Errorf("the environment lacks %s: %q", env, report.Env)
			}
		}
		if strings.Contains(logs, credential1) {
			t.Errorf("the credential is in the probe's output:\n%s", logs)
		}
		if inspect := must(t, "inspect", probeRun); strings.Contains(inspect, credential1) {
			t.Error("the credential is in docker inspect of the run's container")
		}
		if got := must(t, "inspect", "--format", "{{json .Mounts}}", probeRun); got != "[]" {
			t.Errorf("the run's container has mounts: %s", got)
		}
		for _, field := range []string{".HostConfig.PortBindings", ".NetworkSettings.Ports"} {
			var ports map[string]any
			got := must(t, "inspect", "--format", "{{json "+field+"}}", probeRun)
			if err := json.Unmarshal([]byte(got), &ports); err != nil || len(ports) != 0 {
				t.Errorf("%s of the run's container is %s, want none", field, got)
			}
		}
	})

	t.Run("the run image has the pinned Claude Code", func(t *testing.T) {
		createRun(t, Run{ID: runID("version"), Image: runImage, Proxy: proxy.Name, Command: []string{"claude", "--version"}})
		if out, _ := startAndWait(t, runID("version")); strings.TrimSpace(out) != "2.1.326 (Claude Code)" {
			t.Errorf("claude --version printed %q", out)
		}
	})

	t.Run("ReplaceProxy", func(t *testing.T) {
		before := containerID(t, proxy.Name)

		for _, command := range []string{"echo leaked-" + id + "; exit 1", "true"} {
			err := ReplaceProxy(t.Context(), proxy, command)
			if err == nil {
				t.Errorf("with %q: no error", command)
			} else if strings.Contains(err.Error(), "leaked") {
				t.Errorf("with %q: the error carries the output: %v", command, err)
			}
			if got := containerID(t, proxy.Name); got != before {
				t.Errorf("with %q: the proxy is %s, was %s", command, got, before)
			}
		}

		if err := ReplaceProxy(t.Context(), proxy, "echo "+credential2); err != nil {
			t.Fatal(err)
		}
		if got := containerID(t, proxy.Name); got == before || got == "" {
			t.Errorf("the proxy is %q, was %s: not replaced", got, before)
		}
		createRun(t, Run{ID: runID("probe2"), Image: runImage, Proxy: proxy.Name,
			Command: []string{probePath, probeConfigPath}})
		report2, _, _ := runProbe(t, probe, runID("probe2"), probeConfig{HTTP: []string{proxyURL(proxy.Name)}})
		assertCredential(t, report2, credential2)

		// The replaced proxy is on no earlier run's network until it is attached.
		t.Run("AttachProxy", func(t *testing.T) { testAttachProxy(t, f, runID("probe")) })

		// As after a kill between removing the old proxy and starting the new.
		must(t, "rm", "--force", proxy.Name)
		if err := ReplaceProxy(t.Context(), proxy, "echo "+credential2); err != nil {
			t.Errorf("with no proxy: %v", err)
		}
		if got := must(t, "inspect", "--format", "{{.State.Status}}", proxy.Name); got != "running" {
			t.Errorf("with no proxy: the proxy is %s", got)
		}

		foreign := name("foreign")
		must(t, "create", "--name", foreign, "--network", "none", upstreamImage)
		cleanup(t, "rm", "--force", foreign)
		foreignID := containerID(t, foreign)
		marker := filepath.Join(t.TempDir(), "ran")
		if err := ReplaceProxy(t.Context(), Proxy{Name: foreign, Image: proxyImage, Network: ordinary, Upstream: proxy.Upstream},
			"touch "+marker+"; echo "+credential2); err == nil {
			t.Error("with a container that lacks kiln.proxy: no error")
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("with a container that lacks kiln.proxy: the credential command ran")
		}
		if got := containerID(t, foreign); got != foreignID {
			t.Errorf("with a container that lacks kiln.proxy: it is %q, was %s", got, foreignID)
		}
	})

	t.Run("CreateRun", func(t *testing.T) {
		t.Run("with what it cannot take", func(t *testing.T) {
			for _, r := range []Run{
				{ID: runID("label"), Image: runImage, Proxy: proxy.Name, Labels: map[string]string{runLabel: "other"}},
				{ID: runID("label"), Image: runImage, Proxy: proxy.Name, Labels: map[string]string{runLabel + "=other": ""}},
				{ID: "Not-an-ID", Image: runImage, Proxy: proxy.Name},
			} {
				cleanupRun(t, r)
				if err := CreateRun(t.Context(), r); err == nil {
					t.Errorf("%+v: no error", r)
				}
				assertLeft(t, r.ID, false, false)
			}
		})

		t.Run("without a proxy", func(t *testing.T) {
			r := Run{ID: runID("no-proxy"), Image: runImage, Proxy: name("no-such-proxy")}
			cleanupRun(t, r)
			if err := CreateRun(t.Context(), r); err == nil {
				t.Error("no error")
			}
			assertLeft(t, r.ID, false, false)
		})

		// A mistyped proxy names a container that is not Kiln's. It is not put on
		// a run's network, where the run could reach it, and it is not touched.
		t.Run("with a proxy that lacks kiln.proxy", func(t *testing.T) {
			foreign := name("foreign-proxy")
			must(t, "create", "--name", foreign, "--network", ordinary, upstreamImage)
			cleanup(t, "rm", "--force", foreign)
			before := must(t, "inspect", "--format", "{{.Id}} {{.State.Status}} {{json .NetworkSettings.Networks}}", foreign)
			r := Run{ID: runID("foreign-proxy"), Image: runImage, Proxy: foreign}
			cleanupRun(t, r)
			if err := CreateRun(t.Context(), r); err == nil || !strings.Contains(err.Error(), proxyLabel) {
				t.Errorf("CreateRun: want a refusal for the label %s, got %v", proxyLabel, err)
			}
			assertLeft(t, r.ID, false, false)
			if err := CreateRunNetwork(t.Context(), r.ID, foreign); err == nil || !strings.Contains(err.Error(), proxyLabel) {
				t.Errorf("CreateRunNetwork: want a refusal for the label %s, got %v", proxyLabel, err)
			}
			assertLeft(t, r.ID, false, false)
			if got := must(t, "inspect", "--format", "{{.Id}} {{.State.Status}} {{json .NetworkSettings.Networks}}", foreign); got != before {
				t.Errorf("the container that lacks kiln.proxy was touched:\nbefore %s\nafter  %s", before, got)
			}
		})

		t.Run("with an image that does not exist", func(t *testing.T) {
			r := Run{ID: runID("no-image"), Image: name("no-such-image"), Proxy: proxy.Name}
			cleanupRun(t, r)
			err := CreateRun(t.Context(), r)
			if err == nil || !strings.Contains(err.Error(), "No such image") {
				t.Errorf("want the image missing locally, got %v", err)
			}
			assertLeft(t, r.ID, false, false)
		})

		t.Run("after a network left alone", func(t *testing.T) {
			r := Run{ID: runID("left-network"), Image: runImage, Proxy: proxy.Name}
			cleanupRun(t, r)
			if err := CreateRunNetwork(t.Context(), r.ID, proxy.Name); err != nil {
				t.Fatal(err)
			}
			network := networkID(t, runName(r.ID))
			if err := CreateRun(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			assertLeft(t, r.ID, true, true)
			if got := networkID(t, runName(r.ID)); got == network {
				t.Errorf("the network %s left behind was reused", network)
			}
		})

		t.Run("after a container that never started", func(t *testing.T) {
			r := Run{ID: runID("left-container"), Image: runImage, Proxy: proxy.Name}
			cleanupRun(t, r)
			if err := CreateRun(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			container, network := containerID(t, runName(r.ID)), networkID(t, runName(r.ID))
			if err := CreateRun(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			assertLeft(t, r.ID, true, true)
			if got := containerID(t, runName(r.ID)); got == container {
				t.Errorf("the container %s left behind was kept", container)
			}
			if got := networkID(t, runName(r.ID)); got == network {
				t.Errorf("the network %s left behind was reused", network)
			}
		})

		t.Run("after a run that has exited", func(t *testing.T) {
			container, network := containerID(t, probeRun), networkID(t, probeRun)
			if err := CreateRun(t.Context(), Run{ID: runID("probe"), Image: runImage, Proxy: proxy.Name}); err == nil {
				t.Error("no error")
			}
			if got := containerID(t, probeRun); got != container {
				t.Errorf("the container is %q, was %s", got, container)
			}
			if got := networkID(t, probeRun); got != network {
				t.Errorf("the network is %q, was %s", got, network)
			}
		})

		t.Run("after a network without kiln.run", func(t *testing.T) {
			r := Run{ID: runID("foreign-network"), Image: runImage, Proxy: proxy.Name}
			cleanupRun(t, r)
			must(t, "network", "create", "--internal", runName(r.ID))
			network := networkID(t, runName(r.ID))
			if err := CreateRun(t.Context(), r); err == nil {
				t.Error("no error")
			}
			assertLeft(t, r.ID, false, true)
			if got := networkID(t, runName(r.ID)); got != network {
				t.Errorf("the network is %q, was %s", got, network)
			}
		})

		t.Run("after a container without kiln.run", func(t *testing.T) {
			r := Run{ID: runID("foreign"), Image: runImage, Proxy: proxy.Name}
			cleanupRun(t, r)
			must(t, "create", "--name", runName(r.ID), "--network", "none", upstreamImage)
			container := containerID(t, runName(r.ID))
			if err := CreateRun(t.Context(), r); err == nil {
				t.Error("no error")
			}
			assertLeft(t, r.ID, true, false)
			if got := containerID(t, runName(r.ID)); got != container {
				t.Errorf("the container is %q, was %s", got, container)
			}
		})
	})

	t.Run("a run's life", func(t *testing.T) { testLife(t, f, runID("probe")) })
	t.Run("PlaceRun refuses", func(t *testing.T) { testPlaceRefusals(t, f, runID("probe")) })
	t.Run("the save", func(t *testing.T) { testSave(t, f) })
	t.Run("RemoveRun", func(t *testing.T) { testRemoveRun(t, f) })
	t.Run("images are never pulled", func(t *testing.T) { testNoPull(t, f) })

	// Needs the developer's credential, so it is run by hand (ADR 0049).
	t.Run("Claude answers in a run", func(t *testing.T) {
		claudeUpstream, command := os.Getenv("KILN_CLAUDE_UPSTREAM"), os.Getenv("KILN_CLAUDE_CREDENTIAL_COMMAND")
		if claudeUpstream == "" || command == "" {
			t.Skip("set KILN_CLAUDE_UPSTREAM and KILN_CLAUDE_CREDENTIAL_COMMAND to run it")
		}
		real := Proxy{Name: name("claude-proxy"), Image: proxyImage, Network: ordinary, Upstream: claudeUpstream}
		cleanup(t, "rm", "--force", real.Name)
		if err := ReplaceProxy(t.Context(), real, command); err != nil {
			t.Fatal(err)
		}
		createRun(t, Run{ID: runID("claude"), Image: runImage, Proxy: real.Name, Command: []string{
			"claude", "-p", "--output-format", "stream-json", "--verbose",
			"--permission-mode", "bypassPermissions", "--model", "claude-haiku-5-5", "--effort", "low",
			"Reply with the single word OK.",
		}})
		out, _ := startAndWait(t, runID("claude"))

		var model, subtype, result string
		var cost float64
		for _, line := range strings.Split(out, "\n") {
			var event struct {
				Type, Subtype, Model, Result string
				IsError                      bool    `json:"is_error"`
				Cost                         float64 `json:"total_cost_usd"`
			}
			if json.Unmarshal([]byte(line), &event) != nil {
				continue
			}
			switch {
			case event.Type == "system" && event.Subtype == "init":
				model = event.Model
			case event.Type == "result":
				subtype, result, cost = event.Subtype, event.Result, event.Cost
				if event.IsError {
					subtype += " (is_error)"
				}
			}
		}
		if model != "claude-haiku-5-5" {
			t.Errorf("the init event names the model %q", model)
		}
		if subtype != "success" || !strings.Contains(result, "OK") {
			t.Errorf("the result is %q with %q; the output:\n%s", subtype, result, out)
		}
		t.Logf("total_cost_usd: %v", cost)
	})
}

// buildProbe builds testdata/probe in the toolchain image with the network off,
// for Linux on the engine's architecture, and returns the program. It comes out
// on standard output, as in check/in-image.
func buildProbe(t *testing.T) []byte {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	arch := must(t, "version", "--format", "{{.Server.Arch}}")
	cmd := exec.CommandContext(t.Context(), "docker", "run", "--rm", "--network", "none",
		"--volume", root+":/src:ro", "--workdir", "/src",
		"--env", "GOOS=linux", "--env", "GOARCH="+arch, "--env", "CGO_ENABLED=0",
		toolchainImage, "sh", "-c",
		"go build -trimpath -o /tmp/probe ./internal/seal/testdata/probe && cat /tmp/probe")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("building the probe: %v: %s", err, stderr.String())
	}
	return out
}

// createRun calls CreateRun and removes the run's container and network
// afterwards.
func createRun(t *testing.T, r Run) {
	t.Helper()
	cleanupRun(t, r)
	if err := CreateRun(t.Context(), r); err != nil {
		t.Fatal(err)
	}
}

// cleanupRun removes a run's save container, container and network when the
// test is over, whatever happened.
func cleanupRun(t *testing.T, r Run) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		name := runName(r.ID)
		for _, c := range []string{saveName(r.ID), name} {
			if _, err := docker(ctx, nil, "rm", "--force", c); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
		if exists, err := networkExists(ctx, name); err != nil || !exists {
			return
		}
		// The proxy may be gone, and then RemoveRunNetwork cannot ask it; a
		// network without the label is refused by it.
		if err := RemoveRunNetwork(ctx, r.ID, r.Proxy); err != nil {
			if _, rmErr := docker(ctx, nil, "network", "rm", "--force", name); rmErr != nil {
				t.Errorf("cleanup: %v, then %v", err, rmErr)
			}
		}
	})
}

// tarEntry is one entry of a tar the test makes.
type tarEntry struct {
	hdr  tar.Header
	data string
}

// file, dir and symlink make tar entries as a caller of PlaceRun might give
// them: owned by someone else, with an extended attribute as macOS's tar
// writes it.
func file(name string, mode int64, data string) tarEntry {
	return tarEntry{hdr: foreign(tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(data))}), data: data}
}

func symlink(name, target string) tarEntry {
	return tarEntry{hdr: foreign(tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777})}
}

func foreign(h tar.Header) tar.Header {
	h.Uid, h.Gid, h.Uname, h.Gname = 501, 20, "someone", "staff"
	h.ModTime = time.Now()
	h.Format = tar.FormatPAX
	h.PAXRecords = map[string]string{"SCHILY.xattr.com.apple.provenance": "\x01\x02"}
	return h
}

// tarOf writes entries into a tar.
func tarOf(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, e := range entries {
		if err := tw.WriteHeader(&e.hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

// probeInstruction is the instruction PlaceRun places with the probe.
const probeInstruction = "The probe's instruction.\n"

// runProbe places the probe, its configuration and any extra entries in a
// created run's container with PlaceRun, starts it, waits for it and returns
// the probe's report, its whole output, and what `docker diff` showed before
// the start.
func runProbe(t *testing.T, probe []byte, id string, config probeConfig, extra ...tarEntry) (probeReport, string, string) {
	t.Helper()
	configJSON, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	entries := append([]tarEntry{
		file(strings.TrimPrefix(probePath, "/work/"), 0o755, string(probe)),
		file(strings.TrimPrefix(probeConfigPath, "/work/"), 0o644, string(configJSON)),
	}, extra...)
	if err := PlaceRun(t.Context(), id, bytes.NewReader(tarOf(t, entries...)), []byte(probeInstruction)); err != nil {
		t.Fatal(err)
	}
	diff := must(t, "diff", runName(id))

	out, stderr := startAndWait(t, id)
	var report probeReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the probe's report: %v\n%s\n%s", err, out, stderr)
	}
	return report, out + stderr, diff
}

// startAndWait starts a created run with StartRun, follows it with ReadRun
// until it has exited, for up to five minutes, and returns what it printed to
// standard output and to standard error. It fails the test when the container
// did not exit with 0.
func startAndWait(t *testing.T, id string) (string, string) {
	t.Helper()
	if err := StartRun(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	var stdout, stderr strings.Builder
	if err := ReadRun(ctx, id, true, &stdout, &stderr); err != nil {
		t.Fatalf("ReadRun: %v: %s", err, stderr.String())
	}
	state, err := InspectRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Status != "exited" || state.ExitCode != 0 {
		t.Errorf("%s ended as %+v:\n%s\n%s", runName(id), state, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

// assertCredential asserts that every request of the probe reached the upstream,
// which saw the credential in both headers. The probe reports the headers by
// their SHA-256.
func assertCredential(t *testing.T, report probeReport, credential string) {
	t.Helper()
	hash := func(s string) string { sum := sha256.Sum256([]byte(s)); return "sha256:" + hex.EncodeToString(sum[:]) }
	if len(report.HTTP) == 0 {
		t.Fatal("the probe sent no request")
	}
	for _, r := range report.HTTP {
		var got map[string]string
		if r.Error != "" || json.Unmarshal([]byte(r.Value), &got) != nil {
			t.Errorf("%s: the upstream did not answer: %s%s", r.What, r.Error, r.Value)
			continue
		}
		if got["path"] != "/hello" {
			t.Errorf("%s: the upstream saw the path %q", r.What, got["path"])
		}
		if got["authorization"] != hash("Bearer "+credential) || got["x-api-key"] != hash(credential) {
			t.Errorf("%s: the upstream did not see the credential in both headers", r.What)
		}
	}
}

func resultFor(results []probeResult, what string) *probeResult {
	for i := range results {
		if results[i].What == what {
			return &results[i]
		}
	}
	return nil
}

// fileIn returns a file the probe read, and fails the test when it could not.
func fileIn(t *testing.T, report probeReport, path string) string {
	t.Helper()
	r := resultFor(report.Files, path)
	if r == nil || r.Error != "" {
		t.Fatalf("the probe could not read %s: %v", path, r)
	}
	return r.Value
}

// statusFields reads /proc/self/status into its fields.
func statusFields(t *testing.T, status string) map[string]string {
	t.Helper()
	fields := map[string]string{}
	for _, line := range strings.Split(status, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fields[k] = strings.TrimSpace(v)
		}
	}
	return fields
}

// pseudoFileSystems are the file systems a run may have mounted anywhere: none
// of them is backed by a disk.
var pseudoFileSystems = []string{"proc", "sysfs", "tmpfs", "devpts", "mqueue", "cgroup2"}

// mountProblems judges /proc/self/mountinfo by kind. It allows the root, the
// pseudo file systems, and of the mounts of a host's file only the engine's
// own: /etc/hostname, /etc/hosts, /etc/resolv.conf and the init program.
func mountProblems(mountinfo, init string) []string {
	engines := []string{"/etc/hostname", "/etc/hosts", "/etc/resolv.conf", init}
	var problems []string
	for _, line := range strings.Split(strings.TrimSpace(mountinfo), "\n") {
		f := strings.Fields(line)
		sep := slices.Index(f, "-")
		if len(f) < 5 || sep < 0 || sep+1 >= len(f) {
			problems = append(problems, fmt.Sprintf("cannot read the mount %q", line))
			continue
		}
		point, fsType := unescapeMount(f[4]), f[sep+1]
		switch {
		case point == "/":
		case slices.Contains(pseudoFileSystems, fsType):
		case slices.Contains(engines, point):
		default:
			problems = append(problems, fmt.Sprintf("the mount %s (%s) is not allowed: %s", point, fsType, line))
		}
	}
	return problems
}

// unescapeMount undoes the octal escapes of /proc/self/mountinfo.
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// containerID returns the ID of the container of a name, empty if there is none.
func containerID(t *testing.T, name string) string {
	t.Helper()
	c, err := findContainer(t.Context(), name, runLabel)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		return ""
	}
	return c.id
}

// networkID returns the ID of the network of a name, empty if there is none.
func networkID(t *testing.T, name string) string {
	t.Helper()
	for _, line := range strings.Split(must(t, "network", "ls", "--no-trunc", "--filter", "name="+name, "--format", "{{.ID}}\t{{.Name}}"), "\n") {
		if id, n, _ := strings.Cut(line, "\t"); n == name {
			return id
		}
	}
	return ""
}

// assertLeft asserts what docker ps -a and docker network ls show of a run.
func assertLeft(t *testing.T, id string, container, network bool) {
	t.Helper()
	name := runName(id)
	if got := containerID(t, name) != ""; got != container {
		t.Errorf("a container %s exists: %v, want %v", name, got, container)
	}
	if got := networkID(t, name) != ""; got != network {
		t.Errorf("a network %s exists: %v, want %v", name, got, network)
	}
}
