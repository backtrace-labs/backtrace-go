//go:build freebsd

package bt

import (
	"strings"

	"golang.org/x/sys/unix"
)

// collectMachineInfo gathers FreeBSD machine metadata via sysctl (native
// syscalls) and /etc/os-release — no subprocesses.
func collectMachineInfo(attrs map[string]interface{}, d diag) {
	if v, err := unix.Sysctl("hw.model"); err == nil && v != "" {
		attrs["cpu.brand"] = v
	}

	if data, err := readSmallFile("/etc/os-release", 16<<10); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok && k == "VERSION" {
				attrs["uname.version"] = strings.Trim(strings.TrimSpace(v), `"`)
				break
			}
		}
	}
}

// collectMachineGUID resolves the host UUID (opt-in via Config.SendMachineID).
func collectMachineGUID(d diag) string {
	if v, err := unix.Sysctl("kern.hostuuid"); err == nil {
		return v
	}
	return ""
}
