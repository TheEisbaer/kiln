package tick

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
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
	"github.com/TheEisbaer/kiln/internal/settings"
	"github.com/TheEisbaer/kiln/internal/store"
)

// Publishing (card 19). A card the developer publishes waits in review for the
// Git host. While it has no pull request's address, a publisher beside the
// tick, like a run's follower and counted in no limit, brings main up to date,
// puts the change onto it as one commit, and runs the developer's publish
// command with it; the command's address is kept with the card. Once it has
// one, the tick tests it after a fetch that moved main, with the merge
// publishing makes of main and the published commit: a change that is in main
// closes the card, and a commit that no longer merges onto main makes the card
// one the publisher publishes again, which sends it back to implement with
// main merged into its branch and Kiln's markers, or publishes it again onto
// the same pull request (testAtHost).
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

// mainState is the fetching of one repo's main: when the last round began,
// the round under way, if any, which done is closed after, the rounds that
// have ended, and the last failure that was printed, kept so that a failure
// is printed once per text.
type mainState struct {
	last    time.Time
	done    chan struct{}
	rounds  int
	printed string
}

// mainOf is the fetching of the named repo's main, made on first use. The
// caller holds fetchMu.
func (c *core) mainOf(name string) *mainState {
	if c.mains == nil {
		c.mains = map[string]*mainState{}
	}
	m, ok := c.mains[name]
	if !ok {
		m = &mainState{}
		c.mains[name] = m
	}
	return m
}

// refreshMain brings a repo's main up to date from its copy's origin, if it
// has one: a round is started beside the tick at most once in fetchEvery
// (startRound), and refreshMain waits for the round under way, if any, at
// most wait. It returns the rounds of the repo that have ended.
func (c *core) refreshMain(ctx context.Context, repo settings.Repo, wait time.Duration) int {
	awaitRounds(ctx, time.Now().Add(wait), c.startRound(ctx, repo))
	return c.rounds(repo.Name)
}

// startRound starts a round of fetching a repo's main beside the tick, unless
// one is under way or the last began less than fetchEvery ago, and returns the
// round under way, nil for none. A copy without origin is used as it is, and a
// fetch that fails is printed once per text of its failure, with the repo's
// name; the main that is there is used either way.
func (c *core) startRound(ctx context.Context, repo settings.Repo) <-chan struct{} {
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	m := c.mainOf(repo.Name)
	if m.done == nil && (m.last.IsZero() || time.Since(m.last) >= fetchEvery) {
		m.last = time.Now()
		done := make(chan struct{})
		m.done = done
		c.followers.Add(1)
		go func() {
			defer c.followers.Done()
			defer close(done)
			has, err := hasOrigin(ctx, repo.Copy)
			if err == nil && has {
				fctx, cancel := context.WithTimeout(ctx, fetchLimit)
				err = fetchMain(fctx, repo.Copy)
				cancel()
			}
			c.fetchMu.Lock()
			defer c.fetchMu.Unlock()
			m.done = nil
			if ctx.Err() != nil {
				return
			}
			m.rounds++
			if err != nil {
				if text := run.Printable(err.Error()); text != m.printed {
					fmt.Fprintf(c.errOut, "kiln core: main is not brought up to date from origin, repo %s: %s\n", repo.Name, text)
					m.printed = text
				}
			} else {
				m.printed = ""
			}
		}()
	}
	return m.done
}

// rounds are the rounds of fetching a repo's main that have ended.
func (c *core) rounds(name string) int {
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	return c.mainOf(name).rounds
}

// awaitRounds waits until each round has ended, or the deadline has passed, or
// the context is done; a nil round has nothing to wait for.
func awaitRounds(ctx context.Context, deadline time.Time, rounds ...<-chan struct{}) {
	for _, done := range rounds {
		if done == nil {
			continue
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-done:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}
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

// fetchMain fetches origin's main into the repo's main, replacing it, with
// the protections of hostGit.
func fetchMain(ctx context.Context, repo string) error {
	cmd, err := hostGit(ctx, repo, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head",
		"origin", "+refs/heads/main:refs/heads/main")
	if err != nil {
		return err
	}
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

// hostGit is a git command in the repo that reaches the Git host, by
// fetchMain and by the comparison before the report (compare.go). Nothing it
// starts can ask anything: it runs in a session of its own, with no
// terminal, git never prompts, ssh runs in batch mode and never asks for a
// passphrase by a program either, and when the context ends the whole
// session is killed, ssh included, and its pipes are let go after a moment.
// The ssh command is the developer's GIT_SSH_COMMAND, or the repo's
// core.sshCommand, or ssh, with -o BatchMode=yes after it.
func hostGit(ctx context.Context, repo string, args ...string) (*exec.Cmd, error) {
	ssh := os.Getenv("GIT_SSH_COMMAND")
	if ssh == "" {
		configured, err := gitLine(ctx, repo, "config", "--default", "", "--get", "core.sshCommand")
		if err != nil {
			return nil, err
		}
		ssh = cmp.Or(configured, "ssh")
	}
	cmd := gitCommand(ctx, repo, args...)
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND="+ssh+" -o BatchMode=yes", "SSH_ASKPASS_REQUIRE=never")
	detach(cmd)
	return cmd, nil
}

// detach runs a command in a session of its own, with no terminal to read or
// ask on, and kills the whole session when its context ends, letting go of
// its pipes a moment later even if something still holds them.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
}

// atHostRound is what the tick does for the cards at the Git host, repo by
// repo, in the settings' order: a repo's main is brought up to date when a
// run of one of its cards may start, those in fetch, or when one of its cards
// waits for the Git host; Kiln's own also on every tick under a launcher. After
// each round of a repo, every card of it with a pull request is tested against
// its main when main or its commit moved since its last test: closed when its
// change is in main, published again when it no longer merges (testAtHost).
// The update looks at Kiln's own main only (update.go). Then a checker is
// started for every card still at the Git host whose check is due
// (checkresult.go), never for one the test moved in this round, and a
// publisher for every card the core publishes, each with its card's repo.
//
// The tick waits for the fetches at most fetchWait in all, so that a slow
// origin holds no run back for long; a publisher waits for it to its end.
func (c *core) atHostRound(ctx context.Context, fetch map[string]bool) error {
	type side struct {
		repo      settings.Repo
		toPublish []int64
		published map[int64]string
		fetched   bool
	}
	// The rounds of every repo that needs one are started first, and then
	// waited for at most fetchWait in all, so that two slow origins do not
	// hold the runs back twice as long. A core with a launcher brings Kiln's
	// own main up to date on every tick as well, so that an update is seen
	// while nothing else needs main, without waiting for the fetch
	// (update.go).
	var sides []side
	deadline := time.Now().Add(fetchWait)
	var waits []<-chan struct{}
	for _, r := range c.repos() {
		toPublish, err := c.store.Publishing(r.Name)
		if err != nil {
			return err
		}
		published, err := c.store.AtHost(r.Name)
		if err != nil {
			return err
		}
		needed := fetch[r.Name] || len(toPublish) > 0 || len(published) > 0
		s := side{repo: r, toPublish: toPublish, published: published, fetched: needed || (r.Own && c.update.launched)}
		if s.fetched {
			done := c.startRound(ctx, r)
			if needed {
				waits = append(waits, done)
			}
		}
		sides = append(sides, s)
	}
	awaitRounds(ctx, deadline, waits...)
	// What a card was last tested with is dropped once it is at the Git host
	// in no repo, so that one repo's round keeps another's.
	for id := range c.tested {
		if !slices.ContainsFunc(sides, func(s side) bool { _, ok := s.published[id]; return ok }) {
			delete(c.tested, id)
		}
	}
	if c.checked == nil {
		c.checked = map[string]int{}
	}
	moved := false
	for _, s := range sides {
		if !s.fetched {
			continue
		}
		if rounds := c.rounds(s.repo.Name); rounds != c.checked[s.repo.Name] {
			c.checked[s.repo.Name] = rounds
			moved = moved || s.repo.Own
			c.testAtHost(ctx, s.repo, s.published)
		}
	}
	for _, s := range sides {
		c.startCheckers(ctx, s.repo, s.published)
	}
	c.checkUpdate(ctx, moved)
	for _, s := range sides {
		if err := c.startPublishers(ctx, s.repo, s.toPublish); err != nil {
			return err
		}
	}
	return nil
}

// startPublishers starts a publisher for each card of a repo the core
// publishes, when the repo has a publish command.
func (c *core) startPublishers(ctx context.Context, repo settings.Repo, toPublish []int64) error {
	if repo.PublishCommand == "" {
		return nil
	}
	for _, id := range toPublish {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.startPublisher(ctx, repo, id)
	}
	return nil
}

// closeIfMerged closes a card at the Git host whose published commit, merged
// into its repo's main, changes nothing: the Git host merged it.
func (c *core) closeIfMerged(ctx context.Context, repo settings.Repo, card int64, commit string) error {
	if commit == "" {
		return nil
	}
	main, err := head(ctx, repo.Copy)
	if err != nil {
		return err
	}
	in, err := changesNothing(ctx, repo.Copy, main, commit)
	if err != nil || !in {
		return err
	}
	return c.closeMerged(repo, card)
}

// closeMerged closes a card of a repo at the Git host as merged by the Git
// host. Only a merge in Kiln's own repo counts for the update (ADR 0019).
func (c *core) closeMerged(repo settings.Repo, card int64) error {
	if _, err := c.store.MoveWithReason(card, atHost, closed, store.CauseHost, store.MergedReason); err != nil {
		return err
	}
	if repo.Own {
		c.update.merged.Store(true)
	}
	fmt.Fprintf(c.out, "card %d: merged\n", card)
	return nil
}

// hostTest is what a card at the Git host was last tested with: main and its
// published commit.
type hostTest struct{ main, commit string }

// testAtHost tests every card of a repo at the Git host with a published
// commit, by ID, against the repo's main, with the one merge that publishing makes, of main and the commit, unless
// main and the commit are what they were at this core's last test of it
// (ADR 0035): the test is a pace kept in memory only, so a core started again
// tests every card at once. A merge without a conflict whose tree is main's
// closes the card as merged: a merge is found first. A merge with a conflict
// makes the card one the core publishes again (store.PublishAgain), its
// publication cleared and its branch kept; the publisher then sends it back to
// implement with main merged into its branch and Kiln's markers, publishes it
// again onto the same pull request when its branch no longer collides, or
// closes it when its change is all in main. Without a publish command the
// card stays, and that is said once per main.
//
// The test and the check result command's round never both move one card:
// each moves it only while it is at the Git host and its publication has the
// commit it looked at, checked in the transaction that writes, and the second
// is refused and writes nothing. The test touches no branch: only the
// publisher does, once the card has no address, which store.CheckFailed
// needs. A card the test moved, closed or cleared is taken out of published,
// so that no checker is started for it in this round.
//
// A test that ended without an error, a refusal included, is kept in tested;
// an error is printed, with the repo's name when the instance has more than
// one, and keeps nothing, and the card is tested again after the next round.
func (c *core) testAtHost(ctx context.Context, repo settings.Repo, published map[int64]string) {
	if c.tested == nil {
		c.tested = map[int64]hostTest{}
	}
	if len(published) == 0 {
		return
	}
	main, mainTree, err := mainAndTree(ctx, repo.Copy)
	if err != nil {
		text := run.Printable(err.Error())
		if len(c.repos()) > 1 {
			text = "repo " + repo.Name + ": " + text
		}
		fmt.Fprintf(c.errOut, "kiln core: the cards at the Git host are not tested: %s\n", text)
		return
	}
	for _, id := range slices.Sorted(maps.Keys(published)) {
		if ctx.Err() != nil {
			return
		}
		commit := published[id]
		if commit == "" || c.tested[id] == (hostTest{main, commit}) {
			continue
		}
		gone, err := c.testCard(ctx, repo, id, main, mainTree, commit)
		if errors.Is(err, store.ErrRefused) {
			gone, err = true, nil // moved meanwhile, by the checker or the developer
		}
		if err != nil {
			fmt.Fprintf(c.errOut, "kiln core: card %d: %s\n", id, run.Printable(err.Error()))
			continue
		}
		c.tested[id] = hostTest{main, commit}
		if gone {
			delete(published, id)
		}
	}
}

// mainAndTree reads main and its tree.
func mainAndTree(ctx context.Context, repo string) (string, string, error) {
	main, err := head(ctx, repo)
	if err != nil {
		return "", "", err
	}
	tree, err := gitLine(ctx, repo, "rev-parse", "--verify", main+"^{tree}")
	return main, tree, err
}

// testCard tests one card at the Git host with its repo's main, and reports
// whether it moved, was closed or had its publication cleared.
func (c *core) testCard(ctx context.Context, repo settings.Repo, id int64, main, mainTree, commit string) (bool, error) {
	at("host-tested")
	tree, conflicts, err := mergeTree(ctx, repo.Copy, main, commit)
	switch {
	case err != nil:
		return false, err
	case len(conflicts) == 0 && tree == mainTree:
		return true, c.closeMerged(repo, id)
	case len(conflicts) == 0:
		return false, nil
	case repo.PublishCommand == "":
		fmt.Fprintf(c.errOut, "kiln core: card %d: %s\n", id, run.Printable(fmt.Sprintf(
			"the published commit %s no longer merges onto main: %s; without a publish command it stays at the Git host",
			commit, fileNames(conflicts))))
		return false, nil
	}
	reason := run.Printable(fmt.Sprintf("main moved, and the published commit %s no longer merges onto it: %s; the core publishes the change again",
		commit, fileNames(conflicts)))
	if _, err := c.store.PublishAgain(id, commit, reason); err != nil {
		return false, err
	}
	fmt.Fprintf(c.out, "card %d: %s\n", id, reason)
	return true, nil
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

// startPublisher starts a publisher for a card of a repo beside the tick,
// unless one publishes it already. The card is published from now until the publisher
// has returned; an error of it is printed with the card's ID, and the next
// tick starts another.
func (c *core) startPublisher(ctx context.Context, repo settings.Repo, id int64) {
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
		err := c.publish(ctx, repo, id)
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

// publish publishes one card of a repo that waits for the Git host without a
// pull request, all of it in the repo's copy: its main is brought up to date; a branch that still holds an earlier
// merge of main with its conflicts, or a change that collides with main,
// sends the card back to implement; a change that is all in main closes it;
// and otherwise the change is put onto main as one commit and the publish
// command is run with it. The command's address is kept with the card, and
// its failure moves the card to "needs you".
func (c *core) publish(ctx context.Context, repo settings.Repo, id int64) error {
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
	c.refreshMain(ctx, repo, fetchLimit+10*time.Second)
	branch := branchOf(id)
	tip, err := branchTip(ctx, repo.Copy, branch)
	if err != nil {
		return err
	}
	if tip == "" {
		return c.publishFailed(card, "the card's branch "+branch+" does not exist")
	}
	main, err := head(ctx, repo.Copy)
	if err != nil {
		return err
	}

	// A branch that still holds Kiln's conflict markers, from a merge of
	// main that no run has resolved, or not all of it, goes back as it is.
	if files, err := markedFiles(ctx, repo.Copy, tip, id); err != nil || len(files) > 0 {
		if err != nil {
			return err
		}
		return c.toImplement(card, files)
	}
	tree, conflicts, err := mergeTree(ctx, repo.Copy, main, tip)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		if err := c.mergeMain(ctx, repo, id, tip, main); err != nil {
			return err
		}
		return c.toImplement(card, conflicts)
	}
	if mainTree, err := gitLine(ctx, repo.Copy, "rev-parse", "--verify", main+"^{tree}"); err != nil || tree == mainTree {
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
	touched, err := workflowFiles(ctx, repo.Copy, tip)
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
	runID, err := gitLine(ctx, repo.Copy, "log", "-1", "--format=%(trailers:key=Kiln-Run,valueonly,separator=%x2C)", tip)
	if err != nil {
		return err
	}
	// The branch on the Git host is named at the first publishing and kept,
	// so that a title that changes later opens no second pull request.
	branchName, err := c.store.PublishBranch(id, publishBranch(id, title))
	if err != nil {
		return err
	}
	commit, err := commitTree(ctx, repo.Copy, tree, publishMessage(title, id, runID), []string{main}, []string{tip, main})
	if err != nil {
		return err
	}
	if _, err := gitLine(ctx, repo.Copy, "update-ref", "--no-deref", publishRef(id), commit); err != nil {
		return err
	}

	env := []string{
		envCard + "=" + strconv.FormatInt(id, 10),
		envRepo + "=" + repo.Copy,
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
	address, failure := c.runPublish(ctx, repo.PublishCommand, env)
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
// It counts as a send-back (ADR 0054, rule 3): a card sent back twice already
// waits in implement for the developer instead of a run.
func (c *core) toImplement(card store.Card, files []string) error {
	reason := run.Printable(conflictReason + ": " + fileNames(files))
	to := implementWaits
	if card.SendBacks >= store.MaxSendBacks {
		to = implementNeedsYou
		reason = run.Printable(conflictReason + ": " + fileNames(files) + "; the card was sent back twice already, so it waits for you")
	}
	if _, err := c.store.MoveSentBack(card.ID, atHost, to, reason); err != nil {
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
// its tip, all in the copy of the card's repo. The implement run that follows
// starts there (told).
func (c *core) mergeMain(ctx context.Context, r settings.Repo, card int64, tip, main string) error {
	repo := r.Copy
	// Merged by the refs, so that the conflict markers name them, and only
	// while the refs are where they were read.
	tree, _, err := mergeTree(ctx, repo, "refs/heads/"+branchOf(card), "refs/heads/main")
	if err != nil {
		return err
	}
	for ref, want := range map[string]string{"refs/heads/" + branchOf(card): tip, "refs/heads/main": main} {
		if at, err := gitLine(ctx, repo, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}"); err != nil || at != want {
			return fmt.Errorf("%s moved while main was merged into the card's branch: %v", ref, err)
		}
	}
	merge, err := commitTree(ctx, repo, tree, mergeMessage(card), []string{tip, main}, []string{tip, main})
	if err != nil {
		return err
	}
	_, err = gitLine(ctx, repo, "update-ref", "--no-deref", "refs/heads/"+branchOf(card), merge, tip)
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

// runPublish runs command, the publish command of a card's repo (card 19):
// with sh -c, in the data directory, with no standard input, for at most
// publishLimit, its process group ended with it, and with the environment of
// the core, from which the command's variables are taken out, and env. It
// returns the pull request's address, the last line of its standard output
// that starts with https://, or the failure: its exit status and its standard
// error, made printable and cut to its last 2 KiB.
func (c *core) runPublish(ctx context.Context, command string, env []string) (address, failure string) {
	ctx, cancel := context.WithTimeout(ctx, publishLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
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
