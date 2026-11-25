package tick

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/settings"
	"github.com/TheEisbaer/kiln/internal/store"
)

// The check on the Git host. For every card at the Git host with a pull
// request, a checker beside the tick, like a publisher and counted in no
// limit, runs the developer's check result command with the published commit
// and reads its result from the last line it prints: pending, passed or
// failed. A failed check for the commit that is still published sends the
// card back to implement by the rule, as a send-back (ADR 0012, rule 2; ADR
// 0054), with the check's output in the entry, which the implement run is
// told (told). pending and passed change nothing, and anything else is said
// on the core's error output and changes nothing either.
//
// What the core knows of the checks is in memory only, as a guard and a
// pace (ADR 0035): a core started again checks at once.

// The times of the check. Only the test changes them.
var (
	// checkEvery is the least time between two checks of one card's commit.
	checkEvery = 2 * time.Minute
	// checkLimit bounds one run of the check result command.
	checkLimit = time.Minute
)

// The results the check result command prints as its last line.
const (
	checkPending = "pending"
	checkPassed  = "passed"
	checkFailed  = "failed"
)

// maxCheckOutput is the most of the check's output that is kept, its end,
// from a line start.
const maxCheckOutput = 16 << 10

// checkCut is the first line of a check's output of which only the end is
// kept.
const checkCut = "[… cut: only the end of the check's output is kept]"

// maxLastLine is the most bytes of a last line that is not a result which are
// said.
const maxLastLine = 200

// commitName is the name of a commit as commitTree makes it, the only text the
// check result command is told.
var commitName = regexp.MustCompile(`^[0-9a-f]{40}$`)

// checkState is what the core knows of the checks of one card's published
// commit: the card's repo, the commit, when its last check started, whether a
// checker runs for it, and the text last said on the error output about it.
type checkState struct {
	repo    settings.Repo
	commit  string
	last    time.Time
	running bool
	said    string
}

// startCheckers starts a checker for each card of a repo at the Git host whose
// check is due, when the repo has a check result command: one that has none
// running, whose commit this core sees for the first time or was checked
// checkEvery ago. The state of a card of the repo that is no longer at the Git
// host, or whose commit changed, is dropped once no checker runs for it; the
// other repos' states are left to their own rounds.
func (c *core) startCheckers(ctx context.Context, repo settings.Repo, published map[int64]string) {
	if repo.CheckResultCommand == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, st := range c.checking {
		if st.repo.Name != repo.Name {
			continue
		}
		if commit, ok := published[id]; !st.running && (!ok || commit != st.commit) {
			delete(c.checking, id)
		}
	}
	for _, id := range slices.Sorted(func(yield func(int64) bool) {
		for id := range published {
			if !yield(id) {
				return
			}
		}
	}) {
		if ctx.Err() != nil {
			return
		}
		commit := published[id]
		st := c.checking[id]
		if st == nil {
			st = &checkState{repo: repo, commit: commit}
			c.checking[id] = st
		}
		if st.commit != commit || st.running || (!st.last.IsZero() && time.Since(st.last) < checkEvery) {
			continue // a checker of an earlier commit still runs, or the check is not due
		}
		if !commitName.MatchString(commit) {
			c.sayCheck(st, fmt.Sprintf("kiln core: card %d: the published commit is not a commit name; it is not checked", id))
			continue
		}
		st.running, st.last = true, time.Now()
		c.followers.Add(1)
		go func() {
			defer c.followers.Done()
			c.check(ctx, id, st)
			c.mu.Lock()
			st.running = false
			c.mu.Unlock()
		}()
	}
}

// sayCheck says a text about a card's check on the error output, made
// printable, unless it was the last said for the card's commit; an empty text
// says nothing and clears the last, so that a later failure is said again.
// The caller holds c.mu.
func (c *core) sayCheck(st *checkState, text string) {
	if text != st.said && text != "" {
		fmt.Fprintln(c.errOut, run.Printable(text))
	}
	st.said = text
}

// check runs the check result command once for a card's commit and acts on
// its result.
func (c *core) check(ctx context.Context, id int64, st *checkState) {
	result, output, failure := c.runCheckResult(ctx, st.repo.CheckResultCommand, st.commit)
	if ctx.Err() != nil {
		return // an interrupt: the next core checks again
	}
	c.mu.Lock()
	if failure != "" {
		c.sayCheck(st, fmt.Sprintf("kiln core: card %d: %s", id, failure))
	} else {
		c.sayCheck(st, "")
	}
	c.mu.Unlock()
	if result == checkFailed {
		if err := c.checkFailed(ctx, st.repo, id, st.commit, output); err != nil && ctx.Err() == nil {
			fmt.Fprintf(c.errOut, "kiln core: card %d: the failed check is not acted on: %s\n", id, run.Printable(err.Error()))
		}
	}
	at("checked")
}

// checkFailed acts on a failed check for a card's commit: the main of the
// card's repo is brought up to date, a change that is in main already closes the card as merged, and
// otherwise the card goes to implement by the rule, as a send-back, with the
// check's output in the entry; to implement waiting for the developer when it
// was sent back twice already. A card that is no longer at the Git host with
// this commit is left as it is.
func (c *core) checkFailed(ctx context.Context, repo settings.Repo, id int64, commit, output string) error {
	c.refreshMain(ctx, repo, fetchLimit+10*time.Second)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := c.closeIfMerged(ctx, repo, id, commit); err != nil && !errors.Is(err, store.ErrRefused) {
		return err
	}
	card, err := c.store.Card(id)
	if err != nil {
		return err
	}
	sentence := store.CheckFailedReason + " for " + commit + "; "
	to := implementWaits
	if card.SendBacks >= store.MaxSendBacks {
		to = implementNeedsYou
		sentence += "the card was sent back twice already, so it waits for you"
	} else {
		sentence += fmt.Sprintf("implement runs again by the rule, send-back %d of %d", card.SendBacks+1, store.MaxSendBacks)
	}
	reason := sentence
	if output != "" {
		reason += "\n\n" + fenced(output, "text")
	}
	if _, err := c.store.CheckFailed(id, commit, to, reason); err != nil {
		if errors.Is(err, store.ErrRefused) {
			return nil // merged, moved by the developer, or published anew meanwhile
		}
		return err
	}
	fmt.Fprintf(c.out, "card %d: %s\n", id, sentence)
	return nil
}

// runCheckResult runs command, the check result command of a card's repo, for
// a commit: with sh -c, in the data directory, with no standard input, for at
// most checkLimit, its session killed with it, and with the environment of the
// core without any name that begins with KILN_, and KILN_COMMIT, the commit.
// Its standard error is only said, in a failure. It returns the result and,
// for a failed check, its output (see readCheck), or the failure.
func (c *core) runCheckResult(ctx context.Context, command, commit string) (result, output, failure string) {
	cctx, cancel := context.WithTimeout(ctx, checkLimit)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = c.Data
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "KILN_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, envCommit+"="+commit)
	detach(cmd)
	stdout, stderr := &tail{max: 64 << 10}, &tail{max: 4 * maxReason}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		return "", "", fmt.Sprintf("the check result command did not end within %s", checkLimit)
	case err != nil:
		failure = "the check result command failed: " + err.Error()
		if said := said(stderr); said != "" {
			failure += ": " + said
		}
		return "", "", failure
	}
	return readCheck(stdout.bytes(), stdout.cut)
}

// readCheck reads what the check result command printed on its standard
// output, of which a part at the start was dropped when cut is set: valid
// UTF-8, without a carriage return before a newline, and trimmed at its end,
// its last line, trimmed, is the result, pending, passed or failed. With
// failed the output is what comes before it, trimmed and made printable, and
// only its end when it is longer than maxCheckOutput or was cut: from the
// first line start within the last maxCheckOutput bytes, or the first
// character start when there is none, after a line that says so. Any other
// last line, or none, is the failure.
func readCheck(data []byte, cut bool) (result, output, failure string) {
	s := strings.ReplaceAll(strings.ToValidUTF8(string(data), "�"), "\r\n", "\n")
	s = strings.TrimRightFunc(s, unicode.IsSpace)
	if s == "" {
		return "", "", "the check result command printed nothing"
	}
	before, last := "", s
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		before, last = s[:i], s[i+1:]
	}
	switch last = strings.TrimSpace(last); last {
	case checkPending, checkPassed:
		return last, "", ""
	case checkFailed:
	default:
		if len(last) > maxLastLine {
			end := maxLastLine
			for !utf8.RuneStart(last[end]) {
				end--
			}
			last = last[:end] + "…"
		}
		return "", "", "the check result command's last line is not pending, passed or failed: " + last
	}
	output = strings.TrimSpace(run.Printable(before))
	if len(output) > maxCheckOutput {
		start := len(output) - maxCheckOutput
		if i := strings.IndexByte(output[start-1:], '\n'); i >= 0 {
			start += i
		} else {
			for !utf8.RuneStart(output[start]) {
				start++
			}
		}
		output, cut = output[start:], true
	}
	if cut && output != "" {
		output = checkCut + "\n" + output
	}
	return checkFailed, output, ""
}
