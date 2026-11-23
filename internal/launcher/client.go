package launcher

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
)

// Client is a core's end of its socket to the launcher.
type Client struct {
	mu   sync.Mutex
	f    *os.File
	gone chan struct{}
}

// FromEnv is the core's end of the socket that the launcher named by
// KILN_LAUNCHER, or nil without the variable, when the core runs without a
// launcher. It is the first thing a core does: it makes file descriptor 3
// close-on-exec and unsets the variable, so that no child of the core sees
// either. Any value but 3 is an error.
func FromEnv() (*Client, error) {
	v, ok := os.LookupEnv(launcherVar)
	if !ok {
		return nil, nil
	}
	if v != "3" {
		return nil, fmt.Errorf("%s is %q; only kiln launch sets it, to 3", launcherVar, v)
	}
	syscall.CloseOnExec(coreFD)
	if err := os.Unsetenv(launcherVar); err != nil {
		return nil, err
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(coreFD, &st); err != nil {
		return nil, fmt.Errorf("%s is 3, and file descriptor 3 is not open: %w", launcherVar, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
		return nil, fmt.Errorf("%s is 3, and file descriptor 3 is not a socket", launcherVar)
	}
	// Non-blocking, so that the read below goes through Go's poller.
	if err := syscall.SetNonblock(coreFD, true); err != nil {
		return nil, err
	}
	c := &Client{f: os.NewFile(coreFD, "the launcher's socket"), gone: make(chan struct{})}
	go func() {
		defer close(c.gone)
		buf := make([]byte, 512)
		for {
			// The launcher writes nothing; a read ends only when it is gone.
			if _, err := c.f.Read(buf); err != nil {
				return
			}
		}
	}()
	return c, nil
}

// Report writes the core's report, healthy, to the launcher.
func (c *Client) Report() error { return c.write("healthy") }

// AskReplace asks the launcher to replace the core by the program at path,
// an absolute path. The core exits after it.
func (c *Client) AskReplace(path string) error {
	if strings.ContainsAny(path, "\n\x00") {
		return errors.New("the path of a program has no new line")
	}
	return c.write("replace " + path)
}

// Gone is closed once the launcher is gone: its end of the socket is closed.
func (c *Client) Gone() <-chan struct{} { return c.gone }

func (c *Client) write(line string) error {
	if len(line)+1 > maxLine {
		return fmt.Errorf("a line to the launcher is at most %d bytes", maxLine)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.f.Write([]byte(line + "\n")); err != nil {
		return fmt.Errorf("the launcher: %w", err)
	}
	return nil
}
