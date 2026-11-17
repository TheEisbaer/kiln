package seal

import (
	"archive/tar"
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fixture is what TestRun has set up for the cases in this file.
type fixture struct {
	runImage, upstreamImage, proxyImage string
	proxy                               Proxy
	probe                               []byte
	name, runID                         func(string) string
}

// placedExtra is placed with the probe in the probe run: a file in a directory
// that the tar names no entry for, and a symlink.
var placedExtra = []tarEntry{
	file("dir/sub/file", 0o600, "in a directory\n"),
	symlink("link", "probe.json"),
}

// testPlacing judges what the probe run's probe listed under /work and /kiln,
// and what `docker diff` showed before the start.
func testPlacing(t *testing.T, report probeReport, diff string) {
	want := map[string]string{
		"/work/probe":          "-rwxr-xr-x",
		"/work/probe.json":     "-rw-r--r--",
		"/work/dir":            "drwxr-xr-x",
		"/work/dir/sub":        "drwxr-xr-x",
		"/work/dir/sub/file":   "-rw-------",
		"/work/link":           "Lrwxrwxrwx",
		"/kiln/instruction.md": "-rw-r--r--",
		"/kiln/out":            "drwxr-xr-x", // the run image's
	}
	seen := map[string]bool{}
	for _, e := range report.Entries {
		seen[e.Path] = true
		mode, ok := want[e.Path]
		switch {
		case e.Error != "":
			t.Errorf("the listing: %s: %s", e.Path, e.Error)
		case !ok:
			t.Errorf("the listing: %s (%s) was not placed", e.Path, e.Mode)
		case e.Mode != mode:
			t.Errorf("the listing: %s has the mode %s, want %s", e.Path, e.Mode, mode)
		}
		if e.Uid != runUID || e.Gid != runUID {
			t.Errorf("the listing of owners: %s is owned by %d:%d, want 1000:1000", e.Path, e.Uid, e.Gid)
		}
	}
	for p := range want {
		if !seen[p] {
			t.Errorf("the listing: %s is missing", p)
		}
	}
	for _, e := range report.Entries {
		if e.Path == "/work/link" && e.Link != "probe.json" {
			t.Errorf("the listing: /work/link points to %q, want probe.json", e.Link)
		}
	}

	if !strings.Contains(diff, "A /work/probe\n") || !strings.Contains(diff, "A /kiln/instruction.md") {
		t.Errorf("docker diff does not show the placed files:\n%s", diff)
	}
	for _, line := range strings.Split(diff, "\n") {
		_, p, _ := strings.Cut(line, " ")
		if p != "/work" && p != "/kiln" && !strings.HasPrefix(p, "/work/") && !strings.HasPrefix(p, "/kiln/") {
			t.Errorf("docker diff shows a change outside /work and /kiln: %q", line)
		}
	}
}

// testSaveContainer runs the probe once in a container made by createSave, and
// asserts what it finds: what a run's probe finds, and no connection at all.
func testSaveContainer(t *testing.T, f fixture, dial []string) {
	id := f.runID("saveprobe")
	cleanupRun(t, Run{ID: id, Proxy: f.proxy.Name})
	files := []string{"/proc/self/status", "/proc/self/mountinfo"}
	for _, c := range cgroupFiles {
		files = append(files, "/sys/fs/cgroup/"+c)
	}
	config, err := json.Marshal(probeConfig{Dial: dial, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	save, err := createSave(t.Context(), id, f.runImage, []string{"/probe", "/probe.json"})
	if err != nil {
		t.Fatal(err)
	}
	in := tarOf(t,
		tarEntry{hdr: tar.Header{Typeflag: tar.TypeReg, Name: "probe", Mode: 0o755, Size: int64(len(f.probe)), Uid: runUID, Gid: runUID}, data: string(f.probe)},
		tarEntry{hdr: tar.Header{Typeflag: tar.TypeReg, Name: "probe.json", Mode: 0o644, Size: int64(len(config)), Uid: runUID, Gid: runUID}, data: string(config)})
	if err := dockerIO(t.Context(), bytes.NewReader(in), nil, "cp", "--archive", "-", save+":/"); err != nil {
		t.Fatal(err)
	}
	must(t, "start", save)
	if status := must(t, "wait", save); status != "0" {
		t.Errorf("the save container's probe exited with %s", status)
	}
	var out strings.Builder
	if err := dockerIO(t.Context(), nil, &out, "logs", save); err != nil {
		t.Fatal(err)
	}
	var report probeReport
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("the probe's report: %v\n%s", err, out.String())
	}

	status := statusFields(t, fileIn(t, report, "/proc/self/status"))
	for _, field := range []string{"Uid", "Gid"} {
		if got := strings.Fields(status[field]); len(got) != 4 || slices.ContainsFunc(got, func(v string) bool { return v != "1000" }) {
			t.Errorf("%s is %q, want 1000 throughout", field, status[field])
		}
	}
	for _, field := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		if v, err := strconv.ParseUint(status[field], 16, 64); err != nil || v != 0 {
			t.Errorf("%s is %q, want 0", field, status[field])
		}
	}
	if status["NoNewPrivs"] != "1" {
		t.Errorf("NoNewPrivs is %q, want 1", status["NoNewPrivs"])
	}
	assertLimits(t, report)
	for _, problem := range mountProblems(fileIn(t, report, "/proc/self/mountinfo"), "") {
		t.Error(problem)
	}

	if len(report.Dial) != len(dial) {
		t.Fatalf("the probe tried %d addresses, want %d", len(report.Dial), len(dial))
	}
	for _, d := range report.Dial {
		if d.Errno != linuxENETUNREACH && d.Errno != linuxEHOSTUNREACH {
			t.Errorf("the probe's targets: %s: want a failure at once as unreachable, got %q (error number %d)", d.What, d.Value+d.Error, d.Errno)
		}
	}

	if got := must(t, "inspect", "--format", "{{.HostConfig.NetworkMode}} {{json .Mounts}} {{index .Config.Labels \""+runLabel+"\"}}", save); got != "none [] "+id {
		t.Errorf("the save container has network, mounts and label %q, want none, [] and %s", got, id)
	}
	// No environment beyond the image's own.
	if got, image := must(t, "inspect", "--format", "{{json .Config.Env}}", save),
		must(t, "image", "inspect", "--format", "{{json .Config.Env}}", f.runImage); got != image {
		t.Errorf("the save container's environment is %s, the image's %s", got, image)
	}
}

// cgroupFiles are the limits a probe reads, under /sys/fs/cgroup.
var cgroupFiles = []string{"memory.max", "memory.swap.max", "cpu.max", "pids.max"}

// assertLimits asserts the run's limits from the cgroup files a probe read.
func assertLimits(t *testing.T, report probeReport) {
	t.Helper()
	want := map[string]string{"memory.max": "4294967296", "pids.max": "1024", "memory.swap.max": "0"}
	for _, f := range cgroupFiles {
		r := resultFor(report.Files, "/sys/fs/cgroup/"+f)
		if r == nil || r.Error != "" {
			// Whether the runner has these files is a question for the
			// developer (spec of card 3): fail and name it, never drop it.
			t.Errorf("/sys/fs/cgroup/%s cannot be read: %v", f, r)
			continue
		}
		got := strings.TrimSpace(r.Value)
		if f == "cpu.max" {
			quota, period, _ := strings.Cut(got, " ")
			q, qErr := strconv.ParseFloat(quota, 64)
			p, pErr := strconv.ParseFloat(period, 64)
			if qErr != nil || pErr != nil || p == 0 || q/p != 2 {
				t.Errorf("cpu.max is %q, want a quota of twice the period", got)
			}
		} else if got != want[f] {
			t.Errorf("%s is %q, want %s", f, got, want[f])
		}
	}
}

// testAttachProxy attaches the proxy, replaced since the run was created, to
// that run's network, twice, and refuses a proxy that Kiln did not make.
func testAttachProxy(t *testing.T, f fixture, id string) {
	network := runName(id)
	attached := func(proxy string) bool {
		t.Helper()
		ok, err := attachedTo(t.Context(), proxy, network)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if attached(f.proxy.Name) {
		t.Fatal("the replaced proxy is attached already, so this proves nothing")
	}
	for i := range 2 {
		if err := AttachProxy(t.Context(), id, f.proxy.Name); err != nil {
			t.Errorf("AttachProxy, call %d: %v", i+1, err)
		}
		if !attached(f.proxy.Name) {
			t.Errorf("after AttachProxy, call %d: the proxy is not attached", i+1)
		}
	}

	foreign := f.name("foreign-proxy")
	must(t, "create", "--name", foreign, "--network", "none", f.upstreamImage)
	cleanup(t, "rm", "--force", foreign)
	if err := AttachProxy(t.Context(), id, foreign); err == nil {
		t.Error("with a proxy that lacks kiln.proxy: no error")
	}
	if attached(foreign) {
		t.Error("with a proxy that lacks kiln.proxy: it was attached")
	}
}

// testLife exercises StartRun, InspectRun, ReadRun, StopRun and CopyFromRun on
// the exited probe run and on runs of their own.
func testLife(t *testing.T, f fixture, probeID string) {
	t.Run("InspectRun", func(t *testing.T) {
		state, err := InspectRun(t.Context(), probeID)
		if err != nil || state == nil {
			t.Fatalf("%+v, %v", state, err)
		}
		if state.Status != "exited" || state.ExitCode != 0 || state.OOMKilled || state.Labels[runLabel] != probeID {
			t.Errorf("the probe run is %+v", state)
		}
		if state.StartedAt.IsZero() || state.FinishedAt.Before(state.StartedAt) {
			t.Errorf("the probe run started at %v and ended at %v", state.StartedAt, state.FinishedAt)
		}
		if state, err := InspectRun(t.Context(), f.runID("none")); state != nil || err != nil {
			t.Errorf("with no container: %+v, %v", state, err)
		}
	})

	t.Run("StartRun never starts a run twice", func(t *testing.T) {
		before, err := InspectRun(t.Context(), probeID)
		if err != nil {
			t.Fatal(err)
		}
		if err := StartRun(t.Context(), probeID); err != nil {
			t.Errorf("on an exited run: %v", err)
		}
		after, err := InspectRun(t.Context(), probeID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != "exited" || !after.StartedAt.Equal(before.StartedAt) {
			t.Errorf("the exited run was started again: %+v", after)
		}
	})

	t.Run("CopyFromRun", func(t *testing.T) {
		var out bytes.Buffer
		if err := CopyFromRun(t.Context(), probeID, "/kiln", &out); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		tr := tar.NewReader(&out)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(tr)
			got[hdr.Name] = string(data)
		}
		if got["kiln/instruction.md"] != probeInstruction {
			t.Errorf("the tar of /kiln holds %q", got)
		}
		if err := CopyFromRun(t.Context(), probeID, "kiln", io.Discard); err == nil {
			t.Error("with a relative path: no error")
		}
	})

	t.Run("StopRun ends a run and its following", func(t *testing.T) {
		id := f.runID("sleep")
		createRun(t, Run{ID: id, Image: f.runImage, Proxy: f.proxy.Name, Command: []string{"sleep", "300"}})
		for i := range 2 {
			if err := StartRun(t.Context(), id); err != nil {
				t.Fatalf("StartRun, call %d: %v", i+1, err)
			}
		}
		if state, err := InspectRun(t.Context(), id); err != nil || state.Status != "running" {
			t.Fatalf("after StartRun: %+v, %v", state, err)
		}
		followed := make(chan error, 1)
		go func() { followed <- ReadRun(t.Context(), id, true, io.Discard, io.Discard) }()
		select {
		case err := <-followed:
			t.Fatalf("ReadRun with follow ended while the run was running: %v", err)
		case <-time.After(time.Second):
		}
		for i := range 2 {
			if err := StopRun(t.Context(), id); err != nil {
				t.Errorf("StopRun, call %d: %v", i+1, err)
			}
		}
		select {
		case err := <-followed:
			if err != nil {
				t.Errorf("ReadRun with follow: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("ReadRun with follow did not end when the run stopped")
		}
		if state, err := InspectRun(t.Context(), id); err != nil || state.Status != "exited" {
			t.Errorf("after StopRun: %+v, %v", state, err)
		}
	})

	t.Run("a container without kiln.run", func(t *testing.T) {
		id := f.runID("life-foreign")
		cleanupRun(t, Run{ID: id, Proxy: f.proxy.Name})
		must(t, "create", "--name", runName(id), "--network", "none", f.upstreamImage)
		container := containerID(t, runName(id))
		for what, call := range map[string]func() error{
			"PlaceRun":    func() error { return PlaceRun(t.Context(), id, bytes.NewReader(tarOf(t)), nil) },
			"StartRun":    func() error { return StartRun(t.Context(), id) },
			"ReadRun":     func() error { return ReadRun(t.Context(), id, false, io.Discard, io.Discard) },
			"InspectRun":  func() error { _, err := InspectRun(t.Context(), id); return err },
			"StopRun":     func() error { return StopRun(t.Context(), id) },
			"CopyFromRun": func() error { return CopyFromRun(t.Context(), id, "/", io.Discard) },
			"SaveRun": func() error {
				_, _, err := SaveRun(t.Context(), id, f.runImage, strings.Repeat("0", 40), nil)
				return err
			},
		} {
			if err := call(); err == nil {
				t.Errorf("%s: no error", what)
			}
		}
		if got := must(t, "inspect", "--format", "{{.Id}} {{.State.Status}}", runName(id)); got != container+" created" {
			t.Errorf("the container is %q, was %s and created", got, container)
		}
	})
}

// testPlaceRefusals places into a run that has started, and places what
// PlaceRun refuses into one that has not, after a file it accepts.
func testPlaceRefusals(t *testing.T, f fixture, probeID string) {
	late := tarOf(t, file("late.txt", 0o644, "late\n"))
	if err := PlaceRun(t.Context(), probeID, bytes.NewReader(late), nil); err == nil {
		t.Error("on a started container: no error")
	}
	if diff := must(t, "diff", runName(probeID)); strings.Contains(diff, "late.txt") {
		t.Errorf("on a started container: late.txt was placed:\n%s", diff)
	}

	id := f.runID("refuse")
	createRun(t, Run{ID: id, Image: f.runImage, Proxy: f.proxy.Name})
	ok := file("a.txt", 0o644, "a\n")
	dev := file("dev", 0o644, "")
	dev.hdr.Typeflag, dev.hdr.Devmajor, dev.hdr.Devminor = tar.TypeChar, 1, 3
	hard := file("hard", 0o644, "")
	hard.hdr.Typeflag, hard.hdr.Linkname = tar.TypeLink, "a.txt"
	for what, entries := range map[string][]tarEntry{
		"../x":                    {ok, file("../x", 0o644, "x\n")},
		"a/../../x":               {ok, file("a/../../x", 0o644, "x\n")},
		"/x":                      {ok, file("/x", 0o644, "x\n")},
		"a device":                {ok, dev},
		"a hard link":             {ok, hard},
		"a file under a symlink":  {ok, symlink("s", "/etc"), file("s/x", 0o644, "x\n")},
		"a name given twice":      {ok, file("a.txt", 0o644, "again\n")},
		"a file under a file":     {ok, file("a.txt/x", 0o644, "x\n")},
		"a directory over a file": {ok, {hdr: foreign(tar.Header{Typeflag: tar.TypeDir, Name: "a.txt/", Mode: 0o755})}},
	} {
		if err := PlaceRun(t.Context(), id, bytes.NewReader(tarOf(t, entries...)), []byte("instruction\n")); err == nil {
			t.Errorf("with %s: no error", what)
		}
		if diff := must(t, "diff", runName(id)); diff != "" {
			t.Errorf("with %s: something was copied:\n%s", what, diff)
		}
	}
}

// saveRunScript is the command of the run that testSave saves. It runs in
// /work, as uid 1000, on the placed tree of saveCommit.
const saveRunScript = `set -eu
echo two >> tracked.txt
echo new > new.txt
echo junk > debug.log
echo upper > README
echo lower > readme
ln -s tracked.txt link
echo hard > hard.txt
ln hard.txt hardlink.txt
mkfifo fifo
mkdir nested
git -C nested init --quiet
echo n > nested/n.txt
git -C nested add n.txt
git -C nested -c user.name=t -c user.email=t@localhost commit --quiet -m n
mkdir .git
echo junk > .git/HEAD
mkdir -p ro/.git
echo junk > ro/.git/HEAD
echo ro > ro/file.txt
chmod 555 ro
mkdir locked
echo inner > locked/inner.txt
chmod 000 locked
echo zero > zero.txt
chmod 000 zero.txt
`

// saveCommit is the tree the run starts from, with a tracked file under an
// ignored path.
var saveCommit = map[string]string{
	".gitignore":       "ignored/\n*.log\n",
	"tracked.txt":      "one\n",
	"ignored/kept.txt": "kept\n",
}

// testSave saves a run made by CreateRun and checks the tree, then repeats the
// save with a save container left behind, with and without the label.
func testSave(t *testing.T, f fixture) {
	// The commit, in a repo on the host.
	repo := t.TempDir()
	gitIn(t, repo, nil, "init", "--quiet")
	var entries []tarEntry
	for p, data := range saveCommit {
		writeFile(t, repo, p, data)
		entries = append(entries, file(p, 0o644, data))
	}
	gitIn(t, repo, nil, "add", "--force", ".")
	tree := gitIn(t, repo, nil, "write-tree")
	objects := gitIn(t, repo, nil, "rev-list", "--objects", tree)
	base := gitIn(t, repo, strings.NewReader(objects+"\n"), "pack-objects", "--stdout")

	id := f.runID("save")
	createRun(t, Run{ID: id, Image: f.runImage, Proxy: f.proxy.Name, Command: []string{"sh", "-c", saveRunScript}})
	if err := PlaceRun(t.Context(), id, bytes.NewReader(tarOf(t, entries...)), []byte("save\n")); err != nil {
		t.Fatal(err)
	}
	startAndWait(t, id)

	newTree, pack, err := SaveRun(t.Context(), id, f.runImage, tree, []byte(base))
	if err != nil {
		t.Fatalf("SaveRun (the mode-000 case and the read-only .git need chmod first): %v", err)
	}
	gitIn(t, repo, bytes.NewReader(pack), "index-pack", "--stdin", "--strict")
	got := treeOf(t, repo, newTree)

	want := map[string]string{
		".gitignore":       "100644 ignored/\n*.log\n",
		"tracked.txt":      "100644 one\ntwo\n",
		"new.txt":          "100644 new\n",
		"ignored/kept.txt": "100644 kept\n",
		"README":           "100644 upper\n",
		"readme":           "100644 lower\n",
		"link":             "120000 tracked.txt",
		"hard.txt":         "100644 hard\n",
		"hardlink.txt":     "100644 hard\n",
		"nested/n.txt":     "100644 n\n",
		"ro/file.txt":      "100644 ro\n",
		"locked/inner.txt": "100644 inner\n",
		"zero.txt":         "100644 zero\n",
	}
	named := map[string]string{
		"tracked.txt":      "the changed file",
		"new.txt":          "the new file",
		"ignored/kept.txt": "the tracked file under an ignored path",
		"README":           "README next to readme",
		"readme":           "readme next to README",
		"link":             "the symlink, mode 120000",
		"hardlink.txt":     "the hard link, as a copy",
		"zero.txt":         "the mode-000 file",
		"locked/inner.txt": "the file in the mode-000 directory",
		"nested/n.txt":     "the nested repo, as plain files",
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s: %s is %q in the save's tree, want %q", cmp.Or(named[p], p), p, got[p], w)
		}
	}
	for p, g := range got {
		if _, ok := want[p]; ok {
			continue
		}
		switch {
		case p == "debug.log":
			t.Errorf("the new ignored file is in the save's tree")
		case p == "fifo":
			t.Errorf("the fifo is in the save's tree: %q", g)
		case strings.HasPrefix(g, "160000"):
			t.Errorf("the nested repo became a gitlink: %s %q", p, g)
		case p == ".git" || strings.HasPrefix(p, ".git/") || strings.Contains(p, "/.git"):
			t.Errorf("a .git path is in the save's tree: %s", p)
		default:
			t.Errorf("%s is in the save's tree, and was not made: %q", p, g)
		}
	}
	assertNoSave(t, id)

	t.Run("saved twice", func(t *testing.T) {
		again, _, err := SaveRun(t.Context(), id, f.runImage, tree, []byte(base))
		if err != nil || again != newTree {
			t.Errorf("the second save gave %s (%v), the first %s", again, err, newTree)
		}
		assertNoSave(t, id)
	})

	t.Run("after a save container left behind", func(t *testing.T) {
		must(t, "create", "--name", saveName(id), "--label", runLabel+"="+id, "--network", "none", f.runImage, "true")
		left := containerID(t, saveName(id))
		again, _, err := SaveRun(t.Context(), id, f.runImage, tree, []byte(base))
		if err != nil || again != newTree {
			t.Errorf("the save gave %s (%v), the first %s", again, err, newTree)
		}
		if got := containerID(t, saveName(id)); got != "" {
			t.Errorf("a save container %s is left, the leftover was %s", got, left)
		}
	})

	t.Run("after a save container without kiln.run", func(t *testing.T) {
		must(t, "create", "--name", saveName(id), "--network", "none", f.upstreamImage)
		foreign := containerID(t, saveName(id))
		cleanup(t, "rm", "--force", foreign)
		if _, _, err := SaveRun(t.Context(), id, f.runImage, tree, []byte(base)); err == nil {
			t.Error("SaveRun: no error")
		}
		if err := RemoveRun(t.Context(), id, f.proxy.Name); err == nil {
			t.Error("RemoveRun: no error")
		}
		if got := containerID(t, saveName(id)); got != foreign {
			t.Errorf("the foreign save container is %q, was %s", got, foreign)
		}
		assertLeft(t, id, true, true)
	})
}

// testRemoveRun removes runs, twice, and refuses what lacks the label.
func testRemoveRun(t *testing.T, f fixture) {
	t.Run("twice", func(t *testing.T) {
		id := f.runID("remove")
		createRun(t, Run{ID: id, Image: f.runImage, Proxy: f.proxy.Name, Command: []string{"true"}})
		startAndWait(t, id)
		if _, err := createSave(t.Context(), id, f.runImage, []string{"true"}); err != nil {
			t.Fatal(err)
		}
		for i := range 2 {
			if err := RemoveRun(t.Context(), id, f.proxy.Name); err != nil {
				t.Errorf("RemoveRun, call %d: %v", i+1, err)
			}
			assertLeft(t, id, false, false)
			assertNoSave(t, id)
		}
	})

	t.Run("a container without kiln.run", func(t *testing.T) {
		id := f.runID("remove-foreign")
		cleanupRun(t, Run{ID: id, Proxy: f.proxy.Name})
		must(t, "create", "--name", runName(id), "--network", "none", f.upstreamImage)
		container := containerID(t, runName(id))
		if err := RemoveRun(t.Context(), id, f.proxy.Name); err == nil {
			t.Error("no error")
		}
		if got := containerID(t, runName(id)); got != container {
			t.Errorf("the container is %q, was %s", got, container)
		}
	})

	t.Run("a network without kiln.run", func(t *testing.T) {
		id := f.runID("remove-foreign-net")
		cleanupRun(t, Run{ID: id, Proxy: f.proxy.Name})
		must(t, "network", "create", "--internal", runName(id))
		must(t, "network", "connect", runName(id), f.proxy.Name)
		cleanup(t, "network", "disconnect", runName(id), f.proxy.Name) // before cleanupRun's removal
		network := networkID(t, runName(id))
		if err := RemoveRun(t.Context(), id, f.proxy.Name); err == nil {
			t.Error("RemoveRun: no error")
		}
		if err := RemoveRunNetwork(t.Context(), id, f.proxy.Name); err == nil {
			t.Error("RemoveRunNetwork: no error")
		}
		if err := AttachProxy(t.Context(), id, f.proxy.Name); err == nil {
			t.Error("AttachProxy: no error")
		}
		if got := networkID(t, runName(id)); got != network {
			t.Errorf("the network is %q, was %s", got, network)
		}
		if ok, err := attachedTo(t.Context(), f.proxy.Name, runName(id)); err != nil || !ok {
			t.Errorf("the proxy was detached from the foreign network (%v)", err)
		}
	})
}

// testNoPull starts the proxy and creates a save container from an image that
// is not there locally. CreateRun's case is in TestRun's CreateRun.
func testNoPull(t *testing.T, f fixture) {
	missing := f.name("no-such-image")
	p := Proxy{Name: f.name("no-image-proxy"), Image: missing, Network: f.proxy.Network, Upstream: f.proxy.Upstream}
	cleanup(t, "rm", "--force", p.Name)
	if err := StartProxy(t.Context(), p, "unused"); err == nil || !strings.Contains(err.Error(), "No such image") {
		t.Errorf("StartProxy: want the image missing locally, got %v", err)
	}
	if got := containerID(t, p.Name); got != "" {
		t.Errorf("StartProxy left %s", got)
	}

	id := f.runID("no-image-save")
	cleanupRun(t, Run{ID: id, Proxy: f.proxy.Name})
	if _, err := createSave(t.Context(), id, missing, []string{"true"}); err == nil || !strings.Contains(err.Error(), "No such image") {
		t.Errorf("createSave: want the image missing locally, got %v", err)
	}
	assertNoSave(t, id)
}

// assertNoSave asserts that a run has no save container.
func assertNoSave(t *testing.T, id string) {
	t.Helper()
	if got := containerID(t, saveName(id)); got != "" {
		t.Errorf("the save container %s is left: %s", saveName(id), got)
	}
}

// gitIn runs git in a directory on the host and returns its output, trimmed.
func gitIn(t *testing.T, dir string, stdin io.Reader, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = stdin
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	if args[0] == "pack-objects" || args[0] == "cat-file" {
		return string(out)
	}
	return strings.TrimSpace(string(out))
}

// treeOf returns every entry of a tree, recursively, as its mode, a space and
// its content.
func treeOf(t *testing.T, repo, tree string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, line := range strings.Split(gitIn(t, repo, nil, "ls-tree", "-r", tree), "\n") {
		info, p, ok := strings.Cut(line, "\t")
		f := strings.Fields(info)
		if !ok || len(f) != 3 {
			t.Fatalf("cannot read the tree's line %q", line)
		}
		content := ""
		if f[1] == "blob" {
			content = gitIn(t, repo, nil, "cat-file", "blob", f[2])
		}
		got[p] = f[0] + " " + content
	}
	return got
}

// writeFile writes a file under a directory, with its parents.
func writeFile(t *testing.T, dir, name, data string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
