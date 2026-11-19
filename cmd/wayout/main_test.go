package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// TestNotPublic judges one address of every row of the table, and addresses
// just outside the blocks, which are public.
func TestNotPublic(t *testing.T) {
	refused := []string{
		// IPv4, one per block, and the edges of a few.
		"0.0.0.0", "0.255.255.255",
		"10.0.0.1", "10.255.255.1",
		"100.64.0.1", "100.127.255.255",
		"127.0.0.1", "127.255.255.254",
		"169.254.169.254",
		"172.16.0.1", "172.18.0.3", "172.31.255.255",
		"192.0.0.9",
		"192.0.2.1",
		"192.31.196.1",
		"192.52.193.1",
		"192.88.99.2",
		"192.168.1.1",
		"192.175.48.1",
		"198.18.0.1", "198.19.255.255",
		"198.51.100.1",
		"203.0.113.1",
		"224.0.0.1", "239.255.255.255",
		"240.0.0.1", "255.255.255.255",
		// IPv6 outside 2000::/3.
		"::1", "::", "::ffff:10.0.0.1", "::ffff:7f00:1", "::1.2.3.4",
		"64:ff9b::8.8.8.8", "64:ff9b:1::1",
		"100::1", "5f00::1",
		"fc00::1", "fd12:3456::1",
		"fe80::1", "febf::1",
		"fec0::1",
		"ff02::1",
		// IPv6 inside 2000::/3.
		"2001::1", "2001:0:4136:e378::1", // Teredo
		"2001:1::1", "2001:2::1", "2001:1ff:ffff::1", // 2001::/23
		"2001:db8::1",
		"2002::1", "2002:c000:0201::1", // 6to4
		"2620:4f:8000::1",
		"3fff::1", "3fff:fff::1",
	}
	public := []string{
		"1.1.1.1", "8.8.8.8", "100.63.255.255", "100.128.0.0", "169.253.255.255", "172.15.255.255",
		"172.32.0.1", "192.0.1.1", "192.88.100.1", "192.169.0.1", "198.20.0.1", "223.255.255.255",
		"2606:4700:4700::1111", "2001:200::1", "2001:db9::1", "2003::1", "2620:4f:8001::1", "3ffe::1", "3fff:1000::1",
	}
	for _, s := range refused {
		a := netip.MustParseAddr(s)
		if a.Is4In6() && !strings.Contains(s, "::ffff:") {
			t.Fatalf("%s parsed as a mapped address", s)
		}
		if notPublic(a) == "" {
			t.Errorf("%s is judged public", s)
		}
	}
	for _, s := range public {
		if reason := notPublic(netip.MustParseAddr(s)); reason != "" {
			t.Errorf("%s is refused: %s", s, reason)
		}
	}
}

func TestReadHost(t *testing.T) {
	for _, c := range []struct{ in, host, addr, reason string }{
		{in: "Public.Example.", host: "public.example"},
		{in: "go.dev", host: "go.dev"},
		{in: "8.8.8.8", host: "8.8.8.8", addr: "8.8.8.8"},
		{in: "::ffff:7f00:1", host: "::ffff:7f00:1", addr: "::ffff:127.0.0.1"},
		{in: "fe80::1%eth0", reason: "has a zone"},
		{in: "2130706433", reason: "written as a number"},
		{in: "0x7f000001", reason: "written as a number"},
		{in: "0177.0.0.1", reason: "written as a number"},
		{in: "127.1", reason: "written as a number"},
		{in: "example.0x1", reason: "written as a number"},
		{in: "localhost", reason: "is not a name"},
		{in: "kiln-proxy", reason: "is not a name"},
		{in: "a_b.example", reason: "is not a name"},
		{in: "a b.example", reason: "is not a name"},
		{in: "", reason: "is not a name"},
		{in: "a..example", reason: "is not a name"},
	} {
		host, addr, reason := readHost(c.in)
		switch {
		case c.reason != "":
			if !strings.Contains(reason, c.reason) {
				t.Errorf("%q: the reason is %q, want %q", c.in, reason, c.reason)
			}
		case reason != "" || host != c.host || (c.addr != "") != addr.IsValid() || (addr.IsValid() && addr.String() != c.addr):
			t.Errorf("%q: got %q, %v, %q; want %q, %q", c.in, host, addr, reason, c.host, c.addr)
		}
	}
}

func TestParseList(t *testing.T) {
	for _, s := range []string{"*", "go.dev", "go.dev,pkg.go.dev,api.anthropic.com"} {
		if _, err := parseList(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"", "*,go.dev", "go.dev,*", "Go.dev", "go.dev.", "*.go.dev", "go.dev,", "localhost", "127.0.0.1", "[::1]"} {
		if _, err := parseList(s); err == nil {
			t.Errorf("%q: no error", s)
		}
	}
	l, _ := parseList("go.dev")
	if !l.allows("go.dev") || l.allows("pkg.go.dev") || l.allows("dev") {
		t.Error("the list go.dev does not match exactly")
	}
}

// fake is a way out with a resolver and a dialler of the test's own.
type fake struct {
	t       *testing.T
	answers map[string][]string // by name
	// socket is the address the dialler reports for its socket, as the kernel
	// would connect to it; empty for the address it was asked for.
	socket   string
	noJudge  bool // the dialler is given no Control
	mu       sync.Mutex
	resolved []string
	dialled  []string
	out      bytes.Buffer
}

func (f *fake) server(listSpec string) *httptest.Server {
	l, err := parseList(listSpec)
	if err != nil {
		f.t.Fatal(err)
	}
	w := &wayOut{list: l, out: &f.out,
		resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			f.mu.Lock()
			f.resolved = append(f.resolved, host)
			f.mu.Unlock()
			var addrs []netip.Addr
			for _, a := range f.answers[host] {
				addrs = append(addrs, netip.MustParseAddr(a))
			}
			if len(addrs) == 0 {
				return nil, errors.New("no such host")
			}
			return addrs, nil
		},
		connect: func(_ context.Context, address string, control controlFunc) (net.Conn, error) {
			f.mu.Lock()
			f.dialled = append(f.dialled, address)
			f.mu.Unlock()
			socket := address
			if f.socket != "" {
				socket = f.socket
			}
			if control != nil && !f.noJudge {
				if err := control("tcp", socket, nil); err != nil {
					return nil, err
				}
			}
			// An upstream that answers every line with its upper case.
			here, there := net.Pipe()
			go func() {
				defer there.Close()
				s := bufio.NewScanner(there)
				for s.Scan() {
					fmt.Fprintln(there, strings.ToUpper(s.Text()))
				}
			}()
			return here, nil
		},
	}
	srv := httptest.NewServer(w)
	f.t.Cleanup(srv.Close)
	return srv
}

// send sends one request line to the way out and returns the answer's status
// and body. After a 200 it sends a line through the tunnel and returns what
// came back as the body.
func send(t *testing.T, srv *httptest.Server, method, target string) (int, string) {
	t.Helper()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	host := target
	if u, err := url.Parse(target); err == nil && u.Scheme != "" {
		host = u.Host
	}
	fmt.Fprintf(c, "%s %s HTTP/1.1\r\nHost: %s\r\n\r\n", method, target, host)
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(body))
	}
	fmt.Fprintln(c, "through the tunnel")
	line, err := r.ReadString('\n')
	if err != nil {
		return resp.StatusCode, "the tunnel: " + err.Error()
	}
	return resp.StatusCode, strings.TrimSpace(line)
}

func TestWayOut(t *testing.T) {
	answers := map[string][]string{
		"public.example":   {"3ffe:c12::1", "8.8.8.8"},
		"mapped.example":   {"::ffff:8.8.8.8"}, // as Go's resolver gives an A record
		"private.example":  {"172.18.0.3"},
		"inmapped.example": {"::ffff:10.0.0.1"},
		"mixed.example":    {"172.18.0.4", "3ffe:c12::2"},
		"mixed6.example":   {"3ffe:c12::2", "fd00::4"},
	}
	for _, c := range []struct {
		what, list, method, target string
		status                     int
		reason                     string // in the body
		dialled                    string // the addresses dialled, joined by spaces
		resolvedNone               bool   // no name is resolved
	}{
		{what: "a public name", list: "*", target: "public.example:443", status: 200, reason: "THROUGH THE TUNNEL", dialled: "[3ffe:c12::1]:443"},
		{what: "a public name on the list", list: "public.example", target: "Public.Example.:443", status: 200, reason: "THROUGH THE TUNNEL"},
		{what: "an IPv4 answer in mapped form", list: "*", target: "mapped.example:443", status: 200, dialled: "8.8.8.8:443"},
		{what: "a name not on the list", list: "public.example", target: "other.example:443", status: 403, reason: "other.example is not on the list", resolvedNone: true},
		{what: "a subdomain of a name on the list", list: "public.example", target: "www.public.example:443", status: 403, reason: "not on the list", resolvedNone: true},
		{what: "an address with a list of names", list: "public.example", target: "8.8.8.8:443", status: 403, reason: "only the list * allows"},
		{what: "another port", list: "*", target: "public.example:8443", status: 403, reason: "port 8443 is not 443", resolvedNone: true},
		{what: "port 0443", list: "*", target: "public.example:0443", status: 403, reason: "port 0443"},
		{what: "plain HTTP", list: "*", method: "GET", target: "http://public.example/", status: 403, reason: "only CONNECT", resolvedNone: true},
		{what: "a private address", list: "*", target: "172.18.0.3:443", status: 403, reason: "refused: 172.18.0.3 is not public"},
		{what: "loopback", list: "*", target: "127.0.0.1:443", status: 403, reason: "127.0.0.1 is not public"},
		{what: "IPv6 loopback", list: "*", target: "[::1]:443", status: 403, reason: "::1 is not public"},
		{what: "link-local", list: "*", target: "169.254.169.254:443", status: 403, reason: "not public"},
		{what: "IPv6 link-local", list: "*", target: "[fe80::1]:443", status: 403, reason: "not public"},
		{what: "a zone", list: "*", target: "[fe80::1%25eth0]:443", status: 403, reason: "has a zone"},
		{what: "a mapped private address", list: "*", target: "[::ffff:172.18.0.3]:443", status: 403, reason: "refused: 172.18.0.3 is not public"},
		{what: "a mapped loopback", list: "*", target: "[::ffff:7f00:1]:443", status: 403, reason: "refused: 127.0.0.1 is not public"},
		{what: "NAT64", list: "*", target: "[64:ff9b::7f00:1]:443", status: 403, reason: "not public"},
		{what: "6to4", list: "*", target: "[2002:7f00:1::1]:443", status: 403, reason: "not public"},
		{what: "Teredo", list: "*", target: "[2001:0:4136:e378:8000:63bf:3fff:fdd2]:443", status: 403, reason: "not public"},
		{what: "a documentation address", list: "*", target: "[2001:db8::1]:443", status: 403, reason: "not public"},
		{what: "multicast", list: "*", target: "[ff02::1]:443", status: 403, reason: "not public"},
		{what: "a decimal address", list: "*", target: "2130706433:443", status: 403, reason: "written as a number", resolvedNone: true},
		{what: "a hexadecimal address", list: "*", target: "0x7f000001:443", status: 403, reason: "written as a number", resolvedNone: true},
		{what: "an octal address", list: "*", target: "0177.0.0.1:443", status: 403, reason: "written as a number", resolvedNone: true},
		{what: "a short address", list: "*", target: "127.1:443", status: 403, reason: "written as a number", resolvedNone: true},
		{what: "a name of a private address", list: "*", target: "private.example:443", status: 403, reason: "private.example resolves to 172.18.0.3, which is not public"},
		{what: "a name of a mapped private address", list: "*", target: "inmapped.example:443", status: 403, reason: "resolves to 10.0.0.1"},
		{what: "a name with a private answer first", list: "*", target: "mixed.example:443", status: 403, reason: "resolves to 172.18.0.4"},
		{what: "a name with a private answer last", list: "*", target: "mixed6.example:443", status: 403, reason: "resolves to fd00::4"},
		{what: "a name that does not resolve", list: "*", target: "none.example:443", status: 403, reason: "does not resolve"},
		{what: "a name without a dot", list: "*", target: "kiln-proxy:443", status: 403, reason: "is not a name", resolvedNone: true},
	} {
		t.Run(c.what, func(t *testing.T) {
			f := &fake{t: t, answers: answers}
			srv := f.server(c.list)
			method := c.method
			if method == "" {
				method = http.MethodConnect
			}
			status, body := send(t, srv, method, c.target)
			if status != c.status || !strings.Contains(body, c.reason) {
				t.Errorf("got %d %q, want %d with %q", status, body, c.status, c.reason)
			}
			if c.status == 403 && len(f.dialled) > 0 {
				t.Errorf("a refused request dialled %q", f.dialled)
			}
			if c.dialled != "" && strings.Join(f.dialled, " ") != c.dialled {
				t.Errorf("dialled %q, want %q", f.dialled, c.dialled)
			}
			if c.resolvedNone && len(f.resolved) > 0 {
				t.Errorf("resolved %q", f.resolved)
			}

			// One line of JSON per request.
			var e entry
			lines := strings.Split(strings.TrimSpace(f.out.String()), "\n")
			if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &e) != nil {
				t.Fatalf("the output is %q", f.out.String())
			}
			want := map[int]string{200: "allowed", 403: "refused"}[c.status]
			if e.Decision != want || e.Status != c.status || e.Method != method || e.Time == "" {
				t.Errorf("the line is %+v", e)
			}
		})
	}
}

// TestControl: a name answers with a public address, and the socket would
// connect to a private one, as when the name resolves differently between the
// check and the connect. The socket's address is judged again.
func TestControl(t *testing.T) {
	f := &fake{t: t, answers: map[string][]string{"public.example": {"8.8.8.8"}}, socket: "172.18.0.3:443"}
	srv := f.server("*")
	status, body := send(t, srv, http.MethodConnect, "public.example:443")
	if status != http.StatusBadGateway || !strings.Contains(body, "172.18.0.3 is not public") {
		t.Errorf("got %d %q, want 502 with the socket's address refused", status, body)
	}
	if len(f.resolved) != 1 {
		t.Errorf("the name was resolved %d times, want once", len(f.resolved))
	}
}

// TestJudgeSocket judges the address strings that net.Dialer gives Control.
func TestJudgeSocket(t *testing.T) {
	for address, ok := range map[string]bool{
		"8.8.8.8:443": true, "[3ffe::1]:443": true, "[::ffff:8.8.8.8]:443": false,
		"172.18.0.3:443": false, "[fd00::1]:443": false, "[::1]:443": false, "nonsense": false,
	} {
		if err := judgeSocket("tcp", address, nil); (err == nil) != ok {
			t.Errorf("%s: %v, want allowed %v", address, err, ok)
		}
	}
}
