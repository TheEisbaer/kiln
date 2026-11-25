// Package images holds Kiln's layer of a run image (ADR 0039), embedded in the
// core's program, so that the core builds a repo's own run image from the
// installed Kiln's layer, as it tells a run the installed Kiln's instructions
// (ADR 0056), and never from a card's branch.
package images

import "embed"

// RunLayer is the directory run/, Kiln's layer: its Dockerfile, built with the
// directory as the context on top of the toolchain image the build argument
// TOOLCHAIN names.
//
//go:embed run
var RunLayer embed.FS
