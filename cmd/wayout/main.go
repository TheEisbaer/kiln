// Command wayout is a run's way out (ADR 0059): it carries a run's HTTPS
// connections to the hosts on the run's list, and to nothing that is not
// public. It holds no credential. One runs per run that has a list, on the
// run's network and on the outward network.
//
// It answers only CONNECT to port 443. A name must be on the list, or the list
// is `*`; it is resolved once, every answer is judged by the address rule
// (rules.go), and only the judged addresses are dialled, with the socket's
// address judged again right before the connect. Nothing inside a connection
// is read or changed. Each request writes one line of JSON to standard output.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// hostsVar is the variable that holds the list (internal/seal sets it).
const hostsVar = "KILN_WAYOUT_HOSTS"

// listen is the address the way out serves on, inside its container.
const listen = ":3128"

// dialTimeout bounds the connects of one request, all addresses together.
const dialTimeout = 10 * time.Second

func main() {
	list, err := parseList(os.Getenv(hostsVar))
	if err != nil {
		fmt.Fprintln(os.Stderr, "wayout:", err)
		os.Exit(1)
	}
	w := &wayOut{list: list, resolve: resolve, connect: dialTCP, out: os.Stdout}
	srv := &http.Server{Addr: listen, Handler: w, ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintln(os.Stderr, "wayout:", srv.ListenAndServe())
	os.Exit(1)
}

// controlFunc judges a socket's address right before it connects, as
// net.Dialer's Control does.
type controlFunc func(network, address string, c syscall.RawConn) error

// wayOut is the way out's handler.
type wayOut struct {
	list    list
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	connect func(ctx context.Context, address string, control controlFunc) (net.Conn, error)
	mu      sync.Mutex // guards out
	out     io.Writer
}

// resolve resolves a name once, with Go's own resolver: the program has no C.
func resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// dialTCP connects to a literal address, so that nothing is looked up again,
// with control judging the socket's address right before the connect.
func dialTCP(ctx context.Context, address string, control controlFunc) (net.Conn, error) {
	d := net.Dialer{Control: control}
	return d.DialContext(ctx, "tcp", address)
}

// judgeSocket is the Control of every dial: the address that was judged is
// the one connected to.
func judgeSocket(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("the socket's address %q cannot be read", address)
	}
	if reason := notPublic(ap.Addr()); reason != "" {
		return errors.New(reason)
	}
	return nil
}

// entry is the line of JSON written per request.
type entry struct {
	Time      string   `json:"time"`
	Method    string   `json:"method"`
	Target    string   `json:"target"` // the host and port, as asked
	Addresses []string `json:"addresses,omitempty"`
	Decision  string   `json:"decision"` // allowed or refused
	Reason    string   `json:"reason,omitempty"`
	Status    int      `json:"status"`
}

func (w *wayOut) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	e := entry{Time: time.Now().UTC().Format(time.RFC3339Nano), Method: r.Method, Target: r.Host}
	if r.Method != http.MethodConnect {
		// A plain-HTTP request in absolute form among them.
		e.Target = r.RequestURI
		w.refuse(rw, &e, "only CONNECT is answered")
		return
	}
	addrs, reason := w.judge(r.Context(), r.Host)
	for _, a := range addrs {
		e.Addresses = append(e.Addresses, a.String())
	}
	if reason != "" {
		w.refuse(rw, &e, reason)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	defer cancel()
	var upstream net.Conn
	var errs []error
	for _, a := range addrs {
		c, err := w.connect(ctx, netip.AddrPortFrom(a, 443).String(), judgeSocket)
		if err == nil {
			upstream = c
			break
		}
		errs = append(errs, err)
	}
	if upstream == nil {
		e.Decision, e.Reason, e.Status = "allowed", "no address connected: "+errors.Join(errs...).Error(), http.StatusBadGateway
		w.write(e)
		http.Error(rw, e.Reason, e.Status)
		return
	}
	defer upstream.Close()

	client, buffered, err := http.NewResponseController(rw).Hijack()
	if err != nil {
		e.Decision, e.Reason, e.Status = "allowed", "the connection could not be taken over: "+err.Error(), http.StatusInternalServerError
		w.write(e)
		return
	}
	defer client.Close()
	e.Decision, e.Status = "allowed", http.StatusOK
	w.write(e)
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}

	// The bytes are copied both ways until one side closes; whatever the
	// client sent behind its request is passed on first.
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, buffered.Reader); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

// judge applies the rules of the host, the list and the address to a CONNECT
// target, and returns the addresses to dial in order, or the reason it is
// refused. A name is resolved only once it is on the list.
func (w *wayOut) judge(ctx context.Context, target string) ([]netip.Addr, string) {
	h, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, fmt.Sprintf("%q is not a host and port", target)
	}
	if port != "443" {
		return nil, fmt.Sprintf("port %s is not 443", port)
	}
	host, addr, reason := readHost(h)
	if reason != "" {
		return nil, reason
	}
	if addr.IsValid() {
		if !w.list.all {
			return nil, fmt.Sprintf("%s is an address, which only the list * allows", addr)
		}
		addr = addr.Unmap()
		if reason := notPublic(addr); reason != "" {
			return []netip.Addr{addr}, reason
		}
		return []netip.Addr{addr}, ""
	}
	if !w.list.allows(host) {
		return nil, fmt.Sprintf("%s is not on the list", host)
	}
	answers, err := w.resolve(ctx, host)
	if err != nil || len(answers) == 0 {
		return nil, fmt.Sprintf("%s does not resolve", host)
	}
	addrs := make([]netip.Addr, len(answers))
	for i, a := range answers {
		addrs[i] = a.Unmap()
	}
	// A name with one refused answer is refused whole.
	for _, a := range addrs {
		if notPublic(a) != "" {
			return addrs, fmt.Sprintf("%s resolves to %s, which is not public", host, a)
		}
	}
	return addrs, ""
}

// refuse answers 403 with the reason in the body, and writes the line.
func (w *wayOut) refuse(rw http.ResponseWriter, e *entry, reason string) {
	e.Decision, e.Reason, e.Status = "refused", reason, http.StatusForbidden
	w.write(*e)
	http.Error(rw, "refused: "+reason, http.StatusForbidden)
}

// write writes one line of JSON to the output.
func (w *wayOut) write(e entry) {
	line, _ := json.Marshal(e)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.out.Write(append(line, '\n'))
}

// list is the list of hosts: every public host, or names that must match
// exactly.
type list struct {
	all   bool
	names map[string]bool
}

func (l list) allows(host string) bool { return l.all || l.names[host] }

// parseList reads the list: `*`, or names separated by commas, each as
// readName accepts it. An empty or bad list is an error.
func parseList(s string) (list, error) {
	if s == "*" {
		return list{all: true}, nil
	}
	if s == "" {
		return list{}, errors.New(hostsVar + " is not set")
	}
	l := list{names: map[string]bool{}}
	for _, name := range strings.Split(s, ",") {
		if !isName(name) {
			return list{}, fmt.Errorf("%s: %q is not a name in lower case with a dot, or * alone", hostsVar, name)
		}
		l.names[name] = true
	}
	return l, nil
}
