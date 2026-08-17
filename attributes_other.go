//go:build !linux && !darwin && !freebsd && !windows

package bt

// collectMachineInfo has no platform-specific sources here; reports carry
// the portable attributes only.
func collectMachineInfo(attrs map[string]interface{}, d diag) {}

// collectMachineGUID has no source on this platform.
func collectMachineGUID(d diag) string { return "" }
