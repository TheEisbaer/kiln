package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// seen is what the fake upstream reports about the request it received.
type seen struct {
	Method, Host, Path, Authorization, APIKey string
}

// start runs a proxy in front of an upstream that answers with what it saw, and
// returns a client that uses the proxy as its HTTP proxy, as a run does.
func start(t *testing.T, upstreamHandler http.Handler) (*http.Client, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstreamHandler)
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)
	px := httptest.NewServer(handler(upURL, "the-credential"))
	t.Cleanup(px.Close)
	pxURL, _ := url.Parse(px.URL)
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pxURL)}}, px
}

func echo(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(seen{
		Method:        r.Method,
		Host:          r.Host,
		Path:          r.URL.RequestURI(),
		Authorization: r.Header.Get("Authorization"),
		APIKey:        r.Header.Get("X-Api-Key"),
	})
}

func get(t *testing.T, c *http.Client, method, target string, header http.Header) seen {
	t.Helper()
	req, _ := http.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var s seen
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReplacesBothCredentialHeaders(t *testing.T) {
	c, _ := start(t, http.HandlerFunc(echo))
	got := get(t, c, "POST", "http://proxy.test/v1/messages?x=1", http.Header{
		"Authorization": {"Bearer the-runs-own"},
		"X-Api-Key":     {"the-runs-own"},
	})
	if got.Authorization != "Bearer the-credential" || got.APIKey != "the-credential" {
		t.Errorf("upstream saw %q and %q", got.Authorization, got.APIKey)
	}
	if got.Method != "POST" || got.Path != "/v1/messages?x=1" {
		t.Errorf("upstream saw %s %s", got.Method, got.Path)
	}
}

func TestAddsCredentialWhenTheRunSendsNone(t *testing.T) {
	c, _ := start(t, http.HandlerFunc(echo))
	got := get(t, c, "GET", "http://proxy.test/", nil)
	if got.Authorization != "Bearer the-credential" || got.APIKey != "the-credential" {
		t.Errorf("upstream saw %q and %q", got.Authorization, got.APIKey)
	}
}

func TestForwardsARequestForAnotherHostToTheUpstream(t *testing.T) {
	c, _ := start(t, http.HandlerFunc(echo))
	got := get(t, c, "GET", "http://elsewhere.example/some/path", nil)
	if got.Path != "/some/path" {
		t.Errorf("upstream saw path %q", got.Path)
	}
	if strings.Contains(got.Host, "elsewhere") {
		t.Errorf("upstream saw host %q", got.Host)
	}
}

func TestRefusesConnect(t *testing.T) {
	_, px := start(t, http.HandlerFunc(echo))
	conn, err := net.Dial("tcp", strings.TrimPrefix(px.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("CONNECT got %d, want 405", resp.StatusCode)
	}
}

func TestStreamsAResponseAsItArrives(t *testing.T) {
	release := make(chan struct{})
	c, _ := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "second\n")
	}))
	c.Timeout = 5 * time.Second
	resp, err := c.Get("http://proxy.test/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	// The upstream holds back "second" until this has been read, so a proxy
	// that buffers the response never answers and the client's timeout fails
	// the test.
	line, err := r.ReadString('\n')
	if err != nil || line != "first\n" {
		t.Fatalf("first chunk: %q, %v", line, err)
	}
	close(release)
	line, err = r.ReadString('\n')
	if err != nil || line != "second\n" {
		t.Fatalf("second chunk: %q, %v", line, err)
	}
}

// heldBody is the request body the proxy's transport reads. It passes reads
// through until the request's Content-Length bytes have gone. The next read is
// the one the HTTP/1 transport always makes after the body's last byte
// (net/http, transfer.go). It waits until release is closed, then reads from the
// server's body and reports what that returned.
type heldBody struct {
	io.ReadCloser
	remaining int64
	ctx       context.Context
	release   <-chan struct{}
	done      chan struct{} // closed once err is set
	err       error
}

func (b *heldBody) Read(p []byte) (int, error) {
	if b.remaining > 0 {
		n, err := b.ReadCloser.Read(p[:min(int64(len(p)), b.remaining)])
		b.remaining -= int64(n)
		return n, err
	}
	select {
	case <-b.release:
	case <-b.ctx.Done():
	}
	n, err := b.ReadCloser.Read(p)
	b.err = err
	close(b.done)
	return n, err
}

// TestDoesNotCutOffAnAnswerItHasBegunToPassOn forces the order in which Go's
// HTTP/1 server closes a request's body at the answer's first write and the
// proxy's transport reads once more after the body's last byte (Go's issue
// 40747). The proxy is handler, unchanged; the wrapper only replaces the body.
// Every wait is on an event; the two 5 s limits only turn a hang into a failure.
func TestDoesNotCutOffAnAnswerItHasBegunToPassOn(t *testing.T) {
	const size = 50000
	release := make(chan struct{})
	var once sync.Once
	releaseNow := func() { once.Do(func() { close(release) }) }
	defer releaseNow()
	held := &heldBody{release: release, done: make(chan struct{})}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-held.done:
			if held.err == io.EOF {
				io.WriteString(w, "rest\n")
				return
			}
		case <-r.Context().Done():
		}
		<-r.Context().Done()
	}))
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)
	proxy := handler(upURL, "the-credential")
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		held.ReadCloser, held.remaining, held.ctx = r.Body, r.ContentLength, r.Context()
		r.Body = held
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(px.Close)
	pxURL, _ := url.Parse(px.URL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pxURL)}, Timeout: 5 * time.Second}

	resp, err := c.Post("http://proxy.test/v1/messages", "application/octet-stream", bytes.NewReader(make([]byte, size)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The client has the answer's headers, so the proxy's first write has happened.
	releaseNow()
	select {
	case <-held.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the transport made no read after the body's last byte; the test no longer forces the order")
	}
	if held.err != io.EOF {
		t.Errorf("the read after the body's last byte got %v, want EOF", held.err)
	}
	got, err := io.ReadAll(resp.Body)
	if string(got) != "first\nrest\n" || err != nil {
		t.Errorf("the run got %q, %v; want the whole answer", got, err)
	}
}

func TestSettings(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, _, err := settings(env(map[string]string{credentialVar: "c"})); err == nil {
		t.Error("started without an upstream")
	}
	if _, _, err := settings(env(map[string]string{upstreamVar: "https://up.example"})); err == nil {
		t.Error("started without a credential")
	}
	if _, _, err := settings(env(map[string]string{upstreamVar: "up.example", credentialVar: "c"})); err == nil {
		t.Error("started with an upstream that is not a URL")
	}
	u, c, err := settings(env(map[string]string{upstreamVar: "https://up.example", credentialVar: "c"}))
	if err != nil || u.Host != "up.example" || c != "c" {
		t.Errorf("got %v, %q, %v", u, c, err)
	}
}
