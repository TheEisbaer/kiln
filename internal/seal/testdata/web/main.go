// Command web stands in for a public host in the tests of a run with a list
// of hosts (ADR 0059). It serves HTTPS on ports 443 and 8443 for every name
// under kiln-test.example, with a certificate authority of its own that it
// makes when it starts. It writes, as lines of JSON on standard output, first
// that authority's certificate, then every request with all its headers, so
// that the test can tell that no request carries the credential.
//
// /page answers a page that holds the marker given as its argument,
// /redirect sends the client to https://private.kiln-test.example/, /hold
// answers the marker and holds the connection open until the client goes
// away, and anything else answers the marker as plain text.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log"
	"math/big"
	"net/http"
	"os"
	"sync"
	"time"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] == "" {
		log.Fatal("usage: web <marker>")
	}
	marker := os.Args[1]
	caPEM, cert := certificates()
	var mu sync.Mutex
	out := json.NewEncoder(os.Stdout)
	write := func(v any) { mu.Lock(); defer mu.Unlock(); out.Encode(v) }
	write(map[string]string{"kind": "ca", "pem": caPEM})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write(map[string]any{"kind": "request", "host": r.Host, "path": r.URL.Path, "method": r.Method,
			"remote": r.RemoteAddr, "headers": r.Header})
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte("<!doctype html><html><head><title>Kiln's test page</title></head><body>" +
				"<h1>Kiln's test page</h1><p>The page's marker is " + marker + ".</p></body></html>\n"))
		case "/hold":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte(marker + "\n"))
			http.NewResponseController(w).Flush()
			<-r.Context().Done()
		case "/redirect":
			http.Redirect(w, r, "https://private.kiln-test.example/", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte(marker + "\n"))
		}
	})
	config := &tls.Config{Certificates: []tls.Certificate{cert}}
	errs := make(chan error, 2)
	for _, addr := range []string{":443", ":8443"} {
		srv := &http.Server{Addr: addr, Handler: handler, TLSConfig: config, ReadHeaderTimeout: 10 * time.Second}
		go func() { errs <- srv.ListenAndServeTLS("", "") }()
	}
	log.Fatal(<-errs)
}

// certificates makes a certificate authority and a certificate for
// kiln-test.example and every name under it, and returns the authority's
// certificate as PEM and the other with its key.
func certificates() (string, tls.Certificate) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(err)
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Kiln's test authority"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	check(err)
	ca, err = x509.ParseCertificate(caDER)
	check(err)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(err)
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "kiln-test.example"},
		DNSNames:  []string{"kiln-test.example", "*.kiln-test.example"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	check(err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
