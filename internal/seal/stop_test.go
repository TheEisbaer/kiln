package seal

import (
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"testing"
)

// stopped is set once the test program is told to stop by SIGINT, SIGTERM
// or SIGHUP, as check/in-image tells it when it is stopped itself. The cases
// that run then go on to their end and their cleanups, and a case that has
// not begun fails before it makes anything (failIfStopped): nothing is left
// on the engine, and the program ends soon.
var stopped atomic.Bool

// notifyStop sets stopped on the first of those signals.
func notifyStop() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-c
		stopped.Store(true)
	}()
}

// failIfStopped fails a case that begins after the program was told to stop.
func failIfStopped(t *testing.T) {
	t.Helper()
	if stopped.Load() {
		t.Fatal("the test program was told to stop")
	}
}
