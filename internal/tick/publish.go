package tick

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/store"
)

// Publishing (card 19). A card the developer publishes waits in review for the
// Git host. While it has no pull request's address, a publisher beside the
// tick, like a run's follower and counted in no limit, brings main up to date,
// puts the change onto it as one commit, and runs the developer's publish
// command with it; the command's address is kept with the card. Once it has
// one, the tick looks after every fetch whether the change is in main, and
// closes the card when it is.
//
// Every step looks at what is there and goes on from it (ADR 0035): the commit
// to publish is made the same from the same branch and main, so a publisher
// that a kill stopped is started again by the next core and publishes the
// same commit once more; the command copes with a branch or a pull request
// that exists already.

// The times of publishing. Only the test changes them.
var (
	// fetchEvery is the least time between two fetches of main, and the
	// most while a card waits for the Git host.
	fetchEvery = 30 * time.Second
	// fetchLimit bounds one fetch.
	fetchLimit = 30 * time.Second
	// fetchWait is the most the tick waits for a fetch before it starts runs
	// from the main that is there; the fetch goes on beside it.
	fetchWait = 5 * time.Second
	// publishLimit bounds one run of the publish command.
	publishLimit = 5 * time.Minute
)

// The variables the publish command is told, and nothing else is added.
const (
	envCard        = "KILN_CARD"
	envRepo        = "KILN_REPO"
	envCommit      = "KILN_COMMIT"
	envBranch      = "KILN_BRANCH"
	envTitle       = "KILN_TITLE"
	envTextFile    = "KILN_TEXT_FILE"
	envWorkflowYes = "KILN_WORKFLOW_YES"
)

// maxReason is the most of the command's standard error that a reason keeps.
const maxReason = 2 << 10

// conflictReason begins the reason of a card that publishing sends back to
// implement, followed by the files that collide (ADR 0012).
const conflictReason = "main moved and the change no longer applies"

// workflows is where the Git host keeps the workflow files it runs (ADR 0060).
const workflows = ".github/workflows/"

// The states of publishing.
var (
	atHost           = store.State{Stage: store.Review, WaitsFor: store.Host}
	implementWaits   = store.State{Stage: store.Implement, WaitsFor: store.Run}
	closed           = store.State{Stage: store.Done, WaitsFor: store.Nothing}
	publishNeedsYou  = reviewNeedsYou
	publishFailedFor = store.PublishFailedReason + ": "
)

// mainState is the fetching of main: when the last round began, the round
// under way, if any, which done is closed after, the rounds that have ended,
// and the last failure that was printed, kept so that a failure is printed
// once per text.
type mainState struct {
	last    time.Time
	done    chan struct{}
	rounds  int
	printed string
}

// refreshMain brings main up to date from the copy's origin, if it has one: a
// round is started beside the tick at most once in fetchEvery, and refreshMain
// waits for the round under way, if any, at most wait. A copy without origin
// is used as it is, and a fetch that fails is printed once per text of its
// failure; the main that is there is used either way. It returns the rounds
// that have ended.
func (c *core) refreshMain(ctx context.Context, wait time.Duration) int {
	c.fetchMu.Lock()
	if c.main.done == nil && (c.main.last.IsZero() || time.Since(c.main.last) >= fetchEvery) {
		c.main.last = time.Now()
		done := make(chan struct{})
		c.main.done = done
		c.followers.Add(1)
		go func() {
			defer c.followers.Done()
			defer close(done)
			has, err := hasOrigin(ctx, c.Repo)
			if err == nil && has {
				fctx, cancel := context.WithTimeout(ctx, fetchLimit)
				err = fetchMain(fctx, c.Repo)
				cancel()
			}
			c.fetchMu.Lock()
			defer c.fetchMu.Unlock()
			c.main.done = nil
			if ctx.Err() != nil {
				return
			}
			c.main.rounds++
			if err != nil {
				if text := run.Printable(err.Error()); text != c.main.printed {
					fmt.Fprintf(c.errOut, "kiln core: main is not brought up to date from origin: %s\n", text)
					c.main.printed = text
				}
			} else {
				c.main.printed = ""
			}
		}()
	}
	done := c.main.done
	c.fetchMu.Unlock()
	if done != nil {
		timer := time.NewTimer(wait)
		select {
		case <-done:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	return c.main.rounds
}

// hasOrigin reports whether a repo has a remote origin.
func hasOrigin(ctx context.Context, repo string) (bool, error) {
	cmd := gitCommand(ctx, repo, "config", "--get", "remote.origin.url")
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}

// fetchMain fetches origin's main into the repo's main, replacing it. Nothing
// it starts can ask anything: it runs in a session of its own, with no
// terminal, git never prompts, ssh runs in batch mode and never asks for a
// passphrase by a program either, and when the context ends the whole session
// is killed, ssh included, and its pipes are let go after a moment. The ssh
// command is the developer's GIT_SSH_COMMAND, or the repo's core.sshCommand,
// or ssh, with -o BatchMode=yes after it.
func fetchMain(ctx context.Context, repo string) error {
	ssh := os.Getenv("GIT_SSH_COMMAND")
	if ssh == "" {
		configured, err := gitLine(ctx, repo, "config", "--default", "", "--get", "core.sshCommand")
		if err != nil {
			return err
		}
		ssh = cmp.Or(configured, "ssh")
	}
	cmd := gitCommand(ctx, repo, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head",
		"origin", "+refs/heads/main:refs/heads/main")
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND="+ssh+" -o BatchMode=yes", "SSH_ASKPASS_REQUIRE=never")
	detach(cmd)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("git fetch did not end within %s", fetchLimit)
		}
		return fmt.Errorf("git fetch: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// detach runs a command in a session of its own, with no terminal to read or
// ask on, and kills the whole session when its context ends, letting go of
// its pipes a moment later even if something still holds them.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
}

// atHostRound is what the tick does for the cards at the Git host: main is
// brought up to date when a run may start or a card waits for the Git host,
// and after each round every card with a pull request whose change is in main
// is closed; then a publisher is started for every card the core publishes.
//
// The tick waits for a fetch at most fetchWait, so that a slow origin holds
// no run back for long; a publisher waits for it to its end.
func (c *core) atHostRound(ctx context.Context, starts bool) error {
	toPublish, err := c.store.Publishing()
	if err != nil {
		return err
	}
	published, err := c.store.AtHost()
	if err != nil {
		return err
	}
	if !(starts || len(toPublish) > 0 || len(published) > 0) {
		return c.startPublishers(ctx, toPublish)
	}
	if rounds := c.refreshMain(ctx, fetchWait); rounds != c.checked {
		c.checked = rounds
		ids := slices.Sorted(func(yield func(int64) bool) {
			for id := range published {
				if !yield(id) {
					return
				}
			}
		})
		for _, id := range ids {
			if err := c.closeIfMerged(ctx, id, published[id]); err != nil {
				fmt.Fprintf(c.errOut, "kiln core: card %d: %s\n", id, run.Printable(err.Error()))
			}
		}
	}
	return c.startPublishers(ctx, toPublish)
}

// startPublishers starts a publisher for each card the core publishes, when it
// has a publish command.
func (c *core) startPublishers(ctx context.Context, toPublish []int64) error {
	if c.PublishCommand == "" {
		return nil
	}
	for _, id := range toPublish {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.startPublisher(ctx, id)
	}
	return nil
}

// closeIfMerged closes a card at the Git host whose published commit, merged
// into main, changes nothing: the Git host merged it.
func (c *core) closeIfMerged(ctx context.Context, card int64, commit string) error {
	if commit == "" {
		return nil
	}
	main, err := head(ctx, c.Repo)
	if err != nil {
		return err
	}
	in, err := changesNothing(ctx, c.Repo, main, commit)
	if err != nil || !in {
		return err
	}
	if _, err := c.store.MoveWithReason(card, atHost, closed, store.CauseHost, store.MergedReason); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "card %d: merged\n", card)
	return nil
}

// changesNothing reports whether merging a commit into main changes nothing:
// the merge has no conflict, and its tree is main's.
func changesNothing(ctx context.Context, repo, main, commit string) (bool, error) {
	tree, conflicts, err := mergeTree(ctx, repo, main, commit)
	if err != nil || len(conflicts) > 0 {
		return false, err
	}
	mainTree, err := gitLine(ctx, repo, "rev-parse", "--verify", main+"^{tree}")
	return tree == mainTree, err
}

// startPublisher starts a publisher for a card beside the tick, unless one
// publishes it already. The card is published from now until the publisher
// has returned; an error of it is printed with the card's ID, and the next
// tick starts another.
func (c *core) startPublisher(ctx context.Context, id int64) {
	c.mu.Lock()
	if c.publishing[id] {
		c.mu.Unlock()
		return
	}
	c.publishing[id] = true
	c.mu.Unlock()
	c.followers.Add(1)
	go func() {
		defer c.followers.Done()
		err := c.publish(ctx, id)
		if ctx.Err() == nil {
			// An error of the publishing itself leaves the card under
			// "needs you" too, with its moves, and is not tried again on
			// every tick.
			if err != nil {
				card, rerr := c.store.Card(id)
				if rerr == nil {
					rerr = c.publishFailed(card, err.Error())
				}
				if rerr != nil && !errors.Is(rerr, store.ErrRefused) {
					fmt.Fprintf(c.errOut, "kiln core: card %d: %s; %s\n", id, run.Printable(err.Error()), run.Printable(rerr.Error()))
				}
			}
			at("published")
		}
		c.mu.Lock()
		delete(c.publishing, id)
		c.mu.Unlock()
	}()
}

// publish publishes one card that waits for the Git host without a pull
// request: main is brought up to date; a branch that still holds an earlier
// merge of main with its conflicts, or a change that collides with main,
// sends the card back to implement; a change that is all in main closes it;
// and otherwise the change is put onto main as one commit and the publish
// command is run with it. The command's address is kept with the card, and
// its failure moves the card to "needs you".
func (c *core) publish(ctx context.Context, id int64) error {
	card, err := c.store.Card(id)
	if err != nil {
		return err
	}
	pub, err := c.store.Publication(id)
	if err != nil {
		return err
	}
	if card.State != atHost || pub.Address != "" {
		return nil
	}
	c.refreshMain(ctx, fetchLimit+10*time.Second)
	branch := branchOf(id)
	tip, err := branchTip(ctx, c.Repo, branch)
	if err != nil {
		return err
	}
	if tip == "" {
		return c.publishFailed(card, "the card's branch "+branch+" does not exist")
	}
	main, err := head(ctx, c.Repo)
	if err != nil {
		return err
	}

	// A branch that still holds Kiln's conflict markers, from a merge of
	// main that no run has resolved, or not all of it, goes back as it is.
	if files, err := markedFiles(ctx, c.Repo, tip, id); err != nil || len(files) > 0 {
		if err != nil {
			return err
		}
		return c.toImplement(card, files)
	}
	tree, conflicts, err := mergeTree(ctx, c.Repo, main, tip)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		if err := c.mergeMain(ctx, id, tip, main); err != nil {
			return err
		}
		return c.toImplement(card, conflicts)
	}
	if mainTree, err := gitLine(ctx, c.Repo, "rev-parse", "--verify", main+"^{tree}"); err != nil || tree == mainTree {
		if err != nil {
			return err
		}
		if _, err := c.store.MoveWithReason(id, atHost, closed, store.CauseRule, "everything the change does is in main already"); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "card %d: everything the change does is in main already; it is done\n", id)
		return nil
	}

	// A change to the workflow files is published only with the developer's
	// yes for this very change (ADR 0060).
	touched, err := workflowFiles(ctx, c.Repo, tip)
	if err != nil {
		return err
	}
	yes := len(touched) > 0 && pub.WorkflowYes == tip
	if len(touched) > 0 && !yes {
		return c.publishFailed(card, "the change touches workflow files, and you did not say yes to this change: "+fileNames(touched))
	}

	title, err := c.titleOf(card)
	if err != nil {
		return err
	}
	text, err := c.textOf(card)
	if err != nil {
		return err
	}
	textFile := filepath.Join(c.Data, "publish", fmt.Sprintf("card-%d.md", id))
	if err := writeAtomically(textFile, []byte(text)); err != nil {
		return err
	}
	runID, err := gitLine(ctx, c.Repo, "log", "-1", "--format=%(trailers:key=Kiln-Run,valueonly,separator=%x2C)", tip)
	if err != nil {
		return err
	}
	// The branch on the Git host is named at the first publishing and kept,
	// so that a title that changes later opens no second pull request.
	branchName, err := c.store.PublishBranch(id, publishBranch(id, title))
	if err != nil {
		return err
	}
	commit, err := commitTree(ctx, c.Repo, tree, publishMessage(title, id, runID), []string{main}, []string{tip, main})
	if err != nil {
		return err
	}
	if _, err := gitLine(ctx, c.Repo, "update-ref", "--no-deref", publishRef(id), commit); err != nil {
		return err
	}

	env := []string{
		envCard + "=" + strconv.FormatInt(id, 10),
		envRepo + "=" + c.Repo,
		envCommit + "=" + commit,
		envBranch + "=" + branchName,
		envTitle + "=" + title,
		envTextFile + "=" + textFile,
	}
	if yes {
		env = append(env, envWorkflowYes+"=1")
	}
	at("publish/before-command")
	fmt.Fprintf(c.out, "card %d: publishes %s\n", id, commit)
	address, failure := c.runPublish(ctx, env)
	if ctx.Err() != nil {
		return ctx.Err() // an interrupt: the next core publishes again
	}
	if failure != "" {
		return c.publishFailed(card, failure)
	}
	at("publish/before-keep")
	if _, err := c.store.KeepPullRequest(id, commit, address); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "card %d: published as %s\n", id, run.Printable(address))
	return nil
}

// publishFailed moves a card at the Git host to "needs you", with the reason,
// from where the developer can publish it again.
func (c *core) publishFailed(card store.Card, reason string) error {
	if _, err := c.store.MoveWithReason(card.ID, atHost, publishNeedsYou, store.CauseRule, run.Printable(publishFailedFor+reason)); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "card %d: publishing failed: %s\n", card.ID, run.Printable(reason))
	return nil
}

// toImplement sends a card at the Git host back to implement, because main
// moved and the change no longer applies to it, with the files that collide.
func (c *core) toImplement(card store.Card, files []string) error {
	reason := run.Printable(conflictReason + ": " + fileNames(files))
	if _, err := c.store.MoveWithReason(card.ID, atHost, implementWaits, store.CauseRule, reason); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "card %d: %s\n", card.ID, run.Printable(reason))
	return nil
}

// fileNames are file names for a reason, by commas: at most 20, and how many
// more.
func fileNames(files []string) string {
	const most = 20
	if len(files) <= most {
		return strings.Join(files, ", ")
	}
	return strings.Join(files[:most], ", ") + fmt.Sprintf(" and %d more", len(files)-most)
}

// mergeTree merges two commits without a working tree, as `git merge-tree
// --write-tree` does, with the conflicts in the style diff3, and returns the
// tree it wrote, conflict markers included, and the files that collide, if
// any.
func mergeTree(ctx context.Context, repo, ours, theirs string) (string, []string, error) {
	cmd := gitCommand(ctx, repo, "-c", "merge.conflictStyle=diff3", "merge-tree", "--write-tree", "-z", "--name-only",
		"--no-messages", "--end-of-options", ours, theirs)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return "", nil, fmt.Errorf("git merge-tree on %s: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	fields := strings.Split(stdout.String(), "\x00")
	tree := fields[0]
	if tree == "" {
		return "", nil, fmt.Errorf("git merge-tree on %s printed no tree", repo)
	}
	var files []string
	for _, f := range fields[1:] {
		if f == "" {
			break
		}
		if !slices.Contains(files, f) {
			files = append(files, f)
		}
	}
	if err != nil && len(files) == 0 {
		return "", nil, fmt.Errorf("git merge-tree on %s found a conflict without a file", repo)
	}
	return tree, files, nil
}

// mergeMessage is the subject of Kiln's merge of main into a card's branch.
func mergeMessage(card int64) string {
	return fmt.Sprintf("Merge main into %s\n\nKiln-Card: %d\n", branchOf(card), card)
}

// mergeMain merges main into a card's branch whose change collides with it,
// as one commit with the branch's tip and main as its parents and the merge's
// tree, conflict markers included, and moves the branch to it, guarded by
// its tip. The implement run that follows starts there (told).
func (c *core) mergeMain(ctx context.Context, card int64, tip, main string) error {
	// Merged by the refs, so that the conflict markers name them, and only
	// while the refs are where they were read.
	tree, _, err := mergeTree(ctx, c.Repo, "refs/heads/"+branchOf(card), "refs/heads/main")
	if err != nil {
		return err
	}
	for ref, want := range map[string]string{"refs/heads/" + branchOf(card): tip, "refs/heads/main": main} {
		if at, err := gitLine(ctx, c.Repo, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}"); err != nil || at != want {
			return fmt.Errorf("%s moved while main was merged into the card's branch: %v", ref, err)
		}
	}
	merge, err := commitTree(ctx, c.Repo, tree, mergeMessage(card), []string{tip, main}, []string{tip, main})
	if err != nil {
		return err
	}
	_, err = gitLine(ctx, c.Repo, "update-ref", "--no-deref", "refs/heads/"+branchOf(card), merge, tip)
	return err
}

// The conflict markers of Kiln's merge of main into a card's branch, by
// which its lines are told from any other text: the branch's side opens with
// the branch's ref, and main's side ends with main's.
func markerLines(card int64) []string {
	return []string{"<<<<<<< refs/heads/" + branchOf(card), ">>>>>>> refs/heads/main"}
}

// markedFiles are the files of a commit that still hold a line of Kiln's
// conflict markers for the card, whole, as mergeMain wrote them: a merge of
// main that no run has resolved, or a run that resolved only part of it.
func markedFiles(ctx context.Context, repo, commit string, card int64) ([]string, error) {
	args := []string{"grep", "--no-color", "-I", "-l", "-z", "-E"}
	for _, line := range markerLines(card) {
		args = append(args, "-e", "^"+regexp.QuoteMeta(line)+"$")
	}
	cmd := gitCommand(ctx, repo, append(args, commit, "--")...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && stderr.Len() == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("git grep on %s: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, strings.TrimPrefix(f, commit+":"))
		}
	}
	return files, nil
}

// commitTree makes a commit of a tree on the parents, by Kiln, with the
// message, dated from the latest of the commits dated from, so that the same
// commit made again from the same commits is the same.
func commitTree(ctx context.Context, repo, tree, message string, parents, datedFrom []string) (string, error) {
	var newest int64
	for _, commit := range datedFrom {
		ct, err := gitLine(ctx, repo, "log", "-1", "--format=%ct", "--end-of-options", commit)
		if err != nil {
			return "", err
		}
		n, err := strconv.ParseInt(ct, 10, 64)
		if err != nil {
			return "", fmt.Errorf("the date of %s: %w", commit, err)
		}
		newest = max(newest, n)
	}
	when := strconv.FormatInt(newest, 10) + " +0000"
	args := []string{"commit-tree", "--no-gpg-sign"}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	cmd := gitCommand(ctx, repo, append(args, tree)...)
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME=Kiln", "GIT_AUTHOR_EMAIL=kiln@localhost", "GIT_AUTHOR_DATE="+when,
		"GIT_COMMITTER_NAME=Kiln", "GIT_COMMITTER_EMAIL=kiln@localhost", "GIT_COMMITTER_DATE="+when)
	cmd.Stdin = strings.NewReader(message)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git commit-tree on %s: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// publishRef is the ref that keeps the commit a card's publishing made.
func publishRef(card int64) string { return fmt.Sprintf("refs/kiln/publish/card-%d", card) }

// publishMessage is the message of the commit a card's publishing makes: the
// pull request's title, and the card and the implement run whose save is the
// branch's tip, if it names one, in its body.
func publishMessage(title string, card int64, runID string) string {
	body := fmt.Sprintf("Kiln-Card: %d\n", card)
	if runID = strings.TrimSpace(runID); runID != "" && !strings.ContainsAny(runID, "\n,") {
		body += "Kiln-Run: " + runID + "\n"
	}
	return title + "\n\n" + body
}

// maxCardTitle is the most characters of a card's first line that make a pull
// request's title, when the approved spec names none.
const maxCardTitle = 72

// titleOf is a card's pull request title: the approved spec's line "Pull
// request title:", as the save's message has it; without one, the card's first
// line that is not empty, each control character a space, cut to 72
// characters; and "Card <id>" when that leaves nothing.
func (c *core) titleOf(card store.Card) (string, error) {
	spec, err := c.store.Spec(card.ID)
	if err != nil {
		return "", err
	}
	if t := run.TitleOf(spec.Text); t != "" {
		return t, nil
	}
	for _, line := range strings.Split(card.Text, "\n") {
		line = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
				return ' '
			}
			return r
		}, strings.ToValidUTF8(line, "\uFFFD"))
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > maxCardTitle {
			line = strings.TrimSpace(string([]rune(line)[:maxCardTitle]))
		}
		return line, nil
	}
	return fmt.Sprintf("Card %d", card.ID), nil
}

// textOf is a card's pull request text: the newest that an implement run
// wrote, or the card's text when it has none.
func (c *core) textOf(card store.Card) (string, error) {
	d, err := c.store.Newest(card.ID, store.KindPullRequest)
	switch {
	case err == nil:
		return d.Text, nil
	case errors.Is(err, sql.ErrNoRows):
		return card.Text, nil
	}
	return "", err
}

// maxName is the longest name of a published branch, after card/<id>-.
const maxName = 50

// conventional is the head of a Conventional Commits title, type(scope)!:,
// which the branch's name leaves out.
var conventional = regexp.MustCompile(`^[a-z]+(\([^)]*\))?!?: `)

// publishBranch is the branch a card is published on (ADR 0017's card
// branch): card/<id>-<name>, the name from the title without its head of
// type and scope, in lower case, its words of letters and digits joined by
// hyphens, as many whole words as fit in 50 characters, the first word cut
// to 50 when it alone is longer. A title with no letter or digit gives
// card/<id>.
func publishBranch(card int64, title string) string {
	title = conventional.ReplaceAllString(title, "")
	words := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	name := ""
	for _, w := range words {
		switch {
		case name == "" && len(w) > maxName:
			name = w[:maxName]
		case name == "":
			name = w
		case len(name)+1+len(w) <= maxName:
			name += "-" + w
		default:
			return fmt.Sprintf("card/%d-%s", card, name)
		}
		if len(name) == maxName {
			break
		}
	}
	if name == "" {
		return fmt.Sprintf("card/%d", card)
	}
	return fmt.Sprintf("card/%d-%s", card, name)
}

// workflowFiles are the files under .github/workflows/ that the change of a
// card's branch at commit touches, from where it left main, a file renamed
// counted at both its names.
func workflowFiles(ctx context.Context, repo, commit string) ([]string, error) {
	base, err := gitLine(ctx, repo, "merge-base", "HEAD", commit)
	if err != nil {
		return nil, err
	}
	cmd := gitCommand(ctx, repo, "diff", "--name-only", "--no-renames", "-z", "--no-ext-diff", "--end-of-options", base, commit)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff on %s: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	files := []string{}
	for _, f := range strings.Split(string(out), "\x00") {
		if strings.HasPrefix(f, workflows) || f == ".github" || f == strings.TrimSuffix(workflows, "/") {
			files = append(files, f)
		}
	}
	// A .github or .github/workflows that is a symlink or a submodule, which
	// puts the workflow files somewhere the diff above does not look.
	entries, err := gitLine(ctx, repo, "ls-tree", "-z", "--full-tree", "--end-of-options", commit, ".github", strings.TrimSuffix(workflows, "/"))
	if err != nil {
		return nil, err
	}
	for _, e := range strings.Split(entries, "\x00") {
		meta, path, ok := strings.Cut(e, "\t")
		if mode, _, _ := strings.Cut(meta, " "); ok && (mode == "120000" || mode == "160000") && !slices.Contains(files, path) {
			files = append(files, path)
		}
	}
	return files, nil
}

// runPublish runs the publish command (card 19): with sh -c, in the data
// directory, with no standard input, for at most publishLimit, its process
// group ended with it, and with the environment of the core, from which the
// command's variables are taken out, and env. It returns the pull request's
// address, the last line of its standard output that starts with https://,
// or the failure: its exit status and its standard error, made printable and
// cut to its last 2 KiB.
func (c *core) runPublish(ctx context.Context, env []string) (address, failure string) {
	ctx, cancel := context.WithTimeout(ctx, publishLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", c.PublishCommand)
	cmd.Dir = c.Data
	for _, e := range os.Environ() {
		name, _, _ := strings.Cut(e, "=")
		if !slices.Contains([]string{envCard, envRepo, envCommit, envBranch, envTitle, envTextFile, envWorkflowYes}, name) {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	detach(cmd)
	stdout, stderr := &tail{max: 64 << 10}, &tail{max: 4 * maxReason}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	said := said(stderr)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", fmt.Sprintf("the publish command did not end within %s", publishLimit)
	case err != nil:
		failure = "the publish command failed: " + err.Error()
		if said != "" {
			failure += ": " + said
		}
		return "", failure
	}
	for _, line := range strings.Split(string(stdout.bytes()), "\n") {
		if line = strings.TrimRight(line, "\r"); strings.HasPrefix(line, "https://") {
			address = line
		}
	}
	if address == "" {
		return "", "the publish command printed no address that starts with https://"
	}
	if u, err := url.Parse(address); err != nil || u.Host == "" || len(address) > maxReason ||
		strings.ContainsFunc(address, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", "the publish command printed an address that is not one: " + run.Printable(address)
	}
	return address, ""
}

// said is what a command printed to its standard error, kept by a tail:
// made printable, trimmed, and cut to its last maxReason bytes.
func said(stderr *tail) string {
	s := strings.TrimSpace(run.Printable(strings.ToValidUTF8(string(stderr.bytes()), "\uFFFD")))
	if len(s) > maxReason {
		cut := len(s) - maxReason
		for !utf8.RuneStart(s[cut]) {
			cut++
		}
		s = "…" + s[cut:]
	}
	return s
}

// tail keeps the last max bytes written to it.
type tail struct {
	max  int
	data []byte
	cut  bool
}

func (t *tail) Write(p []byte) (int, error) {
	t.data = append(t.data, p...)
	if len(t.data) > t.max {
		t.data = append(t.data[:0], t.data[len(t.data)-t.max:]...)
		t.cut = true
	}
	return len(p), nil
}

// bytes are the bytes kept, without a part of a line that was cut.
func (t *tail) bytes() []byte {
	if t.cut {
		if i := bytes.IndexByte(t.data, '\n'); i >= 0 {
			return t.data[i+1:]
		}
	}
	return t.data
}
