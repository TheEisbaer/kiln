package seal

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// A run's life after CreateRun. Every function here acts only on the container
// kiln-run-<id>, the way out kiln-wayout-<id> and the network kiln-run-<id>
// that carry the label kiln.run=<id>. Anything else of those names was not made for the run: it is
// refused, and nothing is touched.

// What PlaceRun writes into a run's container (ADR 0038): the working tree under
// /work, and the instruction.
const (
	workDir         = "work"
	instructionPath = "kiln/instruction.md"
)

// runUID is the run's user and group, kiln in the run image (ADR 0039).
const runUID = 1000

// findRun finds the container of a run and refuses one without the run's
// label. It returns nil when there is none.
func findRun(ctx context.Context, id string) (*foundContainer, error) {
	if err := checkRunID(id); err != nil {
		return nil, err
	}
	name := runName(id)
	c, err := findContainer(ctx, name, runLabel)
	if err != nil || c == nil {
		return nil, err
	}
	if c.label != id {
		return nil, notOurs("container", name, runLabel+"="+id)
	}
	return c, nil
}

// existingRun is findRun, with no container an error.
func existingRun(ctx context.Context, id string) (*foundContainer, error) {
	c, err := findRun(ctx, id)
	if err == nil && c == nil {
		err = fmt.Errorf("there is no container %s", runName(id))
	}
	return c, err
}

// PlaceRun places the files of a commit and the instruction in a run's
// container that has never started.
//
// files is a tar of the commit with relative paths. PlaceRun reads all of it
// and writes a new one, in which every entry goes under work/, with a directory
// entry for every parent. Each entry is owned by uid and gid 1000, keeps only
// its permission bits, carries no extended attributes, and is a file, a
// directory or a symlink. An absolute name, a name with "..", any other type, a
// name given twice, and a name under one that is not a directory are refused
// before anything is copied. The instruction goes to kiln/instruction.md, mode
// 0644.
//
// Everything is copied with one `docker cp --archive`, so nothing else can be
// placed. It can be repeated on a container that has not started: the same
// files are written again.
func PlaceRun(ctx context.Context, id string, files io.Reader, instruction []byte) error {
	c, err := existingRun(ctx, id)
	if err != nil {
		return err
	}
	if c.state != "created" {
		return fmt.Errorf("the container %s has started (it is %s); nothing is placed in it", runName(id), c.state)
	}
	archive, err := placing(files, instruction)
	if err != nil {
		return err
	}
	// By ID, so that a container that took the name in between is not touched.
	return dockerIO(ctx, bytes.NewReader(archive), nil, "cp", "--archive", "-", c.id+":/")
}

// placing reads the caller's tar and returns the tar that PlaceRun copies.
func placing(files io.Reader, instruction []byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	written := map[string]byte{} // a path under work/, "" for work/ itself, by type

	header := func(name string, typ byte, mode int64, size int64, link string, mtime time.Time) error {
		if mtime.Before(time.Unix(0, 0)) {
			mtime = time.Unix(0, 0) // a time a plain tar header can hold
		}
		return tw.WriteHeader(&tar.Header{
			Typeflag: typ,
			Name:     name,
			Linkname: link,
			Mode:     mode & 0o777,
			Uid:      runUID,
			Gid:      runUID,
			Size:     size,
			ModTime:  mtime.Truncate(time.Second),
			Format:   tar.FormatPAX, // for long names; no other record is written
		})
	}
	dir := func(p string, mode int64, mtime time.Time) error {
		if t, ok := written[p]; ok {
			if t != tar.TypeDir {
				return fmt.Errorf("refused: %q is a directory as well as a file or a symlink", p)
			}
			return nil
		}
		written[p] = tar.TypeDir
		return header(path.Join(workDir, p)+"/", tar.TypeDir, mode, 0, "", mtime)
	}

	if err := dir("", 0o755, time.Unix(0, 0)); err != nil {
		return nil, err
	}
	tr := tar.NewReader(files)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the files to place: %w", err)
		}
		name, err := placeName(hdr.Name)
		if err != nil {
			return nil, err
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
		default:
			return nil, fmt.Errorf("refused: %q is of the tar type %q; only files, directories and symlinks are placed", hdr.Name, hdr.Typeflag)
		}
		// Every parent first, as a directory of the run's own.
		parts := strings.Split(name, "/")
		for i := 1; i < len(parts); i++ {
			if err := dir(strings.Join(parts[:i], "/"), 0o755, hdr.ModTime); err != nil {
				return nil, err
			}
		}
		if hdr.Typeflag == tar.TypeDir {
			if t, ok := written[name]; ok && t == tar.TypeDir {
				// Named after a path under it: its own entry sets its mode.
				if err := header(path.Join(workDir, name)+"/", tar.TypeDir, hdr.Mode, 0, "", hdr.ModTime); err != nil {
					return nil, err
				}
				continue
			}
			if err := dir(name, hdr.Mode, hdr.ModTime); err != nil {
				return nil, err
			}
			continue
		}
		if _, ok := written[name]; ok {
			return nil, fmt.Errorf("refused: %q is given twice", name)
		}
		written[name] = hdr.Typeflag
		var size int64
		if hdr.Typeflag == tar.TypeReg {
			size = hdr.Size
		}
		if err := header(path.Join(workDir, name), hdr.Typeflag, hdr.Mode, size, hdr.Linkname, hdr.ModTime); err != nil {
			return nil, err
		}
		if size > 0 {
			if _, err := io.Copy(tw, tr); err != nil {
				return nil, fmt.Errorf("reading %q: %w", name, err)
			}
		}
	}

	if err := header(instructionPath, tar.TypeReg, 0o644, int64(len(instruction)), "", time.Now()); err != nil {
		return nil, err
	}
	if _, err := tw.Write(instruction); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// placeName judges a name in the caller's tar and returns it cleaned.
func placeName(name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("refused: %q is an absolute name", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("refused: %q has \"..\"", name)
		}
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", fmt.Errorf("refused: %q names no file", name)
	}
	return clean, nil
}

// StartRun starts a run's container. It never starts one a second time: a
// container that has started already, whether it still runs or has exited, is
// left as it is, so that StartRun can be repeated.
//
// Before it starts a container that has never started, it starts the run's
// way out when that is stopped, as after a restart of the engine; one that
// runs is left as it is. A container with the label kiln.hosts whose way out
// is gone is refused, and nothing is started or removed.
func StartRun(ctx context.Context, id string) error {
	c, err := existingRun(ctx, id)
	if err != nil {
		return err
	}
	if c.state != "created" {
		return nil
	}
	found, err := startWayOut(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		withHosts, err := hasHosts(ctx, c.id)
		if err != nil {
			return err
		}
		if withHosts {
			return fmt.Errorf("the way out %s is gone; the run is left alone", wayOutName(id))
		}
	}
	_, err = docker(ctx, nil, "start", c.id)
	return err
}

// AttachProxy attaches the proxy to the network of a run, for instance after
// ReplaceProxy has replaced it. It does nothing when the proxy is attached
// already. A proxy without the label kiln.proxy is refused.
func AttachProxy(ctx context.Context, id, proxy string) error {
	if err := checkRunID(id); err != nil {
		return err
	}
	name := runName(id)
	n, err := findNetwork(ctx, name, runLabel)
	if err != nil {
		return err
	}
	if n == nil {
		return fmt.Errorf("there is no network %s", name)
	}
	if n.label != id {
		return notOurs("network", name, runLabel+"="+id)
	}
	p, err := kilnProxy(ctx, proxy)
	if err != nil {
		return err
	}
	attached, err := attachedTo(ctx, p.id, name)
	if err != nil || attached {
		return err
	}
	_, err = docker(ctx, nil, "network", "connect", n.id, p.id)
	return err
}

// ReadRun writes a run's output to stdout and stderr, as the container wrote
// it, without timestamps (ADR 0038). With follow, it goes on until the
// container stops.
func ReadRun(ctx context.Context, id string, follow bool, stdout, stderr io.Writer) error {
	c, err := existingRun(ctx, id)
	if err != nil {
		return err
	}
	args := []string{"logs"}
	if follow {
		args = append(args, "--follow")
	}
	cmd := commandIn(ctx, "docker", append(args, c.id)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker logs: %w", err)
	}
	return nil
}

// RunState is what InspectRun reads of a run's container.
type RunState struct {
	Status     string // created, running, exited, ...
	Labels     map[string]string
	ExitCode   int
	OOMKilled  bool
	StartedAt  time.Time // zero when it has not started
	FinishedAt time.Time // zero when it has not ended
}

// InspectRun reads the state of a run's container. It returns nil when there
// is no container.
func InspectRun(ctx context.Context, id string) (*RunState, error) {
	c, err := findRun(ctx, id)
	if err != nil || c == nil {
		return nil, err
	}
	out, err := docker(ctx, nil, "inspect", "--type", "container", c.id)
	if err != nil {
		return nil, err
	}
	var got []struct {
		State struct {
			Status                string
			ExitCode              int
			OOMKilled             bool
			StartedAt, FinishedAt time.Time
		}
		Config struct{ Labels map[string]string }
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 1 {
		return nil, fmt.Errorf("reading docker inspect of %s: %v", runName(id), err)
	}
	g := got[0]
	if g.Config.Labels[runLabel] != id {
		return nil, notOurs("container", runName(id), runLabel+"="+id)
	}
	// The engine writes 0001-01-01T00:00:00Z for a time that has not come,
	// which is Go's zero time.
	return &RunState{
		Status:     g.State.Status,
		Labels:     g.Config.Labels,
		ExitCode:   g.State.ExitCode,
		OOMKilled:  g.State.OOMKilled,
		StartedAt:  g.State.StartedAt,
		FinishedAt: g.State.FinishedAt,
	}, nil
}

// StopRun stops a run's container, with docker's own grace period. A container
// that has stopped already is left as it is.
func StopRun(ctx context.Context, id string) error {
	c, err := existingRun(ctx, id)
	if err != nil {
		return err
	}
	_, err = docker(ctx, nil, "stop", c.id)
	return err
}

// CopyFromRun writes a tar of a path in a run's container to w, as the engine
// hands it over. The path is absolute.
func CopyFromRun(ctx context.Context, id, from string, w io.Writer) error {
	if !strings.HasPrefix(from, "/") {
		return fmt.Errorf("the path %q in a run is not absolute", from)
	}
	c, err := existingRun(ctx, id)
	if err != nil {
		return err
	}
	return dockerIO(ctx, nil, w, "cp", c.id+":"+from, "-")
}

// RemoveRun removes the save container kiln-save-<id>, then the run's container,
// then its way out kiln-wayout-<id>, each by its ID with `rm --force`, then the
// network with RemoveRunNetwork. Anything missing is fine, so that it can be
// repeated (ADR 0035). Anything without the label kiln.run=<id> is refused, and
// nothing of it is touched.
func RemoveRun(ctx context.Context, id, proxy string) error {
	if err := checkRunID(id); err != nil {
		return err
	}
	for _, name := range []string{saveName(id), runName(id), wayOutName(id)} {
		c, err := findContainer(ctx, name, runLabel)
		if err != nil {
			return err
		}
		if c == nil {
			continue
		}
		if c.label != id {
			return notOurs("container", name, runLabel+"="+id)
		}
		if _, err := docker(ctx, nil, "rm", "--force", c.id); err != nil {
			return err
		}
	}
	return RemoveRunNetwork(ctx, id, proxy)
}
