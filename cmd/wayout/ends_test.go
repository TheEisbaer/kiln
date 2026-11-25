package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// endsCase is a way out served as main serves it, whose connects reach a
// TCP listener of the test's own, so that the copies run over TCP as in the
// container.
type endsCase struct {
	t      *testing.T
	w      *wayOut
	out    *lockedBuffer
	addr   string        // the way out's address
	host   net.Listener  // the test's host
	cancel func()        // stops the way out, as SIGTERM does
	served chan error    // serve's answer
	dialed chan struct{} // gets a value when a connect is called
	// release, when it is not nil, holds every connect until it is closed.
	release chan struct{}
}

func newEndsCase(t *testing.T, release chan struct{}) *endsCase {
	t.Helper()
	host, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &endsCase{t: t, out: &lockedBuffer{}, addr: ln.Addr().String(), host: host,
		served: make(chan error, 1), dialed: make(chan struct{}, 4), release: release}
	l, _ := parseList("*")
	c.w = &wayOut{list: l, out: c.out,
		resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		// The address asked for is public; the one connected to is the
		// test's, which the socket's rule would refuse, so it is not given.
		connect: func(context.Context, string, controlFunc) (net.Conn, error) {
			c.dialed <- struct{}{}
			if c.release != nil {
				<-c.release
			}
			return net.Dial("tcp", host.Addr().String())
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go func() { c.served <- serve(ctx, ln, c.w) }()
	t.Cleanup(func() {
		cancel()
		<-c.served
	})
	return c
}

// connect sends a CONNECT with first behind it in the same write, and
// returns the connection and its reader once the answer is 200.
func (c *endsCase) connect(first []byte) (net.Conn, *bufio.Reader) {
	c.t.Helper()
	conn, err := net.Dial("tcp", c.addr)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { conn.Close() })
	req := []byte("CONNECT public.example:443 HTTP/1.1\r\nHost: public.example:443\r\n\r\n")
	if _, err := conn.Write(append(req, first...)); err != nil {
		c.t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("the answer is %d", resp.StatusCode)
	}
	return conn, r
}

// accept accepts one connection at the host.
func (c *endsCase) accept() net.Conn {
	c.t.Helper()
	conn, err := c.host.Accept()
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { conn.Close() })
	return conn
}

// end waits for the output's two lines, and returns the end line after
// checking that the first is the request's.
func (c *endsCase) end() ending {
	c.t.Helper()
	lines := c.out.lines(2)
	if len(lines) != 2 {
		c.t.Fatalf("the output is %q, want two lines", c.out.String())
	}
	var e entry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil || e.Status != 200 || e.Decision != "allowed" {
		c.t.Fatalf("the first line is %q", lines[0])
	}
	var end ending
	if err := json.Unmarshal([]byte(lines[1]), &end); err != nil {
		c.t.Fatalf("the end line is %q: %v", lines[1], err)
	}
	if end.Target != e.Target || end.DurationMS < 0 || end.Time == "" {
		c.t.Errorf("the end line is %+v, after %+v", end, e)
	}
	if end.End != endError && strings.Contains(lines[1], `"error"`) {
		c.t.Errorf("the end line has an error without the end error: %q", lines[1])
	}
	return end
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

// TestEnds: a connection's bytes are counted each way, with what the run
// sent behind its CONNECT, and written at its end with the side that closed.
func TestEnds(t *testing.T) {
	const up, down, behind = 300_000, 500_000, 1_000
	toHost, toRun := pattern(up, 1), pattern(down, 7)
	for _, c := range []struct {
		what string
		end  string
	}{
		{"the host closes", endHost},
		{"the run closes", endRun},
	} {
		t.Run(c.what, func(t *testing.T) {
			ec := newEndsCase(t, nil)
			conn, r := ec.connect(toHost[:behind])
			host := ec.accept()

			hostDone := make(chan error, 1)
			go func() {
				got := make([]byte, up)
				if _, err := io.ReadFull(host, got); err != nil {
					hostDone <- err
					return
				}
				if !bytes.Equal(got, toHost) {
					hostDone <- fmt.Errorf("the host read other bytes")
					return
				}
				if _, err := host.Write(toRun); err != nil {
					hostDone <- err
					return
				}
				if c.end == endHost {
					host.Close()
				}
				hostDone <- nil
			}()
			if _, err := conn.Write(toHost[behind:]); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, down)
			if _, err := io.ReadFull(r, got); err != nil || !bytes.Equal(got, toRun) {
				t.Fatalf("the run read %v, the same bytes %v", err, bytes.Equal(got, toRun))
			}
			if err := <-hostDone; err != nil {
				t.Fatal(err)
			}
			if c.end == endHost {
				if rest, err := io.ReadAll(r); err != nil || len(rest) != 0 {
					t.Fatalf("after the host closed, the run read %d bytes more, %v", len(rest), err)
				}
			} else {
				conn.Close()
			}

			end := ec.end()
			if end.Sent != up || end.Received != down || end.End != c.end {
				t.Errorf("the end line is %+v, want sent %d, received %d, end %s", end, up, down, c.end)
			}
		})
	}
}

// TestStop: a stop, as on SIGTERM, ends an open connection and writes its
// line with the exact counts, well inside its bound.
func TestStop(t *testing.T) {
	ec := newEndsCase(t, nil)
	conn, r := ec.connect(nil)
	host := ec.accept()
	go func() {
		buf := make([]byte, 5)
		if _, err := io.ReadFull(host, buf); err == nil {
			host.Write([]byte("answer!"))
		}
	}()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 7)
	if _, err := io.ReadFull(r, got); err != nil || string(got) != "answer!" {
		t.Fatalf("the run read %q, %v", got, err)
	}

	start := time.Now()
	ec.cancel()
	if err := <-ec.served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	ec.served <- nil // for the cleanup
	if took := time.Since(start); took > stopWait/2 {
		t.Errorf("the stop took %s", took)
	}
	end := ec.end()
	if end.Sent != 5 || end.Received != 7 || end.End != endStopped {
		t.Errorf("the end line is %+v, want sent 5, received 7, end stopped", end)
	}
	if _, err := r.ReadByte(); err == nil {
		t.Error("the run's side is still open after the stop")
	}
}

// TestStopDuringDial: a connection whose dial finishes after the stop began
// is closed at once and gets its line with the end stopped.
func TestStopDuringDial(t *testing.T) {
	release := make(chan struct{})
	ec := newEndsCase(t, release)
	conn, err := net.Dial("tcp", ec.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT public.example:443 HTTP/1.1\r\nHost: public.example:443\r\n\r\n")
	<-ec.dialed

	ec.cancel()
	for deadline := time.Now().Add(5 * time.Second); ; {
		ec.w.open.Lock()
		stopping := ec.w.stopping
		ec.w.open.Unlock()
		if stopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the way out did not begin to stop")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-ec.served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	ec.served <- nil // for the cleanup
	end := ec.end()
	if end.Sent != 0 || end.Received != 0 || end.End != endStopped {
		t.Errorf("the end line is %+v, want 0 bytes each way and the end stopped", end)
	}
}
