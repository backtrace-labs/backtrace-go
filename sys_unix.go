//go:build unix

package bt

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const readNonBlocking = os.O_RDONLY | syscall.O_NONBLOCK

func osSignalName(s os.Signal) string {
	if n, ok := s.(syscall.Signal); ok {
		if name := unix.SignalName(n); name != "" {
			return name
		}
	}
	return safeSprint(s)
}
