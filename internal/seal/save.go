package seal

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// saveName is the name of a run's save container.
func saveName(id string) string { return "kiln-save-" + id }

// objectID is a Git object's ID, SHA-1 or SHA-256.
var objectID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// basePath is where SaveRun puts the base in the save container.
const basePath = "kiln/base.pack"

// saveScript is the save container's command, run as `sh -c saveScript sh
// <tree>`. In a temporary bare repo it takes in the base, makes the working
// tree readable to its owner, removes every .git, so that a nested repo is
// saved as plain files and not as a gitlink, and adds the working tree on top
// of <tree> in a temporary index. A file that <tree> tracks under an ignored
// path stays, and a new ignored file is left out. It writes the new tree's ID
// to /tmp/out/tree and a pack of the objects <tree> lacks to
// /tmp/out/objects.pack.
//
// Every run has a .git at /work/.git since its baseline is made (internal/run);
// it is removed like any other .git, and nothing of it is read or saved.
//
// The object list goes through a file, not a pipe, so that a failure of
// rev-list fails the script: sh has no pipefail.
const saveScript = `set -eu
tree=$1
export GIT_DIR=/tmp/save.git GIT_INDEX_FILE=/tmp/save.index
git init --quiet --bare "$GIT_DIR"
git index-pack --stdin < /kiln/base.pack > /dev/null
chmod -R u+rwX /work
find /work -name .git -prune -exec rm -rf {} +
git read-tree "$tree"
git --work-tree=/work add -A
new=$(git write-tree)
mkdir /tmp/out
git rev-list --objects "$new" --not "$tree" > /tmp/objects
git pack-objects --quiet --stdout < /tmp/objects > /tmp/out/objects.pack
printf '%s\n' "$new" > /tmp/out/tree
`

// SaveRun takes the working tree of a run's container that has stopped and
// returns it as a Git tree on top of tree: the new tree's ID, and a pack of
// the objects that tree lacks. base is a pack of tree's objects, as `git
// rev-list --objects <tree> | git pack-objects --stdout` makes it.
//
// The work is done in a save container, kiln-save-<id>, with no network, from
// the run image, so that nothing of the run's tree is unpacked on the host:
//
//  1. A save container left behind with the label kiln.run=<id> is removed.
//     One without it is refused, and nothing is touched.
//  2. It creates the save container fresh, with saveScript as its command.
//  3. It copies in kiln/base.pack, and pipes the run's /work into it; the host
//     only passes the bytes on.
//  4. It starts the save container, waits for it, and reads /tmp/out.
//
// The save container is removed afterwards, after success or failure, on a
// context that cannot be cancelled. SaveRun can be repeated: the same tree
// gives the same result.
func SaveRun(ctx context.Context, id, image, tree string, base []byte) (newTree string, pack []byte, err error) {
	if !objectID.MatchString(tree) {
		return "", nil, fmt.Errorf("the tree %q is not an object ID", tree)
	}
	run, err := existingRun(ctx, id)
	if err != nil {
		return "", nil, err
	}
	if run.state != "exited" {
		return "", nil, fmt.Errorf("the container %s has not stopped (it is %s); it is not saved", runName(id), run.state)
	}

	name := saveName(id)
	old, err := findContainer(ctx, name, runLabel)
	if err != nil {
		return "", nil, err
	}
	if old != nil {
		if old.label != id {
			return "", nil, notOurs("container", name, runLabel+"="+id)
		}
		if _, err := docker(ctx, nil, "rm", "--force", old.id); err != nil {
			return "", nil, err
		}
	}

	save, err := createSave(ctx, id, image, []string{"sh", "-c", saveScript, "sh", tree})
	if err != nil {
		return "", nil, err // createSave leaves nothing behind
	}
	defer func() {
		// The context may be what failed a step, and the container must go anyway.
		if _, rmErr := docker(context.WithoutCancel(ctx), nil, "rm", "--force", save); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("removing the save container: %w", rmErr))
		}
	}()

	in, err := oneFile(basePath, base)
	if err != nil {
		return "", nil, err
	}
	if err := dockerIO(ctx, bytes.NewReader(in), nil, "cp", "--archive", "-", save+":/"); err != nil {
		return "", nil, err
	}
	if err := copyBetween(ctx, run.id+":/"+workDir, save+":/"); err != nil {
		return "", nil, err
	}

	if _, err := docker(ctx, nil, "start", save); err != nil {
		return "", nil, err
	}
	status, err := docker(ctx, nil, "wait", save)
	if err != nil {
		return "", nil, err
	}
	if status != "0" {
		logs, _ := commandIn(ctx, "docker", "logs", "--tail", "20", save).CombinedOutput()
		return "", nil, fmt.Errorf("the save exited with %s: %s", status, strings.TrimSpace(string(logs)))
	}

	var out bytes.Buffer
	if err := dockerIO(ctx, nil, &out, "cp", save+":/tmp/out", "-"); err != nil {
		return "", nil, err
	}
	return readSaved(&out)
}

// createSave creates a save container for a run, with the given command, and
// returns its ID. It has no network, the run's user, the fixed limits
// (fixedLimits) whatever its run's, no capabilities, no mounts and no
// environment, and carries the label kiln.run=<id>. The image is never pulled. On failure nothing is left behind.
func createSave(ctx context.Context, id, image string, command []string) (string, error) {
	if err := checkRunID(id); err != nil {
		return "", err
	}
	args := []string{
		"--pull", "never", // a missing image is an error, never a download
		"--name", saveName(id),
		"--label", runLabel + "=" + id,
		"--network", "none",
		"--user", "1000:1000",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
	}
	args = append(args, fixedLimits...)
	args = append(args, "--log-driver", "json-file", "--", image)
	return createContainer(ctx, nil, "create", append(args, command...)...)
}

// oneFile returns a tar with one file, owned by the run's user, mode 0644.
func oneFile(name string, data []byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644,
		Uid: runUID, Gid: runUID, Size: int64(len(data)), ModTime: time.Now().Truncate(time.Second)}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(data); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// copyBetween pipes `docker cp <from> -` into `docker cp --archive - <to>`.
// The host passes the bytes on and reads none of them.
func copyBetween(ctx context.Context, from, to string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	var outErr, inErr strings.Builder
	out := commandIn(ctx, "docker", "cp", from, "-")
	out.Stdout, out.Stderr = w, &outErr
	in := commandIn(ctx, "docker", "cp", "--archive", "-", to)
	in.Stdin, in.Stderr = r, &inErr

	if err := out.Start(); err != nil {
		r.Close()
		w.Close()
		return err
	}
	if err := in.Start(); err != nil {
		r.Close()
		w.Close()
		cancel()
		out.Wait()
		return err
	}
	// The two commands hold the ends of the pipe now. With the host's copies
	// closed, either one sees the other end.
	r.Close()
	w.Close()
	inRun := in.Wait()
	if inRun != nil {
		cancel()
	}
	outRun := out.Wait()
	switch {
	case outRun != nil:
		return dockerError([]string{"cp"}, outRun, outErr.String())
	case inRun != nil:
		return dockerError([]string{"cp"}, inRun, inErr.String())
	}
	return nil
}

// readSaved reads the tar of /tmp/out: the new tree's ID and the pack.
func readSaved(r io.Reader) (string, []byte, error) {
	var tree string
	var pack []byte
	var haveTree, havePack bool
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("reading the save's output: %w", err)
		}
		switch hdr.Name {
		case "out/tree":
			data, err := io.ReadAll(tr)
			if err != nil {
				return "", nil, err
			}
			tree, haveTree = strings.TrimSpace(string(data)), true
		case "out/objects.pack":
			if pack, err = io.ReadAll(tr); err != nil {
				return "", nil, err
			}
			havePack = true
		}
	}
	if !haveTree || !havePack {
		return "", nil, errors.New("the save's output lacks tree or objects.pack")
	}
	if !objectID.MatchString(tree) {
		return "", nil, fmt.Errorf("the save's tree %q is not an object ID", tree)
	}
	return tree, pack, nil
}
