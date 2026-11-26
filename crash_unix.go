//go:build unix

package bt

import (
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

func ignoreGroupSignals() {
	signal.Ignore(os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
}

func passFiles(cmd *exec.Cmd, files ...*os.File) (string, error) {
	cmd.ExtraFiles = files
	ids := make([]string, len(files))
	for i := range files {
		ids[i] = strconv.Itoa(3 + i)
	}
	return strings.Join(ids, ","), nil
}
