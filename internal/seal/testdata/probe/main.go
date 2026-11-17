// Command probe runs as a run's container in the seal's test and reports what it
// finds from inside, as JSON on standard output. It judges nothing: the test
// does. It reads what to try from the file named by its one argument.
//
// It is built for Linux only.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Config is what the test asks the probe to try.
type Config struct {
	HTTP   []string // URLs to GET, each through the proxy
	Dial   []string // TCP addresses, host:port, to connect to
	Lookup []string // names to resolve
	Files  []string // files to read
}

// Report is what the probe found.
type Report struct {
	HTTP    []Result
	Dial    []Result
	Lookup  []Result
	Sockets []Result
	Files   []Result
	Init    string // process 1's program, from /proc/1/cmdline, with links resolved
	Env     []string
}

// Result is one attempt. Errno is the error number of a failed system call, 0
// if there was none.
type Result struct {
	What  string
	Value string `json:",omitempty"`
	Error string `json:",omitempty"`
	Errno int    `json:",omitempty"`
}

func main() {
	var config Config
	if data, err := os.ReadFile(os.Args[1]); err != nil {
		fail(err)
	} else if err := json.Unmarshal(data, &config); err != nil {
		fail(err)
	}

	r := Report{Env: os.Environ()}
	for _, url := range config.HTTP {
		r.HTTP = append(r.HTTP, get(url))
	}
	for _, address := range config.Dial {
		conn, err := net.DialTimeout("tcp", address, 3*time.Second)
		if err == nil {
			conn.Close()
		}
		r.Dial = append(r.Dial, result(address, "connected", err))
	}
	for _, name := range config.Lookup {
		addrs, err := net.LookupHost(name)
		r.Lookup = append(r.Lookup, result(name, strings.Join(addrs, " "), err))
	}
	for _, s := range []struct {
		what                   string
		family, kind, protocol int
	}{
		{"AF_PACKET/SOCK_RAW", syscall.AF_PACKET, syscall.SOCK_RAW, 0x0300}, // ETH_P_ALL in network byte order
		{"AF_INET/SOCK_RAW", syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP},
		{"AF_INET6/SOCK_RAW", syscall.AF_INET6, syscall.SOCK_RAW, syscall.IPPROTO_ICMPV6},
	} {
		fd, err := syscall.Socket(s.family, s.kind, s.protocol)
		if err == nil {
			syscall.Close(fd)
		}
		r.Sockets = append(r.Sockets, result(s.what, "opened", err))
	}
	for _, file := range config.Files {
		data, err := os.ReadFile(file)
		r.Files = append(r.Files, result(file, string(data), err))
	}
	if cmdline, err := os.ReadFile("/proc/1/cmdline"); err == nil {
		program, _, _ := strings.Cut(string(cmdline), "\x00")
		if resolved, err := filepath.EvalSymlinks(program); err == nil {
			program = resolved
		}
		r.Init = program
	}

	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		fail(err)
	}
}

// get sends a GET with credentials of its own, which the proxy must replace,
// and returns the upstream's answer. The proxy may have only just started, so it
// tries for up to ten seconds.
//
// The upstream echoes the headers it saw. Their values are reported by their
// SHA-256, so that the credential the proxy put there appears nowhere in the
// probe's output.
func get(url string) Result {
	client := &http.Client{Timeout: 5 * time.Second}
	var err error
	for range 40 {
		var body map[string]string
		if body, err = getOnce(client, url); err == nil {
			for _, header := range []string{"authorization", "x-api-key"} {
				sum := sha256.Sum256([]byte(body[header]))
				body[header] = "sha256:" + hex.EncodeToString(sum[:])
			}
			out, _ := json.Marshal(body)
			return Result{What: url, Value: string(out)}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return result(url, "", err)
}

func getOnce(client *http.Client, url string) (map[string]string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer the-probes-own")
	req.Header.Set("X-Api-Key", "the-probes-own")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status + ": " + string(data))
	}
	var body map[string]string
	return body, json.Unmarshal(data, &body)
}

func result(what, value string, err error) Result {
	if err == nil {
		return Result{What: what, Value: value}
	}
	r := Result{What: what, Error: err.Error()}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		r.Errno = int(errno)
	}
	return r
}

func fail(err error) {
	os.Stderr.WriteString("probe: " + err.Error() + "\n")
	os.Exit(2)
}
