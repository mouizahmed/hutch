//go:build linux

package keys

import (
	"net"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// listenIn listens on addr inside the network namespace at ns. The socket stays
// in that namespace; the thread that made it goes back to hutchd's, or, if it
// can't, ends with its goroutine.
func listenIn(ns, addr string) (net.Listener, error) {
	type result struct {
		l   net.Listener
		err error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		self, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			runtime.UnlockOSThread()
			done <- result{err: err}
			return
		}
		defer self.Close()
		target, err := os.Open(ns)
		if err != nil {
			runtime.UnlockOSThread()
			done <- result{err: err}
			return
		}
		defer target.Close()
		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			done <- result{err: err}
			return
		}
		l, err := net.Listen("tcp", addr)
		if unix.Setns(int(self.Fd()), unix.CLONE_NEWNET) == nil {
			runtime.UnlockOSThread()
		}
		done <- result{l, err}
	}()
	r := <-done
	return r.l, r.err
}
