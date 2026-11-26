//go:build !unix

package bt

import "os"

const readNonBlocking = os.O_RDONLY

func osSignalName(s os.Signal) string {
	return safeSprint(s)
}
