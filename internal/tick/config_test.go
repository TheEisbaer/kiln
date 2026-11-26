package tick

import (
	"strings"
	"testing"

	"github.com/TheEisbaer/kiln/internal/store"
)

// TestToldConfig checks the part a laid-over run is told (ADR 0025, rule 2):
// its exact text for an implement run, after the other parts, and for a
// review run, before the change, and its absence for a run whose commit
// differs from main in no configuration path.
func TestToldConfig(t *testing.T) {
	ctx := t.Context()
	c, main := stagedCore(t)
	repo, err := c.repoOf(store.Card{ID: 1, Repo: store.KilnRepo})
	if err != nil {
		t.Fatal(err)
	}
	tip := commitOnBranch(t, c.Repo, main, map[string]string{"CLAUDE.md": "The card's.\n", "AGENTS.md": "The card's.\n"})
	toldAt := func(stage store.Stage, commit string) string {
		t.Helper()
		if _, err := c.store.Move(1, store.State{Stage: stage, WaitsFor: store.Run}, store.CauseDeveloper); err != nil {
			t.Fatal(err)
		}
		card, err := c.store.Card(1)
		if err != nil {
			t.Fatal(err)
		}
		text, held, err := c.told(ctx, repo, store.RunRow{ID: "ab12cd34-1-9", Card: 1, Commit: commit}, card, main)
		if err != nil || held != "" {
			t.Fatal(err, held)
		}
		return text
	}

	common := "Kiln has laid main's version of the repo's Claude Code configuration over /work, so that this run works under the " +
		"configuration as merged, not as the card changes it: every CLAUDE.md, CLAUDE.local.md and AGENTS.md, everything in " +
		"a .claude directory, and .mcp.json at the top. The card's branch differs from main in these paths (A: only the branch " +
		"has it; D: only main has it; M: both, differently):\n\n```text\nA\tAGENTS.md\nA\tCLAUDE.md\n```\n\n" +
		"The branch's version of the whole configuration is in /kiln/card-config, at the same paths. In /work, `HEAD` and " +
		"`HEAD~1` both hold main's configuration, so `git diff HEAD~1` there shows the card's change without it."
	implement := common + " Make every change to the configuration in /kiln/card-config: when you end, Kiln saves the " +
		"configuration from there and everything else from /work, and a change to the configuration in /work is not kept. " +
		"A colliding file of the configuration, if the part on what changed in main lists one, is in /kiln/card-config."
	review := common + " The change below holds the card's changes to the configuration as well."

	got := toldAt(store.Implement, tip)
	if want := "```\n\n" + configHeading + "\n\n" + implement; !strings.HasSuffix(got, want) {
		t.Errorf("the implement run was told\n%s\nwant it to end with\n%s", got, want)
	}
	got = toldAt(store.Review, tip)
	if want := "\n" + configHeading + "\n\n" + review + "\n\n## The change\n\n```diff\n"; !strings.Contains(got, want) {
		t.Errorf("the review run was told\n%s\nwant it to hold\n%s", got, want)
	}

	// A commit with no change to the configuration: no part, for either.
	plain := commitOnBranch(t, c.Repo, main, map[string]string{"NOTES.md": "# Notes\nA line.\n"})
	for _, stage := range []store.Stage{store.Implement, store.Review} {
		if got := toldAt(stage, plain); strings.Contains(got, configHeading) {
			t.Errorf("a %s run without configuration changes was told\n%s", stage, got)
		}
	}
	// And none without main, as for a run whose instruction is not written.
	card, err := c.store.Card(1)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := c.told(ctx, repo, store.RunRow{ID: "ab12cd34-1-9", Card: 1, Commit: tip}, card, ""); err != nil || strings.Contains(got, configHeading) {
		t.Errorf("without main: %v\n%s", err, got)
	}
}
