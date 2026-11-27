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
// and reads its result from the last line it prints: pending, passed, failed,
// closed or review. A failed check for the commit that is still published sends the
// card back to implement by the rule, as a send-back (ADR 0012, rule 2; ADR
// 0054), with the check's output in the entry, which the implement run is
// told (told). closed says that the commit's pull request was closed without
// a merge: the card goes to review, needing you, by the Git host, and it is no
// send-back. review says that a person commented on the commit's pull request
// at or after the card's feedback mark, which the command is told as
// KILN_SINCE: the card goes to review, needing you, by the Git host, with the
// feedback in the entry, and it is no send-back either (ADR 0028); the
// developer's send-back tells the implement run the feedback (told). pending
// and passed change nothing, and anything else is said on
// the core's error output and changes nothing either.
//
// What the core knows of the checks is in memory only, as a guard and a
// pace (ADR 0035): a core started again checks at once. Only the feedback
// mark is kept, on the card's publication, since feedback, unlike a failed
// check, would not come again on its own.

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
	checkClosed  = "closed"
	checkReview  = "review"
)

// envSince is the variable that tells the check result command the card's
// feedback mark, besides envCommit.
const envSince = "KILN_SINCE"

// sinceLayout is how the feedback mark is told: in UTC, cut to the second, in
// the form the Git host's since takes, so that two times compare as strings.
const sinceLayout = "2006-01-02T15:04:05Z"

// sinceOf is the feedback mark as the command is told it: the epoch for a card
// without one.
func sinceOf(mark time.Time) string {
	if mark.IsZero() {
		mark = time.Unix(0, 0)
	}
	return mark.UTC().Truncate(time.Second).Format(sinceLayout)
}

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
	c.mu.Lock()
	read := st.last
	c.mu.Unlock()
	var result, output, failure string
	if pub, err := c.store.Publication(id); err != nil {
		failure = "the card's publication is not read: " + err.Error()
	} else {
		result, output, failure = c.runCheckResult(ctx, st.repo.CheckResultCommand, st.commit, sinceOf(pub.FeedbackSince))
	}
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
	switch result {
	case checkFailed:
		if err := c.checkFailed(ctx, st.repo, id, st.commit, output); err != nil && ctx.Err() == nil {
			fmt.Fprintf(c.errOut, "kiln core: card %d: the failed check is not acted on: %s\n", id, run.Printable(err.Error()))
		}
	case checkClosed:
		if err := c.checkClosed(ctx, st.repo, id, st.commit); err != nil && ctx.Err() == nil {
			fmt.Fprintf(c.errOut, "kiln core: card %d: the closed pull request is not acted on: %s\n", id, run.Printable(err.Error()))
		}
	case checkReview:
		if err := c.checkReview(ctx, st.repo, id, st.commit, read, output); err != nil && ctx.Err() == nil {
			fmt.Fprintf(c.errOut, "kiln core: card %d: the reviewer's feedback is not acted on: %s\n", id, run.Printable(err.Error()))
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
	if err := c.mergeFirst(ctx, repo, id, commit); err != nil {
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

// mergeFirst brings the main of the card's repo up to date and closes the card
// as merged when its commit, merged into main, changes nothing: a merge wins
// over any result of the check. A card that is no longer at the Git host is
// left as it is.
func (c *core) mergeFirst(ctx context.Context, repo settings.Repo, id int64, commit string) error {
	c.refreshMain(ctx, repo, fetchLimit+10*time.Second)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := c.closeIfMerged(ctx, repo, id, commit); err != nil && !errors.Is(err, store.ErrRefused) {
		return err
	}
	return nil
}

// checkClosed acts on a pull request closed without a merge for a card's
// commit: as for a failed check, a change that is in main already closes the
// card as merged, and otherwise the card goes to review, needing you, by the
// Git host, with the reason store.ClosedReason, as no send-back. A card that
// is no longer at the Git host with this commit is left as it is.
func (c *core) checkClosed(ctx context.Context, repo settings.Repo, id int64, commit string) error {
	if err := c.mergeFirst(ctx, repo, id, commit); err != nil {
		return err
	}
	if _, err := c.store.PullRequestClosed(id, commit); err != nil {
		if errors.Is(err, store.ErrRefused) {
			return nil // merged, moved by the developer, or published anew meanwhile
		}
		return err
	}
	fmt.Fprintf(c.out, "card %d: %s\n", id, store.ClosedReason)
	return nil
}

// checkReview acts on a reviewer's feedback on the pull request of a card's
// commit, read at the time read: as for a closed pull request, a change that
// is in main already closes the card as merged, and otherwise the card goes to
// review, needing you, by the Git host, with the reason store.ReviewReason
// and, after an empty line, the feedback fenced, as no send-back; its feedback
// mark becomes read. The feedback goes on no output. A card that is no longer
// at the Git host with this commit is left as it is.
func (c *core) checkReview(ctx context.Context, repo settings.Repo, id int64, commit string, read time.Time, feedback string) error {
	if err := c.mergeFirst(ctx, repo, id, commit); err != nil {
		return err
	}
	reason := store.ReviewReason
	if feedback != "" {
		reason += "\n\n" + fenced(feedback, "text")
	}
	if _, err := c.store.ReviewFeedback(id, commit, read, reason); err != nil {
		if errors.Is(err, store.ErrRefused) {
			return nil // merged, moved by the developer, or published anew meanwhile
		}
		return err
	}
	fmt.Fprintf(c.out, "card %d: %s\n", id, store.ReviewReason)
	return nil
}

// runCheckResult runs command, the check result command of a card's repo, for
// a commit: with sh -c, in the data directory, with no standard input, for at
// most checkLimit, its session killed with it, and with the environment of the
// core without any name that begins with KILN_, and KILN_COMMIT, the commit,
// and KILN_SINCE, the card's feedback mark as sinceOf gives it. Its standard
// error is only said, in a failure. It returns the result and, for a failed
// check or a reviewer's feedback, its output (see readCheck), or the failure.
func (c *core) runCheckResult(ctx context.Context, command, commit, since string) (result, output, failure string) {
	cctx, cancel := context.WithTimeout(ctx, checkLimit)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = c.Data
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "KILN_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, envCommit+"="+commit, envSince+"="+since)
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
// its last line, trimmed, is the result, pending, passed, failed, closed or
// review. With closed what comes before it is dropped. With failed, and with
// review, whose output is the feedback, the output is what comes before it,
// trimmed and made printable, and
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
	case checkPending, checkPassed, checkClosed:
		return last, "", ""
	case checkFailed, checkReview:
	default:
		if len(last) > maxLastLine {
			end := maxLastLine
			for !utf8.RuneStart(last[end]) {
				end--
			}
			last = last[:end] + "…"
		}
		return "", "", "the check result command's last line is not pending, passed, failed, closed or review: " + last
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
	return last, output, ""
}

// readAgain reads the comments made since Kiln's first look when the developer
// sent a card back to implement after a reviewer's feedback (ADR 0028): the
// check result command runs once more for the card's commit, with the card's
// feedback mark, and what it prints on review is written as the Git host's
// entry LaterFeedbackReason, which the implement run is told after the first
// feedback. It acts once per send-back in a core's life, whatever comes of it.
// Another result than review means no comments since the mark and is said
// nowhere; a failure, closed, a repo without the command or a publication
// without a commit is said once on the error output, and the run is told the
// first feedback only. refused says that the card moved meanwhile.
func (c *core) readAgain(ctx context.Context, repo settings.Repo, card store.Card) (refused bool, err error) {
	entry, ok, err := c.store.SentBackAfterFeedback(card.ID)
	if err != nil || !ok {
		return false, err
	}
	if c.readTried == nil {
		c.readTried = map[int64]int64{}
	}
	if c.readTried[card.ID] == entry.Number {
		return false, nil
	}
	c.readTried[card.ID] = entry.Number
	say := func(why string) {
		fmt.Fprintln(c.errOut, run.Printable(fmt.Sprintf("kiln core: card %d: the comments since Kiln's first look are not read, "+
			"so the implement run is told only the feedback read then: %s", card.ID, why)))
	}
	pub, err := c.store.Publication(card.ID)
	if err != nil {
		say("the card's publication is not read: " + err.Error())
		return false, nil
	}
	switch {
	case repo.CheckResultCommand == "":
		say("the card's repo has no check result command")
		return false, nil
	case pub.Address == "" || !commitName.MatchString(pub.Commit):
		say("the card's publication has no published commit")
		return false, nil
	}
	read := time.Now()
	result, output, failure := c.runCheckResult(ctx, repo.CheckResultCommand, pub.Commit, sinceOf(pub.FeedbackSince))
	if ctx.Err() != nil {
		return false, ctx.Err() // an interrupt: nothing is written
	}
	switch {
	case failure != "":
		say(failure)
	case result == checkClosed:
		say("the check result command printed closed")
	case result == checkReview:
		reason := store.LaterFeedbackReason
		if output != "" {
			reason += "\n\n" + fenced(output, "text")
		}
		if _, err := c.store.LaterFeedback(card.ID, entry.Number, read, reason); err != nil {
			if errors.Is(err, store.ErrRefused) {
				return true, nil
			}
			say("the comments are not kept: " + err.Error())
			return false, nil
		}
		fmt.Fprintf(c.out, "card %d: %s\n", card.ID, store.LaterFeedbackReason)
	}
	return false, nil
}
