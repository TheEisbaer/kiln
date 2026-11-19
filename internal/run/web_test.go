package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fake web hosts' names, on the test's own networks (the seal's test has
// the same).
const (
	publicHost  = "public.kiln-test.example"
	privateHost = "private.kiln-test.example"
)

// web is what the cases of a run with a list share.
type web struct {
	*fixture
	image           string // the run image with the fake hosts' authority
	wayOutImage     string
	outward         string // the outward network of the test's ways out
	public, private string // the test's networks, which the cases connect to a way out
	publicHost      string // the public host's container
	marker          string // its page's marker
	repo, commit    string // a repo whose run.sh reads the web
}

// testWeb carries out runs with the list * (ADR 0059): one of the real Claude
// Code, which reads the fake public host's page with curl and with WebFetch,
// and runs in the store whose way out was stopped or removed.
//
// The public host is on an IPv6 network in 3ffe::/16, which the way out's own
// rule calls public (see the seal's test); the case connects it to the run's
// way out after CreateRun.
func testWeb(t *testing.T, f *fixture) {
	w := &web{fixture: f, marker: "kiln-page-" + f.id}
	name := func(what string) string { return "kiln-steptest-" + f.id + "-" + what }
	w.wayOutImage, w.outward = name("wayout"), name("outward")
	webImage := name("web")
	build(t, w.wayOutImage, "images/wayout/Dockerfile")
	build(t, webImage, "internal/seal/testdata/web/Dockerfile")
	t.Cleanup(func() { dockerQuiet("network", "rm", w.outward) })

	// The test's networks and the fake hosts.
	w.public, w.private = name("public"), name("private")
	t.Cleanup(func() { dockerQuiet("network", "rm", w.public, w.private) })
	must(t, "network", "create", "--internal", "--ipv4=false", "--ipv6", "--subnet", "3ffe:"+f.id[:4]+"::/64",
		"--opt", "com.docker.network.bridge.gateway_mode_ipv6=isolated", w.public)
	must(t, "network", "create", "--ipv6", w.private)
	w.publicHost = name("public-host")
	privateName := name("private-host")
	t.Cleanup(func() { dockerQuiet("rm", "--force", w.publicHost, privateName) })
	must(t, "run", "--detach", "--name", w.publicHost, "--network", w.public, "--network-alias", publicHost, webImage, w.marker)
	must(t, "run", "--detach", "--name", privateName, "--network", w.private, "--network-alias", privateHost, webImage, w.marker)

	// The run image with the public host's authority, which it writes first.
	var ca string
	deadline := time.Now().Add(time.Minute)
	for ca == "" {
		line, _, _ := strings.Cut(must(t, "logs", w.publicHost), "\n")
		var first struct{ Kind, PEM string }
		if json.Unmarshal([]byte(line), &first) == nil && first.Kind == "ca" {
			ca = first.PEM
		} else if time.Now().After(deadline) {
			t.Fatal("the public host wrote no authority")
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	dir := t.TempDir()
	writeIn(t, dir, "ca.crt", ca, 0o644)
	w.image = name("web-run")
	t.Cleanup(func() { dockerQuiet("rmi", w.image) })
	must(t, "build", "--quiet", "--tag", w.image, "--build-arg", "RUN="+f.runImage, "--file", "testdata/web/Dockerfile", dir)

	// A repo whose run.sh waits at its gate, so that the case can connect the
	// test's networks to the way out, then reads the page and follows a
	// redirect to the private host. The repo switches WebFetch's check with
	// Anthropic off, which a test cannot reach; a repo's own settings can.
	w.repo, w.commit = bareRepo(t, map[string]string{
		"CLAUDE.md":             "# Try\n\nThe marker is " + f.marker + ".\n",
		".claude/settings.json": `{"skipWebFetchPreflight": true}` + "\n",
		"run.sh": "set -eu\n" +
			"touch /tmp/waiting\n" +
			"while [ ! -e /tmp/gate ]; do sleep 0.1; done\n" +
			"curl -fsS https://" + publicHost + "/page\n" +
			"if curl -fsS -L https://" + publicHost + "/redirect; then echo the redirect was followed; else echo the redirect was refused; fi\n",
	})

	t.Run("curl and WebFetch read a public page", func(t *testing.T) { w.testRead(t) })
	t.Run("in the store", func(t *testing.T) { w.testInStore(t) })
}

// newWebCase is newCase with the web repo, the web run image and the list *.
func (w *web) newWebCase(t *testing.T, name string) testCase {
	t.Helper()
	c := w.newCase(t, name)
	c.in.Repo, c.in.Commit, c.in.Image = w.repo, w.commit, w.image
	c.in.Hosts, c.in.WayOutImage, c.in.WayOutNetwork = []string{"*"}, w.wayOutImage, w.outward
	return c
}

// connectWayOut connects the test's networks to a run's way out.
func (w *web) connectWayOut(t *testing.T, id string) {
	t.Helper()
	must(t, "network", "connect", w.public, "kiln-wayout-"+id)
	must(t, "network", "connect", w.private, "kiln-wayout-"+id)
}

func (w *web) testRead(t *testing.T) {
	c := w.newWebCase(t, "web")
	cmd, done := w.start(t, c, "", false)
	w.waitAtGate(t, c)
	w.connectWayOut(t, c.id)
	must(t, "exec", "kiln-run-"+c.id, "touch", "/tmp/gate")
	if code := <-done; code != 0 {
		t.Fatalf("exit status %d\n%s\n%s", code, cmd.Stdout, cmd.Stderr)
	}
	dir := filepath.Join(w.runs, c.id)

	// The page reached the run, by curl in Bash's result and by WebFetch.
	results := toolResultsIn(t, filepath.Join(dir, "output.jsonl"))
	for id, what := range map[string]string{"toolu_kiln_1": "curl's output", "toolu_kiln_2": "WebFetch's result"} {
		if !strings.Contains(results[id], w.marker) {
			t.Errorf("%s does not hold the page's marker: %q", what, results[id])
		}
	}
	if !strings.Contains(results["toolu_kiln_1"], "the redirect was refused") {
		t.Errorf("curl followed the redirect to the private host: %q", results["toolu_kiln_1"])
	}

	// The way out let through the public host and refused the redirect's
	// target; the model traffic did not take the web's path.
	lines := wayOutLines(t, filepath.Join(dir, "wayout.jsonl"))
	refused := false
	for _, e := range lines {
		t.Logf("wayout.jsonl: %s %s %d %s", e.Target, e.Decision, e.Status, e.Reason)
		switch {
		case e.Target == publicHost+":443" && e.Decision == "allowed":
		case e.Target == privateHost+":443" && e.Status == 403:
			refused = true
		default:
			t.Errorf("wayout.jsonl has %+v", e)
		}
	}
	if len(lines) < 3 || !refused {
		t.Errorf("wayout.jsonl holds %d lines, the redirect's target refused: %v; want curl's two, WebFetch's and the refusal", len(lines), refused)
	}

	var rec record
	if err := json.Unmarshal([]byte(readAll(t, filepath.Join(dir, "result.json"))), &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Hosts) != 1 || rec.Hosts[0] != "*" || rec.WayOut != "" || rec.Result == nil || rec.Result.Subtype != "success" {
		t.Errorf("the record is %+v", rec)
	}
	if _, err := docker(t.Context(), "container", "inspect", "kiln-wayout-"+c.id); err == nil {
		t.Error("the way out is left after the run was removed")
	}

	// Every model request, WebFetch's own call among them, reached the
	// scripted upstream through the proxy with the credential.
	var answers []string
	for _, r := range w.upstreamRequests(t, c) {
		answers = append(answers, r["answer"].(string))
		if r["x_api_key"] != w.credential {
			t.Errorf("the request for %s reached the upstream without the credential", r["answer"])
		}
	}
	t.Logf("the upstream answered %q", answers)
	if strings.Join(answers, " ") != "bash webfetch page done" {
		t.Errorf("the upstream answered %q, want bash, webfetch, page and done", answers)
	}

	// No request to the web carried the credential.
	requests := 0
	for _, line := range strings.Split(must(t, "logs", w.publicHost), "\n") {
		var r struct {
			Kind    string
			Headers map[string][]string
		}
		if json.Unmarshal([]byte(line), &r) != nil || r.Kind != "request" {
			continue
		}
		requests++
		for k, values := range r.Headers {
			if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "X-Api-Key") {
				t.Errorf("a request to the public host carried %s", k)
			}
			for _, v := range values {
				if strings.Contains(v, w.credential) {
					t.Errorf("a request to the public host carried the credential in %s", k)
				}
			}
		}
	}
	if requests < 3 {
		t.Errorf("the public host saw %d requests, want curl's two and WebFetch's", requests)
	}
}

// testInStore carries runs in the store, with Carry and starting nil, after
// their way out was stopped or removed. Nothing of a run is removed or created
// again: its container, network and way out keep their IDs.
func (w *web) testInStore(t *testing.T) {
	// openGate opens a run's gate whenever it waits there, until the test
	// ends, once its way out runs or 30 seconds have passed: a run that waits
	// already must not go on before Carry has started its way out.
	openGate := func(t *testing.T, id string) {
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go func() {
			for deadline := time.Now().Add(30 * time.Second); ctx.Err() == nil && time.Now().Before(deadline); {
				if got, _ := docker(ctx, "inspect", "--format", "{{.State.Status}}", "kiln-wayout-"+id); got == "running" {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			for ctx.Err() == nil {
				if _, err := docker(ctx, "exec", "kiln-run-"+id, "test", "-e", "/tmp/waiting"); err == nil {
					docker(ctx, "exec", "kiln-run-"+id, "touch", "/tmp/gate")
				}
				time.Sleep(200 * time.Millisecond)
			}
		}()
	}
	ids := func(t *testing.T, id string) string {
		t.Helper()
		var parts []string
		for _, what := range [][]string{{"container", "kiln-run-" + id}, {"network", "kiln-run-" + id}, {"container", "kiln-wayout-" + id}} {
			got, err := docker(t.Context(), what[0], "inspect", "--format", "{{.Id}}", what[1])
			if err != nil {
				got = "none"
			}
			parts = append(parts, got)
		}
		return strings.Join(parts, " ")
	}
	killedAt := func(t *testing.T, c testCase, step string) {
		t.Helper()
		if code, out, errOut := w.kiln(t, c, step+":exit", true); code != exitAfterStep {
			t.Fatalf("exit status %d, want %d\n%s\n%s", code, exitAfterStep, out, errOut)
		}
	}
	reached := func(t *testing.T, c testCase) {
		t.Helper()
		results := toolResultsIn(t, filepath.Join(w.runs, c.id, "output.jsonl"))
		if !strings.Contains(results["toolu_kiln_1"], w.marker) {
			t.Errorf("the run did not reach the public host: %q", results["toolu_kiln_1"])
		}
	}

	t.Run("never started, its way out stopped", func(t *testing.T) {
		c := w.newWebCase(t, "store-stopped")
		killedAt(t, c, "place")
		w.connectWayOut(t, c.id)
		must(t, "stop", "kiln-wayout-"+c.id)
		before := ids(t, c.id)
		openGate(t, c.id)
		v, err := Carry(t.Context(), c.in, nil, &bytes.Buffer{})
		if err != nil || !v.Finished {
			t.Fatalf("Carry: %+v, %v", v, err)
		}
		if got := ids(t, c.id); got != before {
			t.Errorf("the IDs are %s, were %s", got, before)
		}
		reached(t, c)
	})

	t.Run("never started, its way out removed by hand", func(t *testing.T) {
		c := w.newWebCase(t, "store-gone")
		killedAt(t, c, "place")
		must(t, "rm", "--force", "kiln-wayout-"+c.id)
		before := ids(t, c.id)
		// Bounded: a run that started by mistake would wait at its gate.
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		_, err := Carry(ctx, c.in, nil, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "is gone; the run is left alone") {
			t.Errorf("Carry: %v, want that the way out is gone", err)
		}
		if got := must(t, "inspect", "--format", "{{.State.Status}}", "kiln-run-"+c.id); got != "created" {
			t.Errorf("the run's container is %s, want created", got)
		}
		if got := ids(t, c.id); got != before {
			t.Errorf("the IDs are %s, were %s", got, before)
		}
	})

	t.Run("running, its way out stopped", func(t *testing.T) {
		c := w.newWebCase(t, "store-running")
		cmd, done := w.start(t, c, "", false)
		w.waitAtGate(t, c)
		w.connectWayOut(t, c.id)
		cmd.Process.Kill()
		<-done
		must(t, "stop", "kiln-wayout-"+c.id)
		before := ids(t, c.id)
		openGate(t, c.id)
		var notes bytes.Buffer
		v, err := Carry(t.Context(), c.in, nil, &notes)
		if err != nil || !v.Finished {
			t.Fatalf("Carry: %+v, %v\n%s", v, err, notes.String())
		}
		if got := ids(t, c.id); got != before {
			t.Errorf("the IDs are %s, were %s", got, before)
		}
		if got := must(t, "inspect", "--format", "{{.State.Status}}", "kiln-wayout-"+c.id); got != "running" {
			t.Errorf("the way out is %s, want running", got)
		}
		reached(t, c)
	})

	t.Run("exited, its way out gone", func(t *testing.T) {
		c := w.newWebCase(t, "store-exited")
		killedAt(t, c, "follow")
		must(t, "rm", "--force", "kiln-wayout-"+c.id)
		if _, err := Carry(t.Context(), c.in, nil, &bytes.Buffer{}); err != nil {
			t.Fatalf("Carry: %v", err)
		}
		dir := filepath.Join(w.runs, c.id)
		var rec record
		if err := json.Unmarshal([]byte(readAll(t, filepath.Join(dir, "result.json"))), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.WayOut != "gone" || len(rec.Hosts) != 1 || rec.Hosts[0] != "*" {
			t.Errorf("the record has the way out %q and the hosts %q, want gone and *", rec.WayOut, rec.Hosts)
		}
		if _, err := os.Stat(filepath.Join(dir, "wayout.jsonl")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("wayout.jsonl: %v, want none", err)
		}
		if err := Remove(t.Context(), c.in); err != nil {
			t.Errorf("Remove: %v", err)
		}
		assertLeft(t, c.id, false, false)
	})
}

// upstreamRequests returns the scripted upstream's requests of a case.
func (w *web) upstreamRequests(t *testing.T, c testCase) []map[string]any {
	t.Helper()
	var requests []map[string]any
	for _, line := range strings.Split(must(t, "logs", w.upstream), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["kind"] == "request" && r["case"] == "kiln-case-"+c.name {
			requests = append(requests, r)
		}
	}
	return requests
}

// toolResultsIn reads the text of each tool result in output.jsonl, by the
// tool use's ID.
func toolResultsIn(t *testing.T, output string) map[string]string {
	t.Helper()
	results := map[string]string{}
	for _, line := range strings.Split(readAll(t, output), "\n") {
		var e struct {
			Type    string
			Message struct {
				Content []struct {
					Type      string          `json:"type"`
					ToolUseID string          `json:"tool_use_id"`
					Content   json.RawMessage `json:"content"`
				}
			}
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.Type != "user" {
			continue
		}
		for _, b := range e.Message.Content {
			if b.Type != "tool_result" {
				continue
			}
			var text string
			if json.Unmarshal(b.Content, &text) != nil {
				var parts []struct{ Text string }
				json.Unmarshal(b.Content, &parts)
				for _, p := range parts {
					text += p.Text
				}
			}
			results[b.ToolUseID] += text
		}
	}
	return results
}

// wayOutEntry is a line of wayout.jsonl.
type wayOutEntry struct {
	Target, Decision, Reason string
	Status                   int
}

func wayOutLines(t *testing.T, name string) []wayOutEntry {
	t.Helper()
	var lines []wayOutEntry
	for i, line := range strings.Split(strings.TrimSpace(readAll(t, name)), "\n") {
		var e wayOutEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("wayout.jsonl, line %s: %v: %q", strconv.Itoa(i+1), err, line)
			continue
		}
		lines = append(lines, e)
	}
	return lines
}
