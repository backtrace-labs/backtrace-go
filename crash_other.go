//go:build !unix && !windows

package bt

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

func ignoreGroupSignals() {}

func passFiles(*exec.Cmd, ...*os.File) (string, error) {
	return "", fmt.Errorf("%w: crash monitor on %s", ErrUnsupportedPlatform, runtime.GOOS)
}
