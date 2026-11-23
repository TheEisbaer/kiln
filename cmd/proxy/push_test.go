package main

import (
	"bytes"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// holding is a credential that holds c.
func holding(c string) *credential {
	cred := &credential{}
	cred.set(c)
	return cred
}

func TestSettings(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := settings(env(nil)); err == nil {
		t.Error("started without an upstream")
	}
	if _, err := settings(env(map[string]string{upstreamVar: "up.example"})); err == nil {
		t.Error("started with an upstream that is not a URL")
	}
	u, err := settings(env(map[string]string{upstreamVar: "https://up.example"}))
	if err != nil || u.Host != "up.example" {
		t.Errorf("got %v, %v", u, err)
	}
}

// pushing is a proxy with a push socket in a directory of its own, in front
// of an upstream that counts its requests and answers with what it saw.
type pushing struct {
	client   *http.Client
	px       *httptest.Server
	socket   string
	cred     *credential
	requests atomic.Int64
	log      lockedBuffer
}

func newPushing(t *testing.T) *pushing {
	t.Helper()
	p := &pushing{cred: &credential{}}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.requests.Add(1)
		echo(w, r)
	}))
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)
	p.px = httptest.NewServer(handler(upURL, p.cred))
	t.Cleanup(p.px.Close)
	pxURL, _ := url.Parse(p.px.URL)
	p.client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pxURL)}}

	// A short path: a Unix socket's path has a limit of about a hundred bytes,
	// and the test's own directories are longer on macOS.
	dir, err := os.MkdirTemp("", "kiln-px-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p.socket = filepath.Join(dir, "push.sock")
	ln, err := listenPush(dir, p.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go servePush(ln, p.cred, log.New(&p.log, "", 0))
	return p
}

// lockedBuffer is the log, written by the pushes' goroutines and read by the
// test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// push runs `proxy push` with stdin and returns its status and what it said.
func (p *pushing) push(t *testing.T, stdin string) (int, string) {
	t.Helper()
	var stderr bytes.Buffer
	code := push(strings.NewReader(stdin), &stderr, p.socket)
	return code, stderr.String()
}

func TestPush(t *testing.T) {
	p := newPushing(t)

	t.Run("without a credential nothing is forwarded", func(t *testing.T) {
		resp, err := p.client.Post("http://proxy.test/v1/messages", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "holds no Claude credential") {
			t.Errorf("got %d %s, want 503 with the proxy's own cause", resp.StatusCode, body)
		}
		if n := p.requests.Load(); n != 0 {
			t.Errorf("the upstream got %d requests", n)
		}
	})

	t.Run("a push", func(t *testing.T) {
		if code, said := p.push(t, "first-credential\n"); code != 0 {
			t.Fatalf("push: %d, %s", code, said)
		}
		got := get(t, p.client, "GET", "http://proxy.test/", nil)
		if got.Authorization != "Bearer first-credential" || got.APIKey != "first-credential" {
			t.Errorf("upstream saw %q and %q", got.Authorization, got.APIKey)
		}
	})

	t.Run("a second push replaces the first", func(t *testing.T) {
		if code, said := p.push(t, "  second-credential  "); code != 0 {
			t.Fatalf("push: %d, %s", code, said)
		}
		got := get(t, p.client, "GET", "http://proxy.test/", nil)
		if got.Authorization != "Bearer second-credential" || got.APIKey != "second-credential" {
			t.Errorf("upstream saw %q and %q", got.Authorization, got.APIKey)
		}
	})

	t.Run("what is refused changes nothing", func(t *testing.T) {
		for what, stdin := range map[string]string{
			"empty":     " \n",
			"a newline": "secret-one\nsecret-two",
			"a control": "secret\x1b[2J",
			"not ASCII": "secret-é",
			"too long":  strings.Repeat("s", maxCredential+1),
			"a space":   "secret one",
			"a delete":  "secret\x7f",
			"a NUL":     "secret\x00",
			"a tab":     "secret\tone",
		} {
			code, said := p.push(t, stdin)
			if code == 0 {
				t.Errorf("%s: pushed", what)
			}
			if strings.Contains(said, "secret") {
				t.Errorf("%s: the answer carries the credential: %s", what, said)
			}
		}
		got := get(t, p.client, "GET", "http://proxy.test/", nil)
		if got.APIKey != "second-credential" {
			t.Errorf("upstream saw %q, want the credential from before", got.APIKey)
		}
		if log := p.log.String(); strings.Contains(log, "secret") {
			t.Errorf("the log carries the credential:\n%s", log)
		}
	})

	// The socket takes nothing from outside the proxy's container: there is
	// no port for the push, a request to the proxy's port that looks like one
	// is a run's request like any other, and the socket is a file of the
	// proxy's user alone, in a directory only it may enter. The seal's test
	// shows on the engine that a run, a container on the proxy's own network
	// namespace and another user in its container do not reach it.
	t.Run("the port takes no push", func(t *testing.T) {
		before := p.requests.Load()
		for _, path := range []string{"/push", "/run/kiln/push.sock", "/credential"} {
			resp, err := p.client.Post("http://proxy.test"+path, "text/plain", strings.NewReader("pushed-by-a-run"))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		}
		if n := p.requests.Load() - before; n != 3 {
			t.Errorf("the upstream got %d of the 3 requests", n)
		}
		got := get(t, p.client, "GET", "http://proxy.test/", nil)
		if got.APIKey != "second-credential" {
			t.Errorf("upstream saw %q, want the credential from before", got.APIKey)
		}
	})

	t.Run("the socket is the proxy's user's alone", func(t *testing.T) {
		info, err := os.Stat(p.socket)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Type() != os.ModeSocket || info.Mode().Perm() != 0o600 {
			t.Errorf("the socket is %v, want a socket of mode 0600", info.Mode())
		}
	})

	if log := p.log.String(); strings.Contains(log, "first-credential") || strings.Contains(log, "second-credential") {
		t.Errorf("the log carries the credential:\n%s", log)
	}
}

// listenPush refuses a directory that others may enter, one that is missing
// and a file, and takes the place of a socket left by an earlier start.
func TestListenPush(t *testing.T) {
	dir, err := os.MkdirTemp("", "kiln-px-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "push.sock")

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if ln, err := listenPush(dir, socket); err == nil {
		ln.Close()
		t.Error("listened in a directory of mode 0755")
	}
	if ln, err := listenPush(filepath.Join(dir, "missing"), socket); err == nil {
		ln.Close()
		t.Error("listened in a missing directory")
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ln, err := listenPush(file, socket); err == nil {
		ln.Close()
		t.Error("listened in a file")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A socket left behind, as after a restart of the proxy in its container.
	old, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close()
	ln, err := listenPush(dir, socket)
	if err != nil {
		t.Fatalf("with a socket left behind: %v", err)
	}
	ln.Close()
}

// A push with no proxy listening fails and says why.
func TestPushWithoutProxy(t *testing.T) {
	defer func(d time.Duration) { dialWait = d }(dialWait)
	dialWait = 200 * time.Millisecond
	var stderr bytes.Buffer
	if code := push(strings.NewReader("c"), &stderr, filepath.Join(t.TempDir(), "none.sock")); code == 0 {
		t.Error("pushed to no proxy")
	}
	if !strings.HasPrefix(stderr.String(), "proxy push: ") {
		t.Errorf("it said %q", stderr.String())
	}
}
