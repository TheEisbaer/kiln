package store

// Interventions counts a card's interventions from its entries, in the order
// of their numbers (ADR 0042, rule 2). An entry counts when its cause is the
// developer, except the card's first entry (writing it), a move out of the
// backlog to spec waiting for a run or to the backlog waiting for room
// (starting it), a move from spec waiting for approval back to spec waiting
// for a run (a rejected spec, which belongs to the approval), and the card's
// first move from spec waiting for approval to implement waiting for a run
// (the planned approval). A move is told from the state of the previous
// entry. An entry about a proposal is passed over as if it were not there:
// it changes no card. It reads nothing else; a count is a query, not a
// counter.
func Interventions(all []Entry) int {
	var entries []Entry
	for _, e := range all {
		if e.Proposal == 0 {
			entries = append(entries, e)
		}
	}
	n := 0
	approved := false // the planned approval has been made
	for i, e := range entries {
		if i == 0 || e.Cause != CauseDeveloper {
			continue
		}
		from := entries[i-1].State
		switch {
		case from.Stage == Backlog && (e.State == State{Spec, Run} || e.State == State{Backlog, Room}):
			// starting the card
		case from == State{Spec, Approval} && e.State == State{Spec, Run}:
			// a rejected spec
		case from == State{Spec, Approval} && e.State == State{Implement, Run} && !approved:
			approved = true
		default:
			n++
		}
	}
	return n
}

// Measures are the board's measures over its whole history.
type Measures struct {
	Merged        int // the cards the Git host merged
	Interventions int // their interventions, in all
	Dropped       int // the cards the developer dropped
}

// MeasuresOf counts the merged and the dropped cards among the summaries, and
// the interventions of the merged ones. Open cards, and cards closed with
// done, count in none of the three.
func MeasuresOf(sums []Summary) Measures {
	var m Measures
	for _, s := range sums {
		switch {
		case s.Merged:
			m.Merged++
			m.Interventions += s.Interventions
		case s.Dropped:
			m.Dropped++
		}
	}
	return m
}
