// Command proxy is the only way a run reaches Claude (ADR 0017, ADR 0034). It
// forwards every plain-HTTP request from runs to one upstream, whatever host the
// request names, and puts the credential on it.
//
// It starts with no credential and takes one while it runs, as often as the
// core pushes one (ADR 0040): `proxy push`, which the seal runs inside the
// proxy's container with `docker exec`, reads the credential from its standard
// input and hands it to the running proxy over a Unix socket, in a directory
// that only the proxy's user in its own container can enter. There is no port
// for the push. Until the first push, a run's request is answered with an
// error of the proxy's own and forwarded nowhere.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// upstreamVar is the variable the proxy reads its upstream from. internal/seal
// passes the same name; a mismatch shows as a proxy that does not start.
const upstreamVar = "KILN_PROXY_UPSTREAM"

// listen is the address the proxy serves on, inside its container, in both IPv4
// and IPv6.
const listen = ":8080"

// pushDir is the directory of the push socket: a tmpfs that the seal mounts,
// owned by the proxy's user with the mode 0700, so that only that user in the
// proxy's own container enters it (internal/seal). pushSocket is the socket.
const (
	pushDir    = "/run/kiln"
	pushSocket = pushDir + "/push.sock"
)

// maxCredential is the longest credential the proxy takes, in bytes.
const maxCredential = 64 << 10

// pushLimit bounds one push on the socket, from the connection to the answer.
const pushLimit = 10 * time.Second

func main() {
	switch {
	case len(os.Args) == 2 && os.Args[1] == "push":
		os.Exit(push(os.Stdin, os.Stderr, pushSocket))
	case len(os.Args) != 1:
		fmt.Fprintln(os.Stderr, "usage: proxy, or proxy push with the credential on standard input")
		os.Exit(2)
	}
	upstream, err := settings(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "proxy:", err)
		os.Exit(1)
	}
	cred := &credential{}
	ln, err := listenPush(pushDir, pushSocket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "proxy:", err)
		os.Exit(1)
	}
	go servePush(ln, cred, log.Default())
	srv := &http.Server{
		Addr:              listen,
		Handler:           handler(upstream, cred),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// settings reads the upstream. Without it the proxy does not start.
func settings(getenv func(string) string) (*url.URL, error) {
	raw := getenv(upstreamVar)
	if raw == "" {
		return nil, errors.New(upstreamVar + " is not set")
	}
	upstream, err := url.Parse(raw)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" {
		return nil, fmt.Errorf("%s is not an http or https URL", upstreamVar)
	}
	return upstream, nil
}

// credential is the credential the proxy holds, empty until the first push.
// A push replaces it whole.
type credential struct{ v atomic.Pointer[string] }

func (c *credential) get() string {
	if p := c.v.Load(); p != nil {
		return *p
	}
	return ""
}

func (c *credential) set(s string) { c.v.Store(&s) }

// noCredential is the proxy's own answer to a run's request while it holds no
// credential, with the status 503. It names the cause.
const noCredential = `{"type":"error","error":{"type":"api_error","message":"Kiln's proxy holds no Claude credential: the core has not given it one"}}`

// credentialKey is the key of a request's credential in its context.
type credentialKey struct{}

// handler forwards every request to upstream, whatever host it names, with
// the credential in both headers Claude Code sends a key helper's output in.
// It refuses CONNECT, streams responses as they arrive, and forwards nothing
// while it holds no credential.
func handler(upstream *url.URL, cred *credential) http.Handler {
	forward := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			credential, _ := r.In.Context().Value(credentialKey{}).(string)
			r.Out.Header.Set("Authorization", "Bearer "+credential)
			r.Out.Header.Set("X-Api-Key", credential)
		},
		FlushInterval: -1,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			w.Header().Set("Allow", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
			http.Error(w, "CONNECT is not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Read once per request, so that a push in between does not put two
		// credentials on it.
		credential := cred.get()
		if credential == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, noCredential)
			return
		}
		// Go's HTTP/1 server closes a request's body at the answer's first write,
		// while the transport may still read from it, and the transport then cuts
		// off the answer (Go's issue 40747). The server's own writer always
		// supports this, and TestDoesNotCutOffAnAnswerItHasBegunToPassOn fails
		// without it.
		http.NewResponseController(w).EnableFullDuplex()
		forward.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), credentialKey{}, credential)))
	})
}

// listenPush listens on the push socket. Its directory must be there, and be
// a directory of the proxy's own user that no one else may enter, as the seal
// mounts it; otherwise the proxy does not start. A socket left by an earlier
// start is removed first.
func listenPush(dir, socket string) (net.Listener, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("the push socket's directory: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != os.Getuid() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("the push socket's directory %s is not a directory of uid %d that only it may enter", dir, os.Getuid())
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// servePush takes pushes on the socket until it is closed. Each connection
// writes one credential and closes its side, and is answered "ok", or
// "refused: " with the reason. Neither the answer nor the log carries the
// credential.
func servePush(ln net.Listener, cred *credential, logger *log.Logger) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			logger.Printf("proxy: the push socket: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(pushLimit))
			data, err := io.ReadAll(io.LimitReader(conn, maxCredential+1))
			if err != nil {
				io.WriteString(conn, "refused: the push was not read whole\n")
				return
			}
			c, why := judge(data)
			if why != "" {
				logger.Printf("proxy: a push was refused: %s", why)
				io.WriteString(conn, "refused: "+why+"\n")
				return
			}
			cred.set(c)
			logger.Print("proxy: a credential was pushed")
			io.WriteString(conn, "ok\n")
		}()
	}
}

// judge is the credential of a push, with the white space around it trimmed,
// or why it is refused: it is empty, longer than maxCredential, or has a byte
// that is not printable ASCII, which a header cannot carry.
func judge(data []byte) (string, string) {
	if len(data) > maxCredential {
		return "", fmt.Sprintf("the credential is longer than %d bytes", maxCredential)
	}
	c := string(bytes.TrimSpace(data))
	if c == "" {
		return "", "the credential is empty"
	}
	for i := 0; i < len(c); i++ {
		if b := c[i]; b < 0x21 || b > 0x7e {
			return "", "the credential has a byte that is not printable ASCII"
		}
	}
	return c, ""
}

// push is `proxy push`: it reads the credential from stdin, hands it to the
// running proxy over the socket, and returns the exit status. It prints only
// a failure, which never carries the credential.
func push(stdin io.Reader, stderr io.Writer, socket string) int {
	fail := func(err any) int {
		fmt.Fprintln(stderr, "proxy push:", err)
		return 1
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxCredential+1))
	if err != nil {
		return fail(fmt.Errorf("reading standard input: %w", err))
	}
	conn, err := dial(socket)
	if err != nil {
		return fail(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(pushLimit))
	if _, err := conn.Write(data); err != nil {
		return fail(err)
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		return fail(err)
	}
	answer, err := io.ReadAll(io.LimitReader(conn, 1<<10))
	if err != nil {
		return fail(err)
	}
	switch a := strings.TrimSpace(string(answer)); a {
	case "ok":
		return 0
	case "":
		return fail("the proxy gave no answer")
	default:
		return fail(a)
	}
}

// dialWait is how long a push waits for a proxy that has only just started
// and has no socket yet. Only the test changes it.
var dialWait = 5 * time.Second

// dial connects to the push socket, trying again for up to dialWait while
// the socket is not there or no one listens on it yet.
func dial(socket string) (net.Conn, error) {
	deadline := time.Now().Add(dialWait)
	for {
		conn, err := net.DialTimeout("unix", socket, pushLimit)
		if err == nil || time.Now().After(deadline) ||
			!(errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)) {
			return conn, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}
