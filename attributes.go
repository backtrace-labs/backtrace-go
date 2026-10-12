package bt

import (
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
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

	machineGUIDOnce sync.Once
	machineGUIDVal  string
)

// machineAttributes gathers machine metadata (CPU model, OS version) using
// native file and syscall reads — never shell pipelines. It runs at most
// once per process, on first use (never at import time). Failures degrade
// to missing attributes.
func machineAttributes(d diag) map[string]interface{} {
	machineOnce.Do(func() {
		attrs := map[string]interface{}{}
		collectMachineInfo(attrs, d)
		machineAttrs = attrs
	})
	return machineAttrs
}

// machineGUID resolves the stable machine identifier lazily and only when a
// client actually opted in via SendMachineID — on macOS this is the one
// probe that spawns a (bounded, non-shell) subprocess.
func machineGUID(d diag) string {
	machineGUIDOnce.Do(func() {
		machineGUIDVal = collectMachineGUID(d)
	})
	return machineGUIDVal
}

var (
	buildInfoOnce    sync.Once
	buildInfoAttrs   map[string]interface{}
	buildInfoModules []string
)

// maxDependencyModules caps the dependency annotation size.
const maxDependencyModules = 512

// buildInfoAttributes extracts release metadata embedded by the Go toolchain:
// main module version, VCS revision/time/dirty flag, and the dependency list
// (attached to reports as the "Dependencies" annotation, capped).
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
		deps := info.Deps
		if len(deps) > maxDependencyModules {
			deps = deps[:maxDependencyModules]
		}
		modules := make([]string, 0, len(deps))
		for _, dep := range deps {
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
// redacted before submission. Case-insensitive substring match. For a crash
// reporter over-redaction beats leaking, so the patterns are deliberately
// broad: PASS covers PASSWORD/PASSWD/PASSPHRASE/DB_PASS, KEY covers
// APIKEY/API_KEY/*_KEY, CONN covers CONNECTION_STRING/CONN_STR, DSN covers
// database and telemetry DSNs.
var defaultEnvScrubPatterns = []string{
	"TOKEN", "SECRET", "PASS", "KEY", "CREDENTIAL", "AUTH",
	"DSN", "COOKIE", "SESSION", "SIGNATURE", "BEARER", "CONN",
}

// urlUserinfoPattern matches connection-string values with embedded
// credentials (scheme://user:password@host), regardless of the variable
// name (DATABASE_URL, REDIS_URL, MONGODB_URI, ...).
var urlUserinfoPattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^/@\s]+:[^/@\s]+@`)

const redactedValue = "[REDACTED]"

// getEnvVars returns the process environment with secret-looking values
// redacted: variable names matching the scrub patterns, plus any value that
// embeds URL credentials. extraPatterns extends the built-in pattern list.
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
		if value != redactedValue && urlUserinfoPattern.MatchString(value) {
			value = redactedValue
		}
		// Submission-URL shapes (the SDK's own BACKTRACE_ENDPOINT, or any
		// variable holding a tokenized submit URL) get their token
		// redacted while keeping the rest of the URL readable.
		if value != redactedValue {
			value = redactSubmissionValue(value)
		}
		result[key] = value
	}
	return result
}

// redactSubmissionValue redacts embedded Backtrace submission tokens
// (?token= query or submit-style path) in URL-shaped env values.
func redactSubmissionValue(value string) string {
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return value
	}
	u, err := neturl.Parse(value)
	if err != nil {
		return value
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Query().Get("token") != "" || pathEmbedsToken(u.Hostname(), segments) {
		return redactURL(value)
	}
	return value
}
