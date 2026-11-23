//go:build unix

package bt

import (
	"os"
	"syscall"
)

const readNonBlocking = os.O_RDONLY | syscall.O_NONBLOCK
