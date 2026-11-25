// Command wayout is a run's way out (ADR 0059): it carries a run's HTTPS
// connections to the hosts on the run's list, and to nothing that is not
// public. It holds no credential. One runs per run that has a list, on the
// run's network and on the outward network.
//
// It answers only CONNECT to port 443. A name must be on the list, or the list
// is `*`; it is resolved once, every answer is judged by the address rule
// (rules.go), and only the judged addresses are dialled, with the socket's
// address judged again right before the connect. Nothing inside a connection
// is read or changed. Each request writes one line of JSON to standard output,
// and each connection writes one more line at its end, with the bytes copied
// each way, which are the totals of the copies. On SIGTERM it ends the open
// connections, whose lines then say "stopped", waits for those lines for at
// most stopWait, and exits.
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
	"os/signal"
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

// stopWait bounds how long a stop waits for the open connections' lines.
// docker stop's own bound before it kills is longer (internal/seal).
const stopWait = 2 * time.Second

func main() {
	list, err := parseList(os.Getenv(hostsVar))
	if err != nil {
		fmt.Fprintln(os.Stderr, "wayout:", err)
		os.Exit(1)
	}
	// The way out is PID 1 of its container: SIGTERM reaches it only because
	// it has a handler for it.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wayout:", err)
		os.Exit(1)
	}
	w := &wayOut{list: list, resolve: resolve, connect: dialTCP, out: os.Stdout}
	if err := serve(ctx, ln, w); err != nil {
		fmt.Fprintln(os.Stderr, "wayout:", err)
		os.Exit(1)
	}
}

// serve serves on ln until ctx is done, then stops: it accepts no more
// requests, ends every open connection, and waits at most stopWait for the
// requests under way, whose lines are then written. It returns nil after a
// stop, and the server's error when serving fails.
func serve(ctx context.Context, ln net.Listener, w *wayOut) error {
	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv := &http.Server{Handler: w, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return base }}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	srv.Close()
	cancelBase() // the lookups and connects under way end
	if left := w.stop(stopWait); left > 0 {
		fmt.Fprintf(os.Stderr, "wayout: %d requests had not ended %s after the stop\n", left, stopWait)
	}
	return nil
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

	// open guards what a stop needs: the open connections, the requests
	// under way, whether the way out is stopping, and the channel a stop
	// waits on.
	open     sync.Mutex
	tunnels  map[*tunnel]bool
	active   int
	stopping bool
	idle     chan struct{} // closed when active reaches 0 during a stop
}

// tunnel is one open connection: the run's side and the host's.
type tunnel struct {
	client, upstream net.Conn
	stopped          bool // closed by a stop; guarded by wayOut.open
}

// begin counts a request under way; end, deferred, counts it out.
func (w *wayOut) begin() {
	w.open.Lock()
	defer w.open.Unlock()
	w.active++
}

func (w *wayOut) end() {
	w.open.Lock()
	defer w.open.Unlock()
	w.active--
	if w.active == 0 && w.idle != nil {
		close(w.idle)
		w.idle = nil
	}
}

// track adds an open connection to the set, or returns false once the way
// out is stopping: the caller then closes it at once.
func (w *wayOut) track(t *tunnel) bool {
	w.open.Lock()
	defer w.open.Unlock()
	if w.stopping {
		t.stopped = true
		return false
	}
	if w.tunnels == nil {
		w.tunnels = map[*tunnel]bool{}
	}
	w.tunnels[t] = true
	return true
}

// untrack takes a connection out of the set and reports whether a stop
// closed it.
func (w *wayOut) untrack(t *tunnel) bool {
	w.open.Lock()
	defer w.open.Unlock()
	delete(w.tunnels, t)
	return t.stopped
}

// stop closes every open connection, and any opened after it, and waits at
// most wait for the requests under way to end. It returns how many had not.
func (w *wayOut) stop(wait time.Duration) int {
	w.open.Lock()
	w.stopping = true
	for t := range w.tunnels {
		t.stopped = true
		t.client.Close()
		t.upstream.Close()
	}
	if w.active == 0 {
		w.open.Unlock()
		return 0
	}
	if w.idle == nil {
		w.idle = make(chan struct{})
	}
	idle := w.idle
	w.open.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-idle:
		return 0
	case <-timer.C:
		w.open.Lock()
		defer w.open.Unlock()
		return w.active
	}
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

// The ends of a connection: the side whose copy ended first without an
// error, an error of that copy, or a stop of the way out.
const (
	endRun     = "run closed"
	endHost    = "host closed"
	endError   = "error"
	endStopped = "stopped"
)

// ending is the line of JSON written at a connection's end. The bytes are the
// totals the copies return: sent from the run to the host, with what the run
// sent behind its CONNECT, and received from the host, without the way out's
// own answer.
type ending struct {
	Time       string `json:"time"`
	Target     string `json:"target"`
	Sent       int64  `json:"sent"`
	Received   int64  `json:"received"`
	DurationMS int64  `json:"duration_ms"`
	End        string `json:"end"`
	Error      string `json:"error,omitempty"`
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
	w.begin()
	defer w.end()
	begun := time.Now()
	e := entry{Time: begun.UTC().Format(time.RFC3339Nano), Method: r.Method, Target: r.Host}
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
	client, buffered, err := http.NewResponseController(rw).Hijack()
	if err != nil {
		upstream.Close()
		e.Decision, e.Reason, e.Status = "allowed", "the connection could not be taken over: "+err.Error(), http.StatusInternalServerError
		w.write(e)
		return
	}
	e.Decision, e.Status = "allowed", http.StatusOK
	w.write(e)
	t := &tunnel{client: client, upstream: upstream}
	ended := func(sent, received int64, end string, err error) {
		now := time.Now()
		l := ending{Time: now.UTC().Format(time.RFC3339Nano), Target: e.Target, Sent: sent, Received: received,
			DurationMS: now.Sub(begun).Milliseconds(), End: end}
		if end == endError && err != nil {
			l.Error = err.Error()
		}
		w.write(l)
	}
	if !w.track(t) {
		// The dial finished after a stop began.
		client.Close()
		upstream.Close()
		ended(0, 0, endStopped, nil)
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		client.Close()
		upstream.Close()
		end := endError
		if w.untrack(t) {
			end = endStopped
		}
		ended(0, 0, end, err)
		return
	}

	// The bytes are copied both ways until one side closes; whatever the
	// client sent behind its request is passed on first. Then both sides are
	// closed, and the other copy is waited for, so that both totals are
	// known.
	type copied struct {
		n    int64
		err  error
		sent bool // the copy from the run to the host
	}
	done := make(chan copied, 2)
	go func() { n, err := io.Copy(upstream, buffered.Reader); done <- copied{n, err, true} }()
	go func() { n, err := io.Copy(client, upstream); done <- copied{n, err, false} }()
	first := <-done
	client.Close()
	upstream.Close()
	second := <-done
	sent, received := first.n, second.n
	if !first.sent {
		sent, received = second.n, first.n
	}
	end := endError
	switch {
	case w.untrack(t):
		end = endStopped
	case first.err == nil && first.sent:
		end = endRun
	case first.err == nil:
		end = endHost
	}
	ended(sent, received, end, first.err)
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

// write writes one line of JSON to the output, in one Write.
func (w *wayOut) write(e any) {
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
