package tick

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/seal"
	"github.com/TheEisbaer/kiln/internal/store"
)

// The proxy and its credential (ADR 0040). The core keeps the proxy at its
// start, and gives it a credential before the first tick: it runs the
// developer's credential command and pushes what the command printed to the
// proxy through the seal. It renews it beside the tick, so that a slow command
// never holds the tick up: every renewEvery after the last success, and at
// once when the output of a run it follows shows that the upstream refused
// the credential, at most once in refusedEvery. A credential that is pushed
// replaces the one before, also when it is the same.
//
// The credential is unavailable when the command fails, runs out of time or
// prints nothing, or when the push fails or runs out of time. The instance is
// then paused (ADR 0016, rule 3): the pause is the instance's state in the
// store, with its history (ADR 0055), which the tick reads before it starts a
// run and the board shows, so nothing of it is kept only in memory
// (ADR 0035). The core says so on its terminal when the store's state
// changes, so once per reason also across a restart, starts no run, leaves
// the runs in flight alone, and tries again every retryEvery; the first
// success ends the pause. What the core keeps in memory, the times of the
// renewal, only times the next attempt: a core started again renews at its
// start.
//
// The command's standard output is the credential: it goes to the proxy and
// nowhere else, never to the terminal, the board or the store. What is said
// of a failure is the command's standard error, made printable and cut.

// The times of renewal. Only the test changes them.
var (
	// renewEvery is the time from a success to the next renewal, Claude
	// Code's own interval for a key helper.
	renewEvery = 5 * time.Minute
	// retryEvery is the time from a failure to the next attempt.
	retryEvery = 30 * time.Second
	// refusedEvery is the least time between two renewals that a refused
	// credential asks for.
	refusedEvery = 10 * time.Second
	// credentialLimit bounds one run of the credential command.
	credentialLimit = 30 * time.Second
	// pushLimit bounds one push to the proxy, so that an engine that hangs
	// holds neither the core's start nor the renewal.
	pushLimit = 30 * time.Second
)

// maxCredential is the most the credential command may print, as the proxy
// takes no longer credential (cmd/proxy).
const maxCredential = 64 << 10

// unavailable begins the reason of a pause for want of a credential.
const unavailable = "the Claude credential is unavailable: "

// clock is the time of the renewal. Only the test gives a core another.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// realClock is the clock of a core.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// credentialState is what the renewal keeps in memory, under credMu.
type credentialState struct {
	failing   bool      // the last attempt failed, or its outcome was not written
	refusedAt time.Time // when a refused credential last asked for a renewal
}

// checkCredential refuses a configuration without an upstream that is an
// http or https URL, or without a credential command.
func checkCredential(cfg Config) error {
	if cfg.CredentialCommand == "" {
		return errors.New("the core needs a credential command")
	}
	u, err := url.Parse(cfg.Upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("the upstream %q is not an http or https URL", cfg.Upstream)
	}
	return nil
}

// keepProxy keeps the proxy, or starts or replaces it, and gives it a
// credential. A proxy that cannot be kept ends the core; a credential that is
// unavailable pauses the instance, and the core goes on.
func (c *core) keepProxy(ctx context.Context) error {
	kept, err := c.engine.KeepProxy(ctx, seal.Proxy{Name: c.Proxy, Image: c.ProxyImage, Network: c.ProxyNetwork, Upstream: c.Upstream})
	if err != nil {
		return fmt.Errorf("the proxy %s: %w", c.Proxy, err)
	}
	if kept {
		fmt.Fprintf(c.out, "kiln core: the proxy %s runs, and is kept\n", c.Proxy)
	} else {
		fmt.Fprintf(c.out, "kiln core: the proxy %s is started\n", c.Proxy)
	}
	c.renew(ctx)
	return nil
}

// renewal renews the credential until the context is cancelled: renewEvery
// after a success, retryEvery after a failure, and at once when asked to by
// credentialRefused.
func (c *core) renewal(ctx context.Context) {
	for {
		c.credMu.Lock()
		wait := renewEvery
		if c.cred.failing {
			wait = retryEvery
		}
		c.credMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-c.clock.After(wait):
		case <-c.renewNow:
		}
		c.renew(ctx)
	}
}

// credentialRefused asks for a renewal at once, at most once in
// refusedEvery. A follower calls it for every event of a run's output that
// says the upstream refused the credential.
func (c *core) credentialRefused() {
	c.credMu.Lock()
	defer c.credMu.Unlock()
	now := c.clock.Now()
	if !c.cred.refusedAt.IsZero() && now.Sub(c.cred.refusedAt) < refusedEvery {
		return
	}
	c.cred.refusedAt = now
	select {
	case c.renewNow <- struct{}{}:
		fmt.Fprintln(c.out, "kiln core: the upstream refused the Claude credential in a run; it is renewed now")
	default: // one is asked for already
	}
}

// renew runs the credential command, pushes what it printed to the proxy
// within pushLimit, and pauses the instance, or ends its pause, by the
// outcome. The terminal says so only when the store's state changed, so that
// a reason is said once, also across a restart of the core; a store that
// cannot be written counts as a failure, and is tried again after
// retryEvery. An interrupt changes nothing.
func (c *core) renew(ctx context.Context) {
	credential, why := c.credential(ctx)
	if why == "" {
		pctx, cancel := context.WithTimeout(ctx, pushLimit)
		err := c.engine.PushCredential(pctx, c.Proxy, credential)
		timedOut := errors.Is(pctx.Err(), context.DeadlineExceeded)
		cancel()
		switch {
		case err != nil && timedOut && ctx.Err() == nil:
			// The engine's own text then varies from one attempt to the next.
			why = fmt.Sprintf("the credential could not be given to the proxy: the push did not end within %s", pushLimit)
		case err != nil:
			why = "the credential could not be given to the proxy: " + err.Error()
		}
	}
	if ctx.Err() != nil {
		return
	}
	reason := ""
	if why != "" {
		reason = run.Printable(unavailable + why)
	}
	changed, err := setPaused(c.store, reason)
	c.credMu.Lock()
	c.cred.failing = reason != "" || err != nil
	c.credMu.Unlock()
	switch {
	case err != nil:
		fmt.Fprintln(c.errOut, "kiln core: the pause cannot be written, and the credential is renewed again in", retryEvery.String()+":", run.Printable(err.Error()))
	case changed && reason != "":
		fmt.Fprintf(c.errOut, "kiln core: paused: %s; no run starts, and the credential command is run again every %s\n", reason, retryEvery)
	case changed:
		fmt.Fprintln(c.out, "kiln core: the Claude credential is available again; runs start")
	}
	at("renewed")
}

// setPaused writes the instance's pause. Only the test changes it.
var setPaused = (*store.Store).SetPaused

// credential runs the credential command with sh -c, in the data directory,
// with no standard input, for at most credentialLimit, its process group
// ended with it, and returns what it printed to standard output, trimmed, or
// why there is no credential: its exit status and its standard error, made
// printable and cut, or that it ran out of time, printed nothing or printed
// too much.
func (c *core) credential(ctx context.Context) (credential, why string) {
	ctx, cancel := context.WithTimeout(ctx, credentialLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", c.CredentialCommand)
	cmd.Dir = c.Data
	detach(cmd)
	stdout, stderr := &capped{max: maxCredential}, &tail{max: 4 * maxReason}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", fmt.Sprintf("the credential command did not end within %s", credentialLimit)
	case err != nil:
		why = "the credential command failed: " + err.Error()
		if said := said(stderr); said != "" {
			why += ": " + said
		}
		return "", why
	case stdout.over:
		return "", fmt.Sprintf("the credential command printed more than %d KiB", maxCredential>>10)
	}
	credential = strings.TrimSpace(string(stdout.data))
	if credential == "" {
		return "", "the credential command printed nothing"
	}
	return credential, ""
}

// capped keeps the first max bytes written to it, and notes whether more
// came.
type capped struct {
	max  int
	data []byte
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.max - len(c.data); n > room {
		c.over = true
		p = p[:max(room, 0)]
	}
	c.data = append(c.data, p...)
	return n, nil
}
