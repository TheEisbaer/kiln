package seal

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// The images of Kiln that the core's update builds (ADR 0019, ADR 0037): each
// is built under the new version's tag, <name>:<first 12 hex digits>, never
// under the plain name, and the new core makes the tag the plain name once it
// has reported (ADR 0036).

// Image is what BuildImage builds: the image Name:Tag from the Dockerfile at
// the path Dockerfile in the context, with the build argument TOOLCHAIN set to
// Toolchain unless it is empty, and with no network for its RUN instructions
// when NoNetwork is set.
type Image struct {
	Name       string
	Tag        string
	Dockerfile string
	Toolchain  string
	NoNetwork  bool
}

// imageName is the name of an image of Kiln's, a repository without a
// registry or a path.
var imageName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// versionTag is the tag of an image built for a version: the first 12 hex
// digits of its commit.
var versionTag = regexp.MustCompile(`^[0-9a-f]{12}$`)

// dockerfilePath is a Dockerfile's path in a context: relative, of plain
// characters.
var dockerfilePath = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)

// checkRef checks a reference: a name, or a name and a version's tag, or,
// with latest, a name and the tag latest. It returns the reference with its
// tag: <name>:latest for a plain name.
func checkRef(ref string, latest bool) (string, error) {
	name, tag, tagged := strings.Cut(ref, ":")
	if !imageName.MatchString(name) {
		return "", fmt.Errorf("the image %q is not a name of Kiln's", ref)
	}
	switch {
	case !tagged:
		return name + ":latest", nil
	case versionTag.MatchString(tag), latest && tag == "latest":
		return ref, nil
	}
	return "", fmt.Errorf("the image %q has no tag of 12 hex digits", ref)
}

// BuildImage builds an image (ADR 0037) with `docker build` from a context on
// standard input, src, a tar as `git archive` makes it, tagged Name:Tag only.
// Standard output is discarded; an error carries the last 4 KiB of standard
// error.
func BuildImage(ctx context.Context, im Image, src io.Reader) error {
	if !imageName.MatchString(im.Name) {
		return fmt.Errorf("the image %q is not a name of Kiln's", im.Name)
	}
	if !versionTag.MatchString(im.Tag) {
		return fmt.Errorf("the tag %q of %s is not 12 hex digits", im.Tag, im.Name)
	}
	if !dockerfilePath.MatchString(im.Dockerfile) || strings.Contains(im.Dockerfile, "..") {
		return fmt.Errorf("the Dockerfile %q is not a path in the context", im.Dockerfile)
	}
	args := []string{"build", "--progress", "plain", "--tag", im.Name + ":" + im.Tag, "--file", im.Dockerfile}
	if im.NoNetwork {
		args = append(args, "--network", "none")
	}
	if im.Toolchain != "" {
		if _, err := checkRef(im.Toolchain, false); err != nil {
			return err
		}
		args = append(args, "--build-arg", "TOOLCHAIN="+im.Toolchain)
	}
	args = append(args, "-")
	cmd := commandIn(ctx, "docker", args...)
	stderr := &lastBytes{max: maxBuildErr}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = src, io.Discard, stderr
	if err := cmd.Run(); err != nil {
		return dockerError(args, err, string(stderr.data))
	}
	return nil
}

// ImageID is the ID of the image of a reference, a name or a name and a
// version's tag, or "" when no image has that reference.
func ImageID(ctx context.Context, ref string) (string, error) {
	full, err := checkRef(ref, false)
	if err != nil {
		return "", err
	}
	id, err := docker(ctx, nil, "image", "inspect", "--format", "{{.Id}}", full)
	if err == nil {
		return id, nil
	}
	// The inspection failed: an image that is not there is no error.
	there, lsErr := docker(ctx, nil, "image", "ls", "--quiet", "--no-trunc", full)
	if lsErr == nil && there == "" {
		return "", nil
	}
	return "", err
}

// TagImage gives the image of src, a name and a version's tag, the reference
// dst as well, a name, or a name and a version's tag or latest.
func TagImage(ctx context.Context, src, dst string) error {
	from, err := checkRef(src, false)
	if err != nil {
		return err
	}
	if !strings.Contains(src, ":") {
		return fmt.Errorf("the image %q has no tag of 12 hex digits", src)
	}
	to, err := checkRef(dst, true)
	if err != nil {
		return err
	}
	_, err = docker(ctx, nil, "tag", from, to)
	return err
}

// RemoveImageTag removes one reference, a name and a version's tag, by that
// exact reference and never by force: an image with another reference keeps
// it, and one a container uses is refused by the engine.
func RemoveImageTag(ctx context.Context, ref string) error {
	full, err := checkRef(ref, false)
	if err != nil {
		return err
	}
	if !strings.Contains(ref, ":") {
		return fmt.Errorf("the image %q has no tag of 12 hex digits", ref)
	}
	_, err = docker(ctx, nil, "image", "rm", full)
	return err
}

// ImageTags lists the tags of a name.
func ImageTags(ctx context.Context, name string) ([]string, error) {
	if !imageName.MatchString(name) {
		return nil, fmt.Errorf("the image %q is not a name of Kiln's", name)
	}
	out, err := docker(ctx, nil, "image", "ls", "--format", "{{.Tag}}", name)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}
