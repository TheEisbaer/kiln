// Package launcher is `kiln launch` (ADR 0036): it stays the core's parent,
// starts `kiln core` with its own arguments, starts it again after a crash,
// and judges a new core that replaces the running one by its report.
//
// The launcher does nothing else (ADR 0036, rule 1). It parses none of the
// core's flags, opens nothing in the data directory or the store, runs no
// command but the core, serves nothing and builds nothing. Its test reads the
// imports of this package to keep it so.
//
// Each start of a core gets a socket of its own, as its file descriptor 3,
// named by KILN_LAUNCHER=3 (see Client). On it the core writes lines:
//
//   - healthy: the core's report, after one full comparison of the store,
//     the container engine and the Git host with the screen served
//     (ADR 0036, rule 2).
//   - replace <absolute path of a program>: the core asks to be replaced by
//     that program, and then exits.
//
// A core that has reported is the running one. One that ends with 0 was
// ended on purpose, and so is the launcher; one that ends otherwise has
// crashed, and is started again after a back-off. A core that ends before
// its report ends the launcher, so that a core that cannot start stops
// Kiln, except while the previous program falls back after a failed update.
//
// A replacement is started on probation: it fails when it cannot be started,
// ends before its report, or does not report within healthLimit. The
// previous program is then started with KILN_UPDATE_FAILED, the reason, and
// KILN_UPDATE_FAILED_PROGRAM, the program that failed, until it reports.
package launcher

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// The variables between the launcher and a core. A core's environment is the
// launcher's without them, with the ones the launcher sets.
const (
	launcherVar      = "KILN_LAUNCHER"
	failedVar        = "KILN_UPDATE_FAILED"
	failedProgramVar = "KILN_UPDATE_FAILED_PROGRAM"
)

// coreFD is the core's file descriptor of its socket, the first of ExtraFiles.
const coreFD = 3

// maxLine is the longest line a core writes to the launcher.
const maxLine = 4 << 10

// The times of the launcher. Only the test changes them.
var (
	// healthLimit is how long a core on probation has for its report.
	healthLimit = 2 * time.Minute
	// killGrace is the time from SIGTERM to SIGKILL for a core on probation
	// that did not report in time.
	killGrace = 10 * time.Second
	// stopGrace is the time a core has to end after the launcher's SIGTERM,
	// or after its accepted request to be replaced, before SIGKILL.
	stopGrace = 30 * time.Second
	// The back-off before a crashed core is started again: backoffBase,
	// doubled with each crash up to backoffCap, and backoffBase again once a
	// core has run backoffReset after its report.
	backoffBase  = 1 * time.Second
	backoffCap   = 60 * time.Second
	backoffReset = 10 * time.Minute
	// drainLimit bounds the wait, after a core has exited, for the lines it
	// wrote before.
	drainLimit = time.Second
)

// fallbackStarts is how often the previous program is started again while it
// falls back, after a death by a signal before its report.
const fallbackStarts = 3

// uid is the user whose program a replacement must be. Only the test changes
// it.
var uid = os.Getuid

const usage = `usage:
  kiln launch <the flags of kiln core>   (starts kiln core with them, and starts it again after a crash)
`

// Main is `kiln launch <the flags of kiln core>`: it starts this program's
// `kiln core` with the same arguments, and returns the status the launcher
// ends with. SIGINT, SIGTERM and SIGHUP end it after the core.
func Main(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	program, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "kiln launch: its own program:", err)
		return 1
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	return Run(program, args, stderr, signals)
}

// mode is how a core was started, which decides what its end means.
type mode int

const (
	// normal is a first start, or a start after a crash.
	normal mode = iota
	// probation is a replacement until its report.
	probation
	// fallingBack is the previous program after a failed update, until its
	// report.
	fallingBack
)

// launcher is one `kiln launch`.
type launcher struct {
	args    []string
	stderr  io.Writer
	signals <-chan os.Signal
}

// Run starts `<program> core <args>` and supervises it until the launcher
// ends, and returns the status it ends with. A signal on signals ends the
// running core and the launcher.
func Run(program string, args []string, stderr io.Writer, signals <-chan os.Signal) int {
	l := &launcher{args: args, stderr: stderr, signals: signals}
	return l.loop(program)
}

func (l *launcher) say(format string, a ...any) {
	fmt.Fprintf(l.stderr, "kiln launch: "+format+"\n", a...)
}

// loop starts the cores one after another, and returns the launcher's status.
func (l *launcher) loop(program string) int {
	m := normal
	var previous string // the program before a core on probation
	var extra []string  // what a falling-back core is told
	crashes, deaths := 0, 0
	for {
		c, err := l.start(program, m, extra)
		if err != nil {
			if m == probation {
				reason := fmt.Sprintf("the new core %s could not be started: %v", program, err)
				l.say("%s; the previous program %s is started", reason, previous)
				program, m, extra, deaths = previous, fallingBack, failedEnv(reason, program), 0
				continue
			}
			l.say("the core %s could not be started: %v; Kiln has stopped", program, err)
			return 1
		}
		e := l.watch(c, m)
		switch {
		case e.stopped:
			return e.code
		case e.replace != "":
			l.say("the core %s is replaced by %s, on probation for %s", program, e.replace, healthLimit)
			previous, program, m, extra = program, e.replace, probation, nil
			crashes = 0
			continue
		case !e.reported && m == probation:
			reason := fmt.Sprintf("the new core %s %s before it reported healthy", program, e.how)
			if e.timedOut {
				reason = fmt.Sprintf("the new core %s did not report healthy within %s", program, healthLimit)
			}
			l.say("%s; the previous program %s is started", reason, previous)
			program, m, extra, deaths = previous, fallingBack, failedEnv(reason, program), 0
			continue
		case !e.reported && m == fallingBack && e.signal != 0 && deaths < fallbackStarts:
			deaths++
			wait := backoff(crashes)
			crashes++
			l.say("the previous program %s %s before it reported healthy; started again in %s, %d of %d", program, e.how, wait, deaths, fallbackStarts)
			if !l.sleep(wait) {
				return 0
			}
			continue
		case !e.reported:
			l.say("the core %s ended with %s before it reported healthy; Kiln has stopped", program, e.status)
			return e.code
		case e.code == 0 && !e.refused:
			l.say("the core %s ended on purpose; Kiln has stopped", program)
			return 0
		}
		// A crash after the report, or an end after a refused request to be
		// replaced, which is never an end on purpose.
		if e.ran >= backoffReset {
			crashes = 0
		}
		wait := backoff(crashes)
		crashes++
		l.say("the core %s crashed; started again in %s", program, wait)
		m, extra = normal, nil
		if !l.sleep(wait) {
			return 0
		}
	}
}

// failedEnv is what a falling-back program is told of the failed update.
func failedEnv(reason, program string) []string {
	return []string{failedVar + "=" + reason, failedProgramVar + "=" + program}
}

// backoff is the wait before a core is started again after a crash, with
// crashes before it since the last reset.
func backoff(crashes int) time.Duration {
	wait := backoffBase
	for i := 0; i < crashes && wait < backoffCap; i++ {
		wait *= 2
	}
	return min(wait, backoffCap)
}

// sleep waits d, and reports false when a signal came meanwhile.
func (l *launcher) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case s := <-l.signals:
		l.say("%s while waiting; Kiln has stopped", signalName(s))
		return false
	}
}

// core is one start of a core.
type core struct {
	program string
	cmd     *exec.Cmd
	ours    *os.File      // the launcher's end of the socket
	lines   chan string   // the lines the core wrote; closed at the socket's end
	done    chan struct{} // closed once the core has exited
	started time.Time
}

// start starts `<program> core <args>` with its own socket as file
// descriptor 3.
func (l *launcher) start(program string, m mode, extra []string) (*core, error) {
	ours, theirs, err := socketPair()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(program, append([]string{"core"}, l.args...)...)
	cmd.Env = append(environment(), launcherVar+"="+strconv.Itoa(coreFD))
	cmd.Env = append(cmd.Env, extra...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{theirs}
	err = cmd.Start()
	theirs.Close()
	if err != nil {
		ours.Close()
		return nil, err
	}
	c := &core{program: program, cmd: cmd, ours: ours, lines: make(chan string, 16), done: make(chan struct{}), started: time.Now()}
	switch m {
	case probation:
		l.say("started %s on probation (pid %d)", program, cmd.Process.Pid)
	case fallingBack:
		l.say("started the previous program %s after the failed update (pid %d)", program, cmd.Process.Pid)
	default:
		l.say("started %s (pid %d)", program, cmd.Process.Pid)
	}
	go c.read()
	go func() {
		cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

// socketPair makes a Unix stream socket pair, both ends close-on-exec; the
// launcher's end is non-blocking, so that closing it ends a read.
func socketPair() (ours, theirs *os.File, err error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, fmt.Errorf("the socket to the core: %w", err)
	}
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
		return nil, nil, fmt.Errorf("the socket to the core: %w", err)
	}
	return os.NewFile(uintptr(fds[0]), "the core's socket"), os.NewFile(uintptr(fds[1]), "the launcher's socket"), nil
}

// environment is the launcher's environment without the variables between
// the launcher and a core.
func environment() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := bytes.Cut([]byte(kv), []byte("="))
		switch string(name) {
		case launcherVar, failedVar, failedProgramVar:
			continue
		}
		env = append(env, kv)
	}
	return env
}

// read hands on the core's lines until its end of the socket is closed. A
// line longer than maxLine is handed on as its first maxLine bytes, marked
// as cut, so that it is ignored.
func (c *core) read() {
	defer close(c.lines)
	r := bufio.NewReaderSize(c.ours, maxLine+1)
	for {
		line, err := r.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			c.lines <- "\x00" + string(line[:maxLine])
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			if err != nil {
				return
			}
		case err != nil:
			return
		default:
			c.lines <- string(bytes.TrimSuffix(line, []byte("\n")))
		}
	}
}

// end is how a core ended.
type end struct {
	code     int            // its status, 128 + the signal's number for a signal
	signal   syscall.Signal // the signal it died by, or 0
	status   string         // "exit status <n>", or the signal's name
	how      string         // "exited with exit status <n>", or "was killed by <signal>"
	reported bool           // it had reported healthy
	ran      time.Duration  // how long it ran after its report
	timedOut bool           // on probation, it did not report in time
	replace  string         // the program of its accepted request to be replaced
	refused  bool           // a request of its to be replaced was refused
	stopped  bool           // a signal ended the launcher
}

// watch follows a core until it has exited, and says how it ended.
func (l *launcher) watch(c *core, m mode) end {
	var e end
	var reportedAt time.Time
	var health, kill <-chan time.Time
	if m == probation {
		t := time.NewTimer(healthLimit)
		defer t.Stop()
		health = t.C
	}
	signals := l.signals
	lines := c.lines
	pid := c.cmd.Process.Pid
	killLater := func(d time.Duration) {
		if kill == nil {
			t := time.NewTimer(d)
			kill = t.C
		}
	}
	handle := func(line string) {
		switch {
		case line == "healthy" && e.timedOut:
			// The launcher has already judged this core; a late report
			// does not overturn that.
			l.say("the core %s (pid %d) reported healthy after its time ran out; ignored", c.program, pid)
		case line == "healthy" && !e.reported:
			e.reported, reportedAt = true, time.Now()
			l.say("the core %s (pid %d) reported healthy", c.program, pid)
		case line == "healthy":
		case len(line) > len("replace ") && line[:len("replace ")] == "replace ":
			path := line[len("replace "):]
			if why := l.refuse(path, e.reported); why != "" {
				l.say("refused to replace the core by %s: %s", strconv.Quote(path), why)
				e.refused = true
				return
			}
			if e.stopped {
				return
			}
			e.replace = path
			l.say("the core %s (pid %d) asks to be replaced by %s", c.program, pid, path)
			killLater(stopGrace)
		default:
			l.say("the core %s (pid %d) wrote a line it does not know, ignored: %s", c.program, pid, strconv.Quote(line))
		}
	}
loop:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			handle(line)
		case <-c.done:
			break loop
		case <-health:
			health = nil
			if e.reported {
				continue
			}
			e.timedOut = true
			l.say("the core %s (pid %d) did not report healthy within %s; it is ended", c.program, pid, healthLimit)
			c.cmd.Process.Signal(syscall.SIGTERM)
			killLater(killGrace)
		case s := <-signals:
			signals = nil
			e.stopped, e.replace = true, ""
			l.say("%s; the core %s (pid %d) is ended, and Kiln stops", signalName(s), c.program, pid)
			c.cmd.Process.Signal(syscall.SIGTERM)
			kill = nil
			killLater(stopGrace)
		case <-kill:
			kill = nil
			l.say("the core %s (pid %d) did not end in time; it is killed", c.program, pid)
			c.cmd.Process.Signal(syscall.SIGKILL)
		}
	}
	// The lines the core wrote before it exited come before its end.
	if lines != nil {
		timer := time.NewTimer(drainLimit)
	drain:
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					break drain
				}
				handle(line)
			case <-timer.C:
				break drain
			}
		}
		timer.Stop()
	}
	c.ours.Close()
	for range c.lines {
	}
	if e.reported {
		e.ran = time.Since(reportedAt)
	}
	state := c.cmd.ProcessState
	e.code, e.status, e.how = state.ExitCode(), state.String(), "exited with "+state.String()
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		e.signal = ws.Signal()
		e.code = 128 + int(e.signal)
		e.status = signalName(e.signal)
		e.how = "was killed by " + e.status
	}
	l.say("the core %s (pid %d) %s", c.program, pid, e.how)
	if e.stopped {
		e.replace = ""
	}
	return e
}

// refuse says why a core's request to be replaced by path is refused, or
// nothing: it is accepted only from a core that has reported, for an
// absolute path to a regular file, not a symlink, of the launcher's user.
func (l *launcher) refuse(path string, reported bool) string {
	switch {
	case !reported:
		return "the core has not reported healthy"
	case !filepath.IsAbs(path):
		return "the path is not absolute"
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err.Error()
	}
	if !fi.Mode().IsRegular() {
		return "it is not a regular file"
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "its owner cannot be read"
	}
	if int(st.Uid) != uid() {
		return fmt.Sprintf("its owner is uid %d, not the launcher's %d", st.Uid, uid())
	}
	return ""
}

// signalNames are the names of the signals a core is likely to end by.
var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT", syscall.SIGILL: "SIGILL",
	syscall.SIGTRAP: "SIGTRAP", syscall.SIGABRT: "SIGABRT", syscall.SIGBUS: "SIGBUS", syscall.SIGFPE: "SIGFPE",
	syscall.SIGKILL: "SIGKILL", syscall.SIGSEGV: "SIGSEGV", syscall.SIGPIPE: "SIGPIPE", syscall.SIGALRM: "SIGALRM",
	syscall.SIGTERM: "SIGTERM", syscall.SIGUSR1: "SIGUSR1", syscall.SIGUSR2: "SIGUSR2",
}

// signalName is a signal's name, such as SIGKILL.
func signalName(s os.Signal) string {
	if sig, ok := s.(syscall.Signal); ok {
		if name, ok := signalNames[sig]; ok {
			return name
		}
		return "signal " + strconv.Itoa(int(sig))
	}
	return s.String()
}
