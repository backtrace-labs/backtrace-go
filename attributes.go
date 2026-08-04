package bt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// execCommandTimeout bounds every machine-metadata subprocess.
const execCommandTimeout = 2 * time.Second

var (
	windowsGUIDCommand = []string{"reg", "query", `HKEY_LOCAL_MACHINE\Software\Microsoft\Cryptography`, "/v", "MachineGuid"}
	linuxGUIDCommand   = []string{"sh", "-c", "( cat /var/lib/dbus/machine-id /etc/machine-id 2> /dev/null || hostname ) | head -n 1 || :"}
	freebsdGUIDCommand = []string{"sh", "-c", "kenv -q smbios.system.uuid || sysctl -n kern.hostuuid"}
	darwinGUIDCommand  = []string{"sh", "-c", "ioreg -rd1 -c IOPlatformExpertDevice | grep IOPlatformUUID | awk -F'= \"' '{print $2}' | tr -d '\"' | tr -d '\n'"}

	windowsCPUCommand = []string{"wmic", "CPU", "get", "NAME"}
	linuxCPUCommand   = []string{"sh", "-c", "lscpu | grep \"Model name\" | awk -F':' '{print $2}' | sed 's/^[[:space:]]*//'"}
	darwinCPUCommand  = []string{"sh", "-c", "sysctl -n machdep.cpu.brand_string | tr -d '\n'"}
	freebsdCPUCommand = []string{"sh", "-c", "sysctl -n hw.model"}

	linuxOSVersionCommand   = []string{"sh", "-c", "cat /etc/os-release | grep VERSION= | awk -F'=\"' '{print $2}' | tr -d '\"'"}
	darwinOSVersionCommand  = []string{"sh", "-c", "sw_vers | grep ProductVersion | awk -F':' '{print $2}' | tr -d '\t' | tr -d '\n'"}
	freebsdOSVersionCommand = []string{"sh", "-c", "cat /etc/os-release | grep VERSION= | awk -F'=\"' '{print $2}' | tr -d '\"'"}
)

// processSessionID identifies this process instance across its reports.
var processSessionID = uuid4()

var processStart = time.Now()

var (
	staticOnce  sync.Once
	staticCache map[string]interface{}
)

// staticAttributes are cheap, in-process values stamped on every report.
// Computed once: none of them change during the process lifetime.
func staticAttributes() map[string]interface{} {
	staticOnce.Do(func() {
		staticCache = computeStaticAttributes()
	})
	return staticCache
}

func computeStaticAttributes() map[string]interface{} {
	hostname, _ := os.Hostname()
	return map[string]interface{}{
		"backtrace.version":   Version,
		"backtrace.agent":     "backtrace-go",
		"hostname":            hostname,
		"uname.sysname":       runtime.GOOS,
		"cpu.arch":            runtime.GOARCH,
		"cpu.count":           runtime.NumCPU(),
		"process.id":          os.Getpid(),
		"application":         filepath.Base(os.Args[0]),
		"application.session": processSessionID,
		"go.version":          runtime.Version(),
	}
}

// runtimeAttributes captures per-report runtime state. Kept cheap: no forced
// GC, no stop-the-world beyond ReadMemStats' brief pause.
func runtimeAttributes(attrs map[string]interface{}) {
	attrs["runtime.goroutines"] = runtime.NumGoroutine()
	attrs["runtime.gomaxprocs"] = runtime.GOMAXPROCS(0)
	attrs["process.age"] = int64(time.Since(processStart).Seconds())

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	attrs["memory.heap.alloc"] = m.HeapAlloc
	attrs["memory.heap.sys"] = m.HeapSys
	attrs["memory.heap.objects"] = m.HeapObjects
	attrs["gc.count"] = m.NumGC
}

var (
	machineOnce  sync.Once
	machineAttrs map[string]interface{}
)

// machineAttributes gathers machine metadata (GUID, CPU model, OS version)
// by shelling out to platform tools. It runs at most once per process, on
// first use — never at import time — and each command is bounded by
// execCommandTimeout. Failures degrade to missing attributes.
func machineAttributes(d diag) map[string]interface{} {
	machineOnce.Do(func() {
		attrs := map[string]interface{}{}

		var guidCommand, cpuCommand, osCommand []string
		switch runtime.GOOS {
		case "windows":
			guidCommand = windowsGUIDCommand
			cpuCommand = windowsCPUCommand
		case "linux":
			guidCommand = linuxGUIDCommand
			cpuCommand = linuxCPUCommand
			osCommand = linuxOSVersionCommand
		case "darwin":
			guidCommand = darwinGUIDCommand
			cpuCommand = darwinCPUCommand
			osCommand = darwinOSVersionCommand
		case "freebsd":
			guidCommand = freebsdGUIDCommand
			cpuCommand = freebsdCPUCommand
			osCommand = freebsdOSVersionCommand
		}

		if output := execCommand(guidCommand, d); output != "" {
			if runtime.GOOS == "windows" {
				// reg query output:
				// HKEY_LOCAL_MACHINE\Software\Microsoft\Cryptography
				//     MachineGuid    REG_SZ    xxxxxxxx-xxxx-...
				if fields := strings.Fields(output); len(fields) > 0 {
					output = strings.Trim(fields[len(fields)-1], "{}")
				}
			}
			attrs["guid"] = strings.TrimSpace(output)
		}

		if output := execCommand(cpuCommand, d); output != "" {
			if runtime.GOOS == "windows" {
				// wmic output: header line "NAME" then the value.
				if lines := strings.Split(output, "\n"); len(lines) > 1 {
					output = lines[1]
				}
			}
			attrs["cpu.brand"] = strings.TrimSpace(output)
		}

		if output := execCommand(osCommand, d); output != "" {
			attrs["uname.version"] = strings.TrimSpace(output)
		}

		machineAttrs = attrs
	})
	return machineAttrs
}

// execCommand runs command[0] with the remaining arguments and returns its
// stdout, or "" on any failure. A nil/empty command returns "".
func execCommand(command []string, d diag) string {
	if len(command) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), execCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, command[0], command[1:]...).Output()
	if err != nil {
		d.logf("machine attribute command %q failed: %v", command[0], err)
		return ""
	}
	return string(out)
}

var (
	buildInfoOnce    sync.Once
	buildInfoAttrs   map[string]interface{}
	buildInfoModules []string
)

// buildInfoAttributes extracts release metadata embedded by the Go toolchain:
// main module version, VCS revision/time/dirty flag, and the dependency list
// (attached to reports as the "Dependencies" annotation).
func buildInfoAttributes() (map[string]interface{}, []string) {
	buildInfoOnce.Do(func() {
		attrs := map[string]interface{}{}
		info, ok := debug.ReadBuildInfo()
		if !ok {
			buildInfoAttrs = attrs
			return
		}
		if v := info.Main.Version; v != "" && v != "(devel)" {
			attrs["application.version"] = v
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				attrs["vcs.revision"] = s.Value
			case "vcs.time":
				attrs["vcs.time"] = s.Value
			case "vcs.modified":
				attrs["vcs.modified"] = s.Value
			}
		}
		modules := make([]string, 0, len(info.Deps))
		for _, dep := range info.Deps {
			m := dep
			if m.Replace != nil {
				m = m.Replace
			}
			modules = append(modules, m.Path+"@"+m.Version)
		}
		buildInfoAttrs = attrs
		buildInfoModules = modules
	})
	return buildInfoAttrs, buildInfoModules
}

// defaultEnvScrubPatterns match environment variable names whose values are
// redacted before submission. Case-insensitive substring match.
var defaultEnvScrubPatterns = []string{
	"TOKEN", "SECRET", "PASSWORD", "PASSWD", "APIKEY", "API_KEY",
	"ACCESS_KEY", "SECRET_KEY", "PRIVATE_KEY", "CREDENTIAL", "AUTH",
}

const redactedValue = "[REDACTED]"

// getEnvVars returns the process environment with secret-looking values
// redacted. extraPatterns extends the built-in pattern list.
func getEnvVars(extraPatterns []string) map[string]string {
	patterns := make([]string, 0, len(defaultEnvScrubPatterns)+len(extraPatterns))
	patterns = append(patterns, defaultEnvScrubPatterns...)
	patterns = append(patterns, extraPatterns...)

	result := map[string]string{}
	for _, line := range os.Environ() {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(key)
		for _, p := range patterns {
			if p != "" && strings.Contains(upper, strings.ToUpper(p)) {
				value = redactedValue
				break
			}
		}
		result[key] = value
	}
	return result
}
