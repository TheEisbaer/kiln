package seal

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The names of the fake web hosts, on the test's own networks. A run cannot
// resolve them; its way out, attached to those networks, can.
const (
	publicHost  = "public.kiln-test.example"
	otherHost   = "other.kiln-test.example"
	privateHost = "private.kiln-test.example"
	mixedHost   = "mixed.kiln-test.example"
)

// connectWant is what the probe should get for one target of the way out.
type connectWant struct {
	target string
	status int
	reason string // in the answer's body
}

// testWayOut runs the probe in runs with a list of hosts and asserts what the
// way out lets through and refuses (spec of card 12, section 2). The test's
// "public" host sits on an IPv6 network in 3ffe::/16, the 6bone's block,
// which no special-purpose registry names, so that the way out's own rule
// calls it public; nothing outside is routed there. The trick is in the
// test's networks, not in the way out. The other cases create, repeat and
// remove runs with a list, and refuse what Kiln did not make.
func testWayOut(t *testing.T, f fixture, credentials []string, published, other string, noConnection []string) {
	marker := "kiln-page-" + strings.TrimSuffix(f.name(""), "-")

	// The test's networks: the "public" one in 3ffe::/16, IPv6 only, with no
	// gateway, and an ordinary private one with IPv6.
	public, private := f.name("public"), f.name("private")
	cleanup(t, "network", "rm", "--force", public)
	cleanup(t, "network", "rm", "--force", private)
	prefix := "3ffe:" + strconv.FormatUint(uint64(randomUint16(t)), 16) + "::/64"
	must(t, "network", "create", "--internal", "--ipv4=false", "--ipv6", "--subnet", prefix,
		"--opt", "com.docker.network.bridge.gateway_mode_ipv6=isolated", public)
	must(t, "network", "create", "--ipv6", private)

	// The fake hosts: one public under two names, one private, and one on
	// both networks. The engine's DNS answers a name from one network only
	// (seen with Docker 29.9.3: mixed.kiln-test.example gets the private
	// network's two addresses), so the probe cannot show a name with a public
	// and a private answer; TestWayOut in cmd/wayout shows that.
	publicName, privateName, mixedName := f.name("public-host"), f.name("private-host"), f.name("mixed-host")
	start(t, publicName, public, f.webImage, "--network-alias", publicHost, "--network-alias", otherHost, "--", marker)
	start(t, privateName, private, f.webImage, "--network-alias", privateHost, "--", marker)
	start(t, mixedName, private, f.webImage, "--network-alias", mixedHost, "--", marker)
	must(t, "network", "connect", "--alias", mixedHost, public, mixedName)
	publicIP := ip6On(t, publicName, public)
	privateIP4, privateIP6 := ipOn(t, privateName, private), ip6On(t, privateName, private)
	if !strings.HasPrefix(publicIP, "3ffe:") {
		t.Fatalf("the public host has the address %s, not one in %s", publicIP, prefix)
	}

	// The probe run with the list *, its way out on the test's networks.
	webID := f.runID("web")
	webRun := runName(webID)
	createRun(t, Run{ID: webID, Image: f.runImage, Proxy: f.proxy.Name, Command: []string{probePath, probeConfigPath},
		Hosts: []string{"*"}, WayOutImage: f.wayOutImage, WayOutNetwork: f.web})
	wayOut := wayOutName(webID)
	must(t, "network", "connect", public, wayOut)
	must(t, "network", "connect", private, wayOut)

	hostPort := lastField(must(t, "port", published, upstreamPort+"/tcp"), ":")
	gateways := []string{gateway4(t, f.web), gateway4(t, "bridge")}
	proxy4, proxy6 := ipOn(t, f.proxy.Name, webRun), ip6On(t, f.proxy.Name, webRun)
	hp := net.JoinHostPort
	wants := []connectWant{
		{hp(publicHost, "443"), 200, marker},
		{hp(otherHost, "443"), 200, marker},
		{hp(publicHost, "8443"), 403, "port 8443 is not 443"},
		{"http://" + publicHost + "/", 403, "only CONNECT"},
		{hp(privateIP4, "443"), 403, privateIP4 + " is not public"},
		{"10.255.255.1:443", 403, "10.255.255.1 is not public"},
		{"127.0.0.1:443", 403, "127.0.0.1 is not public"},
		{"127.255.255.254:443", 403, "127.255.255.254 is not public"},
		{"[::1]:443", 403, "::1 is not public"},
		{"127.0.0.1:3128", 403, "port 3128 is not 443"},
		{"169.254.169.254:443", 403, "169.254.169.254 is not public"},
		{"[fe80::1]:443", 403, "fe80::1 is not public"},
		{"[fe80::1%25eth0]:443", 403, "has a zone"},
		// Go's server answers a zone that is not escaped as %25 itself,
		// before the way out's rules.
		{"[fe80::1%eth0]:443", 400, ""},
		{hp(privateIP6, "443"), 403, privateIP6 + " is not public"},
		{"[::ffff:" + privateIP4 + "]:443", 403, "refused: " + privateIP4 + " is not public"},
		{"[::ffff:7f00:1]:443", 403, "refused: 127.0.0.1 is not public"},
		{"2130706433:443", 403, "written as a number"},
		{"0x7f000001:443", 403, "written as a number"},
		{"0177.0.0.1:443", 403, "written as a number"},
		{"127.1:443", 403, "written as a number"},
		{hp(privateHost, "443"), 403, privateHost + " resolves to "},
		{hp(mixedHost, "443"), 403, mixedHost + " resolves to "},
		{hp(ipOn(t, other, other), "443"), 403, "is not public"},
		{hp(ip6On(t, other, other), "443"), 403, "is not public"},
		{hp(f.proxy.Name, "443"), 403, "is not a name"},
		{hp(f.proxy.Name, "8080"), 403, "port 8080 is not 443"},
		{hp(proxy4, "443"), 403, proxy4 + " is not public"},
		{hp(proxy4, proxyPort), 403, "port " + proxyPort + " is not 443"},
		{hp(proxy6, "443"), 403, proxy6 + " is not public"},
	}
	// The virtual machine's services and a port published on it, at the
	// gateways of the outward network and of the default bridge.
	for _, g := range gateways {
		wants = append(wants,
			connectWant{hp(g, "443"), 403, g + " is not public"},
			connectWant{hp(g, "22"), 403, "port 22 is not 443"},
			connectWant{hp(g, hostPort), 403, "port " + hostPort + " is not 443"})
	}
	// The developer's own, such as the cluster's addresses (section 4 of the
	// spec), which a test cannot know.
	if extra := os.Getenv("KILN_SEAL_EXTRA_REFUSED"); extra != "" {
		for _, target := range strings.Split(extra, ",") {
			wants = append(wants, connectWant{strings.TrimSpace(target), 403, "refused"})
		}
	}
	targets := make([]string, len(wants))
	for i, w := range wants {
		targets[i] = w.target
	}
	report, _, _ := runProbe(t, f.probe, webID, probeConfig{
		HTTP:    []string{"http://" + hp(f.proxy.Name, proxyPort) + "/hello"},
		Connect: targets,
		Dial:    append(slices.Clone(noConnection), hp(publicIP, "443")),
		Lookup:  []string{f.proxy.Name, publicHost, privateHost},
	})

	t.Run("what the way out lets through and refuses", func(t *testing.T) {
		assertConnect(t, report, wants)
	})

	// It reaches the proxy's API, with the credential pushed last; the way out
	// refuses the proxy above, and the push has no address.
	t.Run("the run reaches the proxy's API", func(t *testing.T) {
		assertCredential(t, report, credentials[len(credentials)-1])
	})

	t.Run("the run itself reaches nothing but the proxy and its way out", func(t *testing.T) {
		if len(report.Dial) != len(noConnection)+1 {
			t.Fatalf("the probe tried %d addresses, want %d", len(report.Dial), len(noConnection)+1)
		}
		for _, d := range report.Dial {
			if d.Errno != linuxENETUNREACH && d.Errno != linuxEHOSTUNREACH {
				t.Errorf("%s: want a failure at once as unreachable, got %q (error number %d)", d.What, d.Value+d.Error, d.Errno)
			}
		}
		if len(report.Lookup) != 3 || report.Lookup[0].Error != "" {
			t.Fatalf("the proxy's name does not resolve, so the checks below prove nothing: %+v", report.Lookup)
		}
		for _, l := range report.Lookup[1:] {
			if l.Error == "" {
				t.Errorf("%s resolved in the run to %s", l.What, l.Value)
			}
		}
	})

	t.Run("the run's variables", func(t *testing.T) {
		wayOutURL := "http://" + wayOut + ":" + wayOutPort
		for _, env := range []string{"HTTPS_PROXY=" + wayOutURL, "https_proxy=" + wayOutURL,
			"NO_PROXY=" + f.proxy.Name, "no_proxy=" + f.proxy.Name} {
			if !slices.Contains(report.Env, env) {
				t.Errorf("the environment lacks %s: %q", env, report.Env)
			}
		}
		for _, env := range report.Env {
			if strings.HasPrefix(strings.ToUpper(env), "HTTP_PROXY=") {
				t.Errorf("the environment has %s", env)
			}
		}
		if got := must(t, "inspect", "--format", `{{index .Config.Labels "`+hostsLabel+`"}}`, webRun); got != "*" {
			t.Errorf("the run's label %s is %q, want *", hostsLabel, got)
		}
	})

	t.Run("the way out", func(t *testing.T) {
		got := must(t, "inspect", "--format", `{{index .Config.Labels "`+runLabel+`"}} {{index .Config.Labels "`+wayOutLabel+`"}} {{.HostConfig.ReadonlyRootfs}} {{json .HostConfig.CapDrop}} {{json .HostConfig.SecurityOpt}}`, wayOut)
		if want := webID + ` 1 true ["ALL"] ["no-new-privileges"]`; got != want {
			t.Errorf("the way out has %s, want %s", got, want)
		}
		// Both flags as given, since IPv6 forwarding is off by default in a
		// container's namespace, and reading it there cannot tell.
		var sysctls map[string]string
		if err := json.Unmarshal([]byte(must(t, "inspect", "--format", "{{json .HostConfig.Sysctls}}", wayOut)), &sysctls); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"net.ipv4.ip_forward", "net.ipv6.conf.all.forwarding"} {
			if sysctls[k] != "0" {
				t.Errorf("the way out's sysctls are %v, want %s=0", sysctls, k)
			}
		}
		// Read in the way out's own namespaces, from a container that joins them.
		state := must(t, "run", "--rm", "--network", "container:"+wayOut, "--pid", "container:"+wayOut,
			"--entrypoint", "sh", toolchainImage, "-c",
			"cat /proc/sys/net/ipv4/ip_forward /proc/sys/net/ipv6/conf/all/forwarding; grep -E '^(Cap|NoNewPrivs)' /proc/1/status")
		lines := strings.Split(state, "\n")
		if len(lines) < 2 || lines[0] != "0" || lines[1] != "0" {
			t.Errorf("forwarding in the way out's namespace is %q, want 0 in both families", lines)
		}
		status := statusFields(t, strings.Join(lines[min(2, len(lines)):], "\n"))
		for _, field := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
			if v, err := strconv.ParseUint(status[field], 16, 64); err != nil || v != 0 {
				t.Errorf("the way out's %s is %q, want 0", field, status[field])
			}
		}
		if status["NoNewPrivs"] != "1" {
			t.Errorf("the way out's NoNewPrivs is %q, want 1", status["NoNewPrivs"])
		}
		if got := must(t, "network", "inspect", "--format", `{{index .Labels "`+webLabel+`"}} {{index .Options "com.docker.network.bridge.enable_icc"}}`, f.web); got != "1 false" {
			t.Errorf("the outward network has %s=, enable_icc= %q, want 1 false", webLabel, got)
		}

		var out bytes.Buffer
		if err := ReadWayOut(t.Context(), webID, &out); err != nil {
			t.Fatal(err)
		}
		lines = strings.Split(strings.TrimSpace(out.String()), "\n")
		decisions := map[string]string{}
		for _, line := range lines {
			var e struct{ Target, Decision string }
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Errorf("the way out's output has a line that is not JSON: %q", line)
				continue
			}
			decisions[e.Target] = e.Decision
		}
		if decisions[hp(publicHost, "443")] != "allowed" || decisions[hp(publicHost, "8443")] != "refused" {
			t.Errorf("the way out's output does not record the public host's two ports: %v", decisions)
		}
		if err := ReadWayOut(t.Context(), f.runID("none"), &out); err != ErrNoWayOut {
			t.Errorf("ReadWayOut with no way out: %v, want ErrNoWayOut", err)
		}
	})

	// The credential stays where it is: no request to the web carries it, and
	// the way out does not hold it.
	t.Run("the credential", func(t *testing.T) {
		requests := webRequests(t, publicName)
		if len(requests) == 0 {
			t.Fatal("the public host saw no request")
		}
		for _, r := range requests {
			for k, values := range r.Headers {
				if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "X-Api-Key") {
					t.Errorf("a request to %s carried %s", r.Host, k)
				}
				for _, v := range values {
					for _, c := range credentials {
						if strings.Contains(v, c) {
							t.Errorf("a request to %s carried the credential in %s", r.Host, k)
						}
					}
				}
			}
		}
		inspect := must(t, "inspect", wayOut)
		for _, c := range credentials {
			if strings.Contains(inspect, c) {
				t.Error("the credential is in docker inspect of the way out")
			}
		}
	})

	// A list of one host reaches that host and no other.
	t.Run("a list of one host", func(t *testing.T) {
		id := f.runID("web-one")
		createRun(t, Run{ID: id, Image: f.runImage, Proxy: f.proxy.Name, Command: []string{probePath, probeConfigPath},
			Hosts: []string{publicHost}, WayOutImage: f.wayOutImage, WayOutNetwork: f.web})
		must(t, "network", "connect", public, wayOutName(id))
		wants := []connectWant{
			{hp(publicHost, "443"), 200, marker},
			{hp(otherHost, "443"), 403, otherHost + " is not on the list"},
			{hp(publicIP, "443"), 403, "only the list * allows"},
		}
		report, _, _ := runProbe(t, f.probe, id, probeConfig{Connect: []string{wants[0].target, wants[1].target, wants[2].target}})
		assertConnect(t, report, wants)
		if got := must(t, "inspect", "--format", `{{index .Config.Labels "`+hostsLabel+`"}}`, runName(id)); got != publicHost {
			t.Errorf("the run's label %s is %q, want %s", hostsLabel, got, publicHost)
		}
	})

	// Runs of their own, side by side with the rest of TestRun's later cases.
	t.Run("CreateRun with a list", func(t *testing.T) {
		t.Parallel()
		testCreateWithList(t, f)
	})
	t.Run("RemoveRun with a way out", func(t *testing.T) {
		t.Parallel()
		testRemoveWithWayOut(t, f)
	})
}

// assertConnect asserts the probe's answer for every target.
func assertConnect(t *testing.T, report probeReport, wants []connectWant) {
	t.Helper()
	if len(report.Connect) != len(wants) {
		t.Fatalf("the probe sent %d targets, want %d: %+v", len(report.Connect), len(wants), report.Connect)
	}
	for i, w := range wants {
		got := report.Connect[i]
		t.Logf("%s: %s%s", w.target, got.Value, got.Error)
		status, body, _ := strings.Cut(got.Value, " ")
		if got.Error != "" || status != strconv.Itoa(w.status) || !strings.Contains(body, w.reason) {
			t.Errorf("%s: got %q%s, want %d with %q", w.target, got.Value, got.Error, w.status, w.reason)
		}
	}
}

// webRequest is a request as the fake web host writes it.
type webRequest struct {
	Kind, Host, Path string
	Headers          map[string][]string
}

// webRequests reads the requests a fake web host saw.
func webRequests(t *testing.T, container string) []webRequest {
	t.Helper()
	var requests []webRequest
	for _, line := range strings.Split(must(t, "logs", container), "\n") {
		var r webRequest
		if json.Unmarshal([]byte(line), &r) == nil && r.Kind == "request" {
			requests = append(requests, r)
		}
	}
	return requests
}

// withList is a run with the list * and the test's way out, which never
// starts.
func withList(f fixture, id string) Run {
	return Run{ID: id, Image: f.runImage, Proxy: f.proxy.Name, Command: []string{"true"},
		Hosts: []string{"*"}, WayOutImage: f.wayOutImage, WayOutNetwork: f.web}
}

// testCreateWithList repeats and fails CreateRun with a list, and refuses
// what Kiln did not make.
func testCreateWithList(t *testing.T, f fixture) {
	t.Run("a bad list", func(t *testing.T) {
		for _, hosts := range [][]string{{"*", "go.dev"}, {"Go.dev"}, {"go.dev."}, {"127.0.0.1"}, {"localhost"}, {"*.go.dev"}, {""}} {
			r := withList(f, f.runID("bad-list"))
			r.Hosts = hosts
			cleanupRun(t, r)
			if err := CreateRun(t.Context(), r); err == nil || !strings.Contains(err.Error(), "list of hosts") {
				t.Errorf("%q: want the list refused, got %v", hosts, err)
			}
			assertLeft(t, r.ID, false, false)
			assertNoWayOut(t, r.ID)
		}
	})

	t.Run("after a container that never started", func(t *testing.T) {
		r := withList(f, f.runID("list-again"))
		createRun(t, r)
		container, wayOut := containerID(t, runName(r.ID)), containerID(t, wayOutName(r.ID))
		if err := CreateRun(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		assertLeft(t, r.ID, true, true)
		if got := containerID(t, runName(r.ID)); got == container {
			t.Errorf("the container %s left behind was kept", container)
		}
		if got := containerID(t, wayOutName(r.ID)); got == wayOut || got == "" {
			t.Errorf("the way out is %q, was %s: not replaced", got, wayOut)
		}
		if got := must(t, "inspect", "--format", "{{.State.Status}}", wayOutName(r.ID)); got != "running" {
			t.Errorf("the way out is %s, want running", got)
		}
	})

	t.Run("after a way out left alone", func(t *testing.T) {
		r := withList(f, f.runID("list-left"))
		createRun(t, r)
		wayOut := containerID(t, wayOutName(r.ID))
		must(t, "rm", "--force", runName(r.ID))
		if err := CreateRun(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		if got := containerID(t, wayOutName(r.ID)); got == wayOut || got == "" {
			t.Errorf("the way out is %q, was %s: not replaced", got, wayOut)
		}
		assertLeft(t, r.ID, true, true)
	})

	t.Run("after a way out without kiln.run", func(t *testing.T) {
		r := withList(f, f.runID("list-foreign"))
		cleanupRun(t, r)
		must(t, "create", "--name", wayOutName(r.ID), "--network", "none", f.upstreamImage)
		before := must(t, "inspect", "--format", "{{.Id}} {{.State.Status}}", wayOutName(r.ID))
		if err := CreateRun(t.Context(), r); err == nil || !strings.Contains(err.Error(), "no label "+runLabel) {
			t.Errorf("CreateRun: want a refusal for the label, got %v", err)
		}
		if err := RemoveRun(t.Context(), r.ID, f.proxy.Name); err == nil {
			t.Error("RemoveRun: no error")
		}
		if err := ReadWayOut(t.Context(), r.ID, &bytes.Buffer{}); err == nil || err == ErrNoWayOut {
			t.Errorf("ReadWayOut: want a refusal, got %v", err)
		}
		if got := must(t, "inspect", "--format", "{{.Id}} {{.State.Status}}", wayOutName(r.ID)); got != before {
			t.Errorf("the way out that lacks kiln.run was touched: %s, was %s", got, before)
		}
		assertLeft(t, r.ID, false, false)
	})

	t.Run("with an outward network without kiln.web", func(t *testing.T) {
		foreign := f.name("foreign-web")
		must(t, "network", "create", foreign)
		cleanup(t, "network", "rm", "--force", foreign)
		before := networkID(t, foreign)
		r := withList(f, f.runID("list-foreign-web"))
		r.WayOutNetwork = foreign
		cleanupRun(t, r)
		if err := CreateRun(t.Context(), r); err == nil || !strings.Contains(err.Error(), webLabel) {
			t.Errorf("want a refusal for the label %s, got %v", webLabel, err)
		}
		assertLeft(t, r.ID, false, false)
		assertNoWayOut(t, r.ID)
		if got := networkID(t, foreign); got != before {
			t.Errorf("the network is %q, was %s", got, before)
		}
		if got := must(t, "network", "inspect", "--format", "{{json .Containers}}", foreign); got != "{}" {
			t.Errorf("something was put on the network: %s", got)
		}
	})

	// Runs that start at once each make the outward network if it is
	// missing; every one of them goes on with the one network (card 17).
	t.Run("the outward network made by several runs at once", func(t *testing.T) {
		name := f.name("web-at-once")
		cleanup(t, "network", "rm", "--force", name)
		errs := make(chan error, 4)
		for range 4 {
			go func() { errs <- webNetwork(t.Context(), name) }()
		}
		for range 4 {
			if err := <-errs; err != nil {
				t.Error(err)
			}
		}
		if got := must(t, "network", "ls", "--filter", "label="+webLabel, "--format", "{{.Name}}"); !slices.Contains(strings.Fields(got), name) {
			t.Errorf("the networks with %s are %q, want %s among them", webLabel, got, name)
		}
	})

	t.Run("with a way out image that does not exist", func(t *testing.T) {
		r := withList(f, f.runID("list-no-image"))
		r.WayOutImage = f.name("no-such-image")
		cleanupRun(t, r)
		if err := CreateRun(t.Context(), r); err == nil || !strings.Contains(err.Error(), "No such image") {
			t.Errorf("want the image missing locally, got %v", err)
		}
		assertLeft(t, r.ID, false, false)
		assertNoWayOut(t, r.ID)
	})

	t.Run("with a run image that does not exist", func(t *testing.T) {
		r := withList(f, f.runID("list-no-run-image"))
		r.Image = f.name("no-such-image")
		cleanupRun(t, r)
		if err := CreateRun(t.Context(), r); err == nil || !strings.Contains(err.Error(), "No such image") {
			t.Errorf("want the image missing locally, got %v", err)
		}
		assertLeft(t, r.ID, false, false)
		assertNoWayOut(t, r.ID)
	})
}

// testRemoveWithWayOut removes runs with a way out, twice and after parts were
// removed by hand, and lists a way out that was left alone.
func testRemoveWithWayOut(t *testing.T, f fixture) {
	removed := func(t *testing.T, id string) {
		t.Helper()
		for i := range 2 {
			if err := RemoveRun(t.Context(), id, f.proxy.Name); err != nil {
				t.Errorf("RemoveRun, call %d: %v", i+1, err)
			}
			assertLeft(t, id, false, false)
			assertNoWayOut(t, id)
		}
	}
	t.Run("twice", func(t *testing.T) {
		r := withList(f, f.runID("rm-list"))
		createRun(t, r)
		removed(t, r.ID)
	})
	t.Run("after the way out was removed by hand", func(t *testing.T) {
		r := withList(f, f.runID("rm-list-wayout"))
		createRun(t, r)
		must(t, "rm", "--force", wayOutName(r.ID))
		removed(t, r.ID)
	})
	t.Run("when only the way out was left", func(t *testing.T) {
		r := withList(f, f.runID("rm-list-only"))
		createRun(t, r)
		must(t, "rm", "--force", runName(r.ID))
		removed(t, r.ID)
	})
	t.Run("ListRuns lists a way out left alone", func(t *testing.T) {
		instance := randomID(t)
		r := withList(f, f.runID("ls-list"))
		r.Instance = instance
		createRun(t, r)
		must(t, "rm", "--force", runName(r.ID))
		must(t, "network", "disconnect", "--force", runName(r.ID), wayOutName(r.ID))
		if err := RemoveRunNetwork(t.Context(), r.ID, f.proxy.Name); err != nil {
			t.Fatal(err)
		}
		ids, err := ListRuns(t.Context(), instance)
		if err != nil || !slices.Equal(ids, []string{r.ID}) {
			t.Errorf("ListRuns: %q, %v; want %s", ids, err, r.ID)
		}
		removed(t, r.ID)
		if ids, err := ListRuns(t.Context(), instance); err != nil || len(ids) != 0 {
			t.Errorf("ListRuns after the removal: %q, %v", ids, err)
		}
	})
}

// assertNoWayOut asserts that a run has no way out.
func assertNoWayOut(t *testing.T, id string) {
	t.Helper()
	if got := containerID(t, wayOutName(id)); got != "" {
		t.Errorf("the way out %s is left: %s", wayOutName(id), got)
	}
}

// randomUint16 is a random number for a test network's prefix.
func randomUint16(t *testing.T) uint16 {
	t.Helper()
	v, err := strconv.ParseUint(randomID(t)[:4], 16, 16)
	if err != nil {
		t.Fatal(err)
	}
	return uint16(v)
}
