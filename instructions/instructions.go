// Package instructions holds Kiln's stage instructions (ADR 0056), embedded in
// the core's program, so that a run is told the instructions of the installed
// Kiln and never those on a card's branch.
package instructions

import _ "embed" // for go:embed

// Spec is what a spec run is told before the card's text.
//
//go:embed spec.md
var Spec string

// Implement is what an implement run is told before the card, the approved
// spec and the last review that sent the change back, if any.
//
//go:embed implement.md
var Implement string

// Review is what a review run is told before the card, the approved spec and
// the change as a diff.
//
//go:embed review.md
var Review string
