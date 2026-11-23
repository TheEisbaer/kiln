package tick

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/TheEisbaer/kiln/internal/run"
)

// The comparison before the report (ADR 0035, ADR 0036, rule 2). Every core,
// started by hand or by the launcher, on probation or not, compares the
// store, the container engine and the Git host once, with its screen served,
// before it acts; only after the first comparison that passes does it report
// healthy and start acting. All of it only reads: a core before its report
// changes nothing outside the store (rule 3), and does not know whether it
// is on probation.

// The limits of the comparison. Only the test changes them.
var (
	// compareLimit bounds the reading of main at the Git host.
	compareLimit = 10 * time.Second
	// screenLimit bounds the request to the core's own screen.
	screenLimit = 5 * time.Second
)

// compareUntilPassed compares until a comparison passes, every interval, and
// reports false when the context was cancelled first. An error is printed
// once per text.
func (c *core) compareUntilPassed(ctx context.Context, every time.Duration) bool {
	var printed, hostPrinted string
	for {
		err := c.compare(ctx, &hostPrinted)
		if ctx.Err() != nil {
			return false
		}
		if err == nil {
			return true
		}
		if text := run.Printable(err.Error()); text != printed {
			fmt.Fprintln(c.errOut, "kiln core: the comparison before the report:", text)
			printed = text
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(every):
		}
	}
}

// compare is one comparison: the store, the container engine, main at the
// Git host and the core's own screen, each only read. A Git host that does
// not answer is printed, once per text in hostPrinted, and does not fail the
// comparison.
func (c *core) compare(ctx context.Context, hostPrinted *string) error {
	if _, err := c.store.Board(); err != nil {
		return fmt.Errorf("the store: %w", err)
	}
	if _, err := c.store.Waiting(); err != nil {
		return fmt.Errorf("the store: %w", err)
	}
	inFlight, err := c.store.InFlightRuns()
	if err != nil {
		return fmt.Errorf("the store: %w", err)
	}
	if _, err := c.store.Paused(); err != nil {
		return fmt.Errorf("the store: %w", err)
	}
	if _, err := c.engine.ListRuns(ctx, c.instance); err != nil {
		return fmt.Errorf("the container engine: %w", err)
	}
	for _, id := range inFlight {
		if _, err := c.engine.InspectRun(ctx, id); err != nil {
			return fmt.Errorf("the container engine, run %s: %w", id, err)
		}
	}
	text := ""
	if err := readMain(ctx, c.Repo); err != nil {
		text = run.Printable(err.Error())
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if text != "" && text != *hostPrinted {
		fmt.Fprintf(c.errOut, "kiln core: the comparison before the report: main is not read at the Git host, which does not hold the report: %s\n", text)
	}
	*hostPrinted = text
	if c.screen != "" {
		if err := checkScreen(ctx, c.screen); err != nil {
			return fmt.Errorf("the screen: %w", err)
		}
	}
	return nil
}

// readMain reads main at the copy's origin with git ls-remote, within
// compareLimit and with the protections of fetchMain, and writes nothing. A
// copy without origin has nothing to read.
func readMain(ctx context.Context, repo string) error {
	has, err := hasOrigin(ctx, repo)
	if err != nil || !has {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, compareLimit)
	defer cancel()
	cmd, err := hostGit(ctx, repo, "ls-remote", "--quiet", "origin", "refs/heads/main")
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("git ls-remote did not end within %s", compareLimit)
		}
		return fmt.Errorf("git ls-remote: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// checkScreen asks the core's own screen for the board, within screenLimit,
// and wants 200 with a JSON object.
func checkScreen(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, screenLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/board", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /api/board answered %s", resp.Status)
	}
	var board map[string]json.RawMessage
	if err := json.Unmarshal(body, &board); err != nil {
		return fmt.Errorf("GET /api/board did not answer a JSON object: %w", err)
	}
	return nil
}

// hexCommit is a commit's full hash.
var hexCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// versionOf is the version the board says: the commit the program was built
// from, or nothing for a value that is not one.
func versionOf(v string) string {
	if hexCommit.MatchString(v) {
		return v
	}
	return ""
}
