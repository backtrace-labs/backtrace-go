//go:build darwin

package bt

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// machineInfoBudget bounds the one-time ioreg probe.
const machineInfoBudget = time.Second

// collectMachineInfo gathers macOS machine metadata via sysctl — native
// syscalls only, no subprocesses.
func collectMachineInfo(attrs map[string]interface{}, d diag) {
	if v, err := unix.Sysctl("machdep.cpu.brand_string"); err == nil && v != "" {
		attrs["cpu.brand"] = v
	}
	if v, err := unix.Sysctl("kern.osproductversion"); err == nil && v != "" {
		attrs["uname.version"] = v
	}
}

// collectMachineGUID resolves the platform UUID (opt-in via
// Config.SendMachineID). This is the only machine probe that spawns a
// subprocess — one bounded ioreg invocation with arguments, never a shell
// pipeline — and it runs only after a client opts in.
func collectMachineGUID(d diag) string {
	ctx, cancel := context.WithTimeout(context.Background(), machineInfoBudget)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		d.logf("machine identifier probe (ioreg) failed: %v", err)
		return ""
	}
	return parseIOPlatformUUID(string(out))
}

// parseIOPlatformUUID extracts the quoted IOPlatformUUID value from ioreg
// output ("IOPlatformUUID" = "XXXX-...").
func parseIOPlatformUUID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "IOPlatformUUID") {
			continue
		}
		_, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"`)
	}
	return ""
}
