// Package instructions holds Kiln's stage instructions (ADR 0056), embedded in
// the core's program, so that a run is told the instructions of the installed
// Kiln and never those on a card's branch.
package instructions

import _ "embed" // for go:embed

// Spec is what a spec run is told before the card's text.
//
//go:embed spec.md
var Spec string
