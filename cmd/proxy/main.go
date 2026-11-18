// Command proxy is the only way a run reaches Claude (ADR 0017, ADR 0034). It
// forwards every plain-HTTP request from runs to one upstream, whatever host the
// request names, and puts the credential on it.
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"
)

// The names of the variables the proxy starts from. internal/seal passes the
// same two names; a mismatch shows as a proxy that does not start.
const (
	upstreamVar   = "KILN_PROXY_UPSTREAM"
	credentialVar = "KILN_PROXY_CREDENTIAL"
)

// listen is the address the proxy serves on, inside its container, in both IPv4
// and IPv6.
const listen = ":8080"

func main() {
	upstream, credential, err := settings(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "proxy:", err)
		os.Exit(1)
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           handler(upstream, credential),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// settings reads the upstream and the credential. Without either the proxy does
// not start.
//
// The credential comes from the environment for now. The card that builds the
// core's push (ADR 0040) removes this in the same pull request.
func settings(getenv func(string) string) (*url.URL, string, error) {
	raw := getenv(upstreamVar)
	if raw == "" {
		return nil, "", errors.New(upstreamVar + " is not set")
	}
	upstream, err := url.Parse(raw)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" {
		return nil, "", fmt.Errorf("%s is not an http or https URL", upstreamVar)
	}
	credential := getenv(credentialVar)
	if credential == "" {
		return nil, "", errors.New(credentialVar + " is not set")
	}
	return upstream, credential, nil
}

// handler forwards every request to upstream, whatever host it names, with
// the credential in both headers Claude Code sends a key helper's output in.
// It refuses CONNECT and streams responses as they arrive.
func handler(upstream *url.URL, credential string) http.Handler {
	forward := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
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
		// Go's HTTP/1 server closes a request's body at the answer's first write,
		// while the transport may still read from it, and the transport then cuts
		// off the answer (Go's issue 40747). The server's own writer always
		// supports this, and TestDoesNotCutOffAnAnswerItHasBegunToPassOn fails
		// without it.
		http.NewResponseController(w).EnableFullDuplex()
		forward.ServeHTTP(w, r)
	})
}
