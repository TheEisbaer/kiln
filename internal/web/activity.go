package web

import (
	"sync"

	"github.com/TheEisbaer/kiln/internal/run"
)

// ringSize is how many of a run's activities are kept: the last 200.
const ringSize = 200

// Activity holds the activity of the runs the tick follows, per run, in
// memory: the last ringSize things each run did. Nothing of it is stored; a
// core started again fills it from what following gives. The tick writes it
// and the handlers read it, each from its own goroutine.
type Activity struct {
	mu   sync.Mutex
	runs map[string]*ring
}

// ring is the last ringSize activities of one run, oldest first from start.
type ring struct {
	items []run.Activity
	start int
}

// NewActivity returns an empty Activity.
func NewActivity() *Activity {
	return &Activity{runs: map[string]*ring{}}
}

// Add adds one activity of a run, dropping the oldest when the run has
// ringSize already.
func (a *Activity) Add(id string, act run.Activity) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.runs[id]
	if r == nil {
		r = &ring{}
		a.runs[id] = r
	}
	if len(r.items) < ringSize {
		r.items = append(r.items, act)
		return
	}
	r.items[r.start] = act
	r.start = (r.start + 1) % ringSize
}

// Of returns a run's activities, oldest first.
func (a *Activity) Of(id string) []run.Activity {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.runs[id]
	if r == nil {
		return nil
	}
	out := make([]run.Activity, 0, len(r.items))
	out = append(out, r.items[r.start:]...)
	return append(out, r.items[:r.start]...)
}

// Drop forgets a run's activities: before the run is followed again, which
// gives its output from the first event, and when it has ended.
func (a *Activity) Drop(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.runs, id)
}
