package tick

import (
	"os/exec"
	"testing"

	"github.com/TheEisbaer/kiln/internal/settings"
	"github.com/TheEisbaer/kiln/internal/store"
)

// needWorkItem gives the core the one repo as a settings list, with the repo
// needing a work item number when needs is set, and returns the copy's path.
func (p *publishing) needWorkItem(needs bool) string {
	copy := p.cfg.Repo
	p.cfg.Repos = []settings.Repo{{Name: store.KilnRepo, Copy: copy, Own: true,
		PublishCommand: p.cfg.PublishCommand, CheckResultCommand: p.cfg.CheckResultCommand, SpecHosts: p.cfg.Hosts,
		NeedsWorkItem: needs}}
	p.cfg.Repo, p.cfg.PublishCommand, p.cfg.CheckResultCommand, p.cfg.Hosts = "", "", "", nil
	return copy
}

const needsWorkItemReason = "the repo kiln needs a work item number before publishing; give it with the approval or when you publish"

// TestWorkItemHeldByRule: a review that passes on a card without a number, of a
// repo that needs one, leaves the card in review waiting for approval, with
// the rule's reason and no command run; once a number is given with publish,
// the card is published and the command is told it.
func TestWorkItemHeldByRule(t *testing.T) {
	p := newPublishing(t)
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.reviewing(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
	})
	p.needWorkItem(true)
	e := newFakeEngine()
	core := p.start(t, e, 3)
	e.open(card, "exit")
	got := p.waitFor(t, card, reviewPassed, needsWorkItemReason)
	if got.Reason != needsWorkItemReason || got.Cause != store.CauseRule {
		t.Errorf("the entry is %+v", got)
	}
	e.waitTicks(t, 10)
	if n := p.calls(t); n != 0 {
		t.Errorf("the command ran %d times", n)
	}
	p.look(t, func(s *store.Store) {
		if _, err := s.DecideWith(card, store.Publish, "", store.Options{Publishing: true, NeedsWorkItem: true}); err == nil {
			t.Error("publish without a number was taken")
		}
		if _, err := s.DecideWith(card, store.Publish, "", store.Options{Publishing: true, NeedsWorkItem: true, WorkItem: 4711}); err != nil {
			t.Fatal(err)
		}
	})
	p.waitFor(t, card, atHost, "published as https://git.example/pull/1")
	core.stop(t)
	if n := p.calls(t); n != 1 || p.env(t, 1)[envWorkItem] != "4711" {
		t.Errorf("the command ran %d times, and was told %q", n, p.env(t, 1)[envWorkItem])
	}
}

// TestWorkItemInEnvironment: the command is told KILN_WORK_ITEM only for a card
// with a number; one in the core's own environment never reaches it.
func TestWorkItemInEnvironment(t *testing.T) {
	t.Setenv(envWorkItem, "99")
	for _, tc := range []struct {
		number int64
		want   string
	}{{0, ""}, {3, "3"}} {
		p := newPublishing(t)
		p.workItem = tc.number
		var card int64
		p.with(t, func(s *store.Store) {
			card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
			p.publish(t, s, card, false)
		})
		core := p.start(t, newFakeEngine(), 3)
		p.waitFor(t, card, atHost, "published as ")
		core.stop(t)
		env := p.env(t, 1)
		if v, ok := env[envWorkItem]; ok != (tc.want != "") || v != tc.want {
			t.Errorf("a card with %d: the command was told %s=%q (%v)", tc.number, envWorkItem, v, ok)
		}
	}
}

// TestPublisherChecksWorkItem: a card at the Git host without a number, of a
// repo that needs one, goes to "needs you" with the reason before anything is
// written, and the command is not run.
func TestPublisherChecksWorkItem(t *testing.T) {
	p := newPublishing(t)
	var card int64
	p.with(t, func(s *store.Store) {
		card = p.card(t, s, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
		p.publish(t, s, card, false)
	})
	copy := p.needWorkItem(true)
	core := p.start(t, newFakeEngine(), 3)
	got := p.waitFor(t, card, publishNeedsYou, "publishing failed: "+needsWorkItemReason)
	core.stop(t)
	if got.Cause != store.CauseRule {
		t.Errorf("the entry is %+v", got)
	}
	if n := p.calls(t); n != 0 {
		t.Errorf("the command ran %d times", n)
	}
	if err := exec.Command("git", "--git-dir", copy, "rev-parse", "--verify", "--quiet", "refs/kiln/publish/card-1").Run(); err == nil {
		t.Error("the publisher wrote refs/kiln/publish/card-1 before its check")
	}
}
