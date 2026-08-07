//go:build linux

package bt

import "strings"

// collectMachineInfo gathers Linux machine metadata from procfs and
// standard system files — no subprocesses.
func collectMachineInfo(attrs map[string]interface{}, d diag) {
	if data, err := readSmallFile("/proc/cpuinfo", 64<<10); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if key, value, ok := strings.Cut(line, ":"); ok &&
				strings.TrimSpace(key) == "model name" {
				attrs["cpu.brand"] = strings.TrimSpace(value)
				break
			}
		}
	}

	if version := osReleaseValue("VERSION"); version != "" {
		attrs["uname.version"] = version
	}
}

// collectMachineGUID reads the stable machine identifier (opt-in via
// Config.SendMachineID) from the standard machine-id files.
func collectMachineGUID(d diag) string {
	return firstExistingFileLine("/etc/machine-id", "/var/lib/dbus/machine-id")
}

// osReleaseValue extracts a key from /etc/os-release.
func osReleaseValue(key string) string {
	data, err := readSmallFile("/etc/os-release", 16<<10)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k == key {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}
