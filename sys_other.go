//go:build !unix

package bt

import "os"

const readNonBlocking = os.O_RDONLY
