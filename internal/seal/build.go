package seal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Program is what Build builds: the core of Kiln's repo at a commit, for the
// machine of GOOS and GOARCH, in the toolchain image Image, for an instance.
type Program struct {
	Instance string // names the build's container
	Image    string // the toolchain image (ADR 0039), never pulled
	Version  string // the commit, stamped with -X main.version
	GOOS     string
	GOARCH   string
}

// buildLabel carries the instance on its build's container, and marks the
// container as Kiln's.
const buildLabel = "kiln.build"

// buildName is the name of an instance's build container.
func buildName(instance string) string { return "kiln-build-" + instance }

// goWord is what GOOS and GOARCH may be.
var goWord = regexp.MustCompile(`^[a-z0-9]{1,20}$`)

// buildScript is the build container's command, run as `sh -c buildScript sh
// <version>`: it unpacks the source from standard input, builds cmd/core with
// the version stamped, and writes the program to standard output.
const buildScript = `set -eu
mkdir /tmp/src
tar -xf - -C /tmp/src
cd /tmp/src
go build -trimpath -ldflags "-X main.version=$1" -o /tmp/kiln ./cmd/core
cat /tmp/kiln
`

// maxBuildErr is the most of the build's standard error that its error keeps.
const maxBuildErr = 4 << 10

// Build builds the core of a commit (ADR 0037) in a sealed container,
// kiln-build-<instance>, from the toolchain image: with no network, no mount,
// no capabilities, the fixed limits (fixedLimits) and no environment but
// GOOS, GOARCH and CGO_ENABLED=0, as the image's own user. src is the source,
// a tar as `git archive` makes it, on the container's standard input; the
// program comes out on its standard output into out. An error carries the
// last 4 KiB of the build's standard error.
//
// A container of the name left behind with the label kiln.build=<instance> is
// removed first; one without it is refused, and nothing is touched. The
// container is removed afterwards, after success or failure, on a context
// that cannot be cancelled.
func Build(ctx context.Context, p Program, src io.Reader, out io.Writer) (err error) {
	if !objectID.MatchString(p.Version) {
		return fmt.Errorf("the version %q is not a commit", p.Version)
	}
	if err := checkInstance(p.Instance); err != nil {
		return err
	}
	if !goWord.MatchString(p.GOOS) || !goWord.MatchString(p.GOARCH) {
		return fmt.Errorf("the machine %q/%q is not one Go names", p.GOOS, p.GOARCH)
	}
	if p.Image == "" || strings.HasPrefix(p.Image, "-") {
		return fmt.Errorf("the image %q is not one", p.Image)
	}
	name := buildName(p.Instance)
	old, err := findContainer(ctx, name, buildLabel)
	if err != nil {
		return err
	}
	if old != nil {
		if old.label != p.Instance {
			return fmt.Errorf("the container %s has no label %s=%s, so Kiln did not make it for this build; it is left alone",
				name, buildLabel, p.Instance)
		}
		if _, err := docker(ctx, nil, "rm", "--force", old.id); err != nil {
			return err
		}
	}
	defer func() {
		// The context may be what failed the build, and the container must go anyway.
		if _, rmErr := docker(context.WithoutCancel(ctx), nil, "rm", "--force", name); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("removing the build container: %w", rmErr))
		}
	}()
	args := []string{
		"run", "--rm", "--interactive",
		"--pull", "never", // a missing image is an error, never a download
		"--name", name,
		"--label", buildLabel + "=" + p.Instance,
		"--network", "none",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
	}
	args = append(args, fixedLimits...)
	args = append(args, "--env", "GOOS="+p.GOOS, "--env", "GOARCH="+p.GOARCH, "--env", "CGO_ENABLED=0",
		"--", p.Image, "sh", "-c", buildScript, "sh", p.Version)
	cmd := commandIn(ctx, "docker", args...)
	stderr := &lastBytes{max: maxBuildErr}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = src, out, stderr
	if err := cmd.Run(); err != nil {
		return dockerError(args, err, string(stderr.data))
	}
	return nil
}

// lastBytes keeps the last max bytes written to it.
type lastBytes struct {
	max  int
	data []byte
}

func (l *lastBytes) Write(p []byte) (int, error) {
	l.data = append(l.data, p...)
	if len(l.data) > l.max {
		l.data = append(l.data[:0], l.data[len(l.data)-l.max:]...)
	}
	return len(p), nil
}
