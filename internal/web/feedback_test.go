package web

import (
	"fmt"
	"testing"
	"time"

	"github.com/TheEisbaer/kiln/internal/store"
)

// TestToHost puts a card that needs you for a reviewer's feedback back to the
// Git host, and refuses the move for a card that needs you after a closed
// pull request.
func TestToHost(t *testing.T) {
	f := newFixture(t)
	passed := store.State{Stage: store.Review, WaitsFor: store.Approval}
	atHost := store.State{Stage: store.Review, WaitsFor: store.Host}
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	published := func(text string) int64 {
		t.Helper()
		id := f.card(t, text, passed)
		if _, err := f.store.DecideWith(id, store.Publish, "", store.Options{Publishing: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.KeepPullRequest(id, commit, "https://git.example/pull/1"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	feedback, closed := published("Feedback"), published("Closed")
	if _, err := f.store.ReviewFeedback(feedback, commit, time.Now(), store.ReviewReason+"\n\n```text\nann: why?\n```\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PullRequestClosed(closed, commit); err != nil {
		t.Fatal(err)
	}

	w := f.do(t, "POST", fmt.Sprintf("/api/cards/%d/decide", feedback), `{"action":"to-host"}`)
	if c := decode[cardJSON](t, w); w.Code != 200 || c.Stage != atHost.Stage || c.WaitsFor != atHost.WaitsFor {
		t.Errorf("to-host after feedback: %d %+v", w.Code, c)
	}
	if got, _ := f.store.Card(feedback); got.State != atHost {
		t.Errorf("the store has %v", got.State)
	}
	if w := f.do(t, "POST", fmt.Sprintf("/api/cards/%d/decide", closed), `{"action":"to-host"}`); w.Code != 409 {
		t.Errorf("to-host after a closed pull request: %d %s", w.Code, w.Body)
	}
}
