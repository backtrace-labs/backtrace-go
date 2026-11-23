//go:build windows

package bt

import (
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// signal.Ignore leaves console control events to the default handler. That handler ends the process.
func ignoreGroupSignals() {
	signal.Notify(make(chan os.Signal, 1), os.Interrupt, syscall.SIGTERM)
}

func passFiles(cmd *exec.Cmd, files ...*os.File) (string, error) {
	handles := make([]syscall.Handle, len(files))
	ids := make([]string, len(files))
	for i, f := range files {
		h := syscall.Handle(f.Fd())
		if err := syscall.SetHandleInformation(h, syscall.HANDLE_FLAG_INHERIT, syscall.HANDLE_FLAG_INHERIT); err != nil {
			return "", err
		}
		handles[i] = h
		ids[i] = strconv.FormatUint(uint64(h), 10)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, AdditionalInheritedHandles: handles}
	return strings.Join(ids, ","), nil
}
