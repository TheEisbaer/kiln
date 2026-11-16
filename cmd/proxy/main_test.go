package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
