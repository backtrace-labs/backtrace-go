package bt

import (
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
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
// GC, no stop-the-world.
func runtimeAttributes(attrs map[string]interface{}) {
	attrs["runtime.goroutines"] = runtime.NumGoroutine()
	attrs["runtime.gomaxprocs"] = runtime.GOMAXPROCS(0)
	attrs["process.age"] = int64(time.Since(processStart).Seconds())

	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/heap/unused:bytes"},
		{Name: "/memory/classes/heap/free:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
		{Name: "/gc/heap/objects:objects"},
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/gc/gogc:percent"},
		{Name: "/gc/gomemlimit:bytes"},
	}
	metrics.Read(samples)
	value := make([]uint64, len(samples))
	for i, sample := range samples {
		if sample.Value.Kind() != metrics.KindUint64 {
			return
		}
		value[i] = sample.Value.Uint64()
	}
	attrs["memory.heap.alloc"] = value[0]
	attrs["memory.heap.sys"] = value[0] + value[1] + value[2] + value[3]
	attrs["memory.heap.objects"] = value[4]
	attrs["gc.count"] = value[5]
	// GOGC=off reads as the sign-extended -1.
	if gogc := int64(value[6]); gogc >= 0 {
		attrs["runtime.gogc"] = gogc
	} else {
		attrs["runtime.gogc"] = "off"
	}
	attrs["runtime.gomemlimit"] = value[7]
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
	buildInfoModules []moduleDependency
)

// maxDependencyModules caps the dependency annotation size.
const maxDependencyModules = 512

type moduleDependency struct {
	path, installed, requested string
}

// buildInfoAttributes extracts release metadata embedded by the Go toolchain:
// main module version, VCS revision/time/dirty flag, and the dependency list
// (attached to reports as the "Dependencies" annotation, capped).
func buildInfoAttributes() (map[string]interface{}, []moduleDependency) {
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
		buildInfoAttrs = attrs
		buildInfoModules = moduleDependencies(info.Deps)
	})
	return buildInfoAttrs, buildInfoModules
}

func moduleDependencies(deps []*debug.Module) []moduleDependency {
	if len(deps) > maxDependencyModules {
		deps = deps[:maxDependencyModules]
	}
	modules := make([]moduleDependency, 0, len(deps))
	for _, dep := range deps {
		m := moduleDependency{path: dep.Path, installed: dep.Version}
		if r := dep.Replace; r != nil {
			m.requested = dep.Version
			m.installed = r.Version
			if r.Path != dep.Path {
				m.installed = r.Path
				if r.Version != "" {
					m.installed += "@" + r.Version
				}
			}
		}
		modules = append(modules, m)
	}
	return modules
}

func dependencyAnnotation(modules []moduleDependency) map[string]map[string]string {
	out := make(map[string]map[string]string, len(modules))
	for _, m := range modules {
		entry := map[string]string{"installedVersion": m.installed}
		if m.requested != "" {
			entry["requestedVersion"] = m.requested
		}
		out[m.path] = entry
	}
	return out
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

var urlSchemePattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://`)

var (
	secretQueryWords    = []string{"key", "sig", "pass", "pwd", "auth", "passcode", "authorization"}
	secretQuerySuffixes = []string{"password", "passwd", "passphrase", "secret", "token", "signature", "apikey", "secretkey", "privatekey"}
)

const redactedValue = "[REDACTED]"

// envCaptureOptions controls environment capture.
type envCaptureOptions struct {
	// allowlist, when non-empty, restricts capture to these exact names.
	allowlist []string
	// extraPatterns extends the built-in redaction patterns.
	extraPatterns []string
	maxVars       int
	maxBytes      int
}

// getEnvVars returns the process environment (or an allowlisted subset) with
// secret-looking values redacted: variable names matching the scrub
// patterns, plus any value that embeds URL credentials. Capture is bounded
// by maxVars and maxBytes.
func getEnvVars(opts envCaptureOptions) map[string]string {
	patterns := make([]string, 0, len(defaultEnvScrubPatterns)+len(opts.extraPatterns))
	patterns = append(patterns, defaultEnvScrubPatterns...)
	patterns = append(patterns, opts.extraPatterns...)

	redact := func(key, value string) string {
		upper := strings.ToUpper(key)
		for _, p := range patterns {
			if p != "" && strings.Contains(upper, strings.ToUpper(p)) {
				return redactedValue
			}
		}
		if embedsURLPassword(value) {
			return redactedValue
		}
		// Submission-URL shapes (the SDK's own BACKTRACE_ENDPOINT, or any
		// variable holding a tokenized submit URL) get their token
		// redacted while keeping the rest of the URL readable.
		return redactSubmissionValue(value)
	}

	result := map[string]string{}
	total := 0
	// add stores one variable unless it would exceed the byte budget. An
	// oversized variable is skipped on its own: it must not end the capture
	// of the variables after it.
	add := func(key, value string) {
		value = redact(key, value)
		size := len(key) + len(value)
		if opts.maxBytes > 0 && total+size > opts.maxBytes {
			return
		}
		total += size
		result[key] = value
	}

	if len(opts.allowlist) > 0 {
		// Exact-name capture: read only the listed variables. The count cap
		// does not apply, because the caller chose every name.
		for _, name := range opts.allowlist {
			if v, ok := os.LookupEnv(name); ok {
				add(name, v)
			}
		}
		return result
	}

	for _, line := range os.Environ() {
		if opts.maxVars > 0 && len(result) >= opts.maxVars {
			break
		}
		key, value, ok := envEntry(line)
		if !ok {
			continue
		}
		add(key, value)
	}
	return result
}

func envEntry(line string) (key, value string, ok bool) {
	key, value, ok = strings.Cut(line, "=")
	// Windows keeps hidden entries such as "=C:=C:\dir" in the environment.
	return key, value, ok && key != ""
}

// redactSubmissionValue redacts embedded Backtrace submission tokens
// (?token= query or submit-style path) in URL-shaped env values.
// Other secret-looking query values are redacted too.
func redactSubmissionValue(value string) string {
	if !strings.Contains(value, "://") {
		return value
	}
	u, err := neturl.Parse(value)
	if err != nil || u.Scheme == "" {
		return value
	}
	changed := false
	if query, ok := redactSecretQuery(u.RawQuery); ok {
		u.RawQuery = query
		changed = true
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		segments := strings.Split(strings.Trim(u.Path, "/"), "/")
		if pathEmbedsToken(u.Hostname(), segments) {
			segments[1] = "REDACTED"
			u.Path = "/" + strings.Join(segments, "/")
			changed = true
		}
	}
	if !changed {
		return value
	}
	return u.String()
}

func redactSecretQuery(raw string) (string, bool) {
	pairs := strings.Split(raw, "&")
	changed := false
	for i, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || value == "" {
			continue
		}
		name, err := neturl.QueryUnescape(key)
		if err != nil {
			name = key
		}
		if secretQueryKey(name) {
			pairs[i] = key + "=REDACTED"
			changed = true
		}
	}
	return strings.Join(pairs, "&"), changed
}

// A '?' or '#' before the '@' belongs to the password unless the text before
// it is a host:port.
func embedsURLPassword(value string) bool {
	for _, loc := range urlSchemePattern.FindAllStringIndex(value, -1) {
		rest := value[loc[1]:]
		if end := strings.IndexFunc(rest, func(r rune) bool { return r == '/' || unicode.IsSpace(r) }); end >= 0 {
			rest = rest[:end]
		}
		authority := rest
		if end := strings.IndexAny(rest, "?#"); end >= 0 && (strings.Contains(rest[:end], "@") || isHostPort(rest[:end])) {
			authority = rest[:end]
		}
		at := strings.LastIndex(authority, "@")
		if at < 0 {
			continue
		}
		if _, password, ok := strings.Cut(authority[:at], ":"); ok && password != "" {
			return true
		}
	}
	return false
}

func isHostPort(s string) bool {
	i := strings.LastIndex(s, ":")
	return i < 0 || strings.HasSuffix(s, "]") || strings.Trim(s[i+1:], "0123456789") == ""
}

func secretQueryKey(name string) bool {
	words := keyWords(name)
	for len(words) > 0 && isVersionWord(words[len(words)-1]) {
		words = words[:len(words)-1]
	}
	if len(words) == 0 {
		return false
	}
	last := len(words) - 1
	words[last] = strings.TrimRight(words[last], "0123456789")
	if slices.Contains(secretQueryWords, words[last]) {
		return true
	}
	joined := strings.Join(words, "")
	for _, suffix := range secretQuerySuffixes {
		if strings.HasSuffix(joined, suffix) {
			return true
		}
	}
	return false
}

// keyWords splits at punctuation and camelCase: "X-Amz-Signature" gives x,
// amz, signature and "APIKeyV2" gives api, key, v2.
func keyWords(name string) []string {
	var words []string
	fields := strings.FieldsFunc(name, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, f := range fields {
		start := 0
		for i := 1; i < len(f); i++ {
			if isUpperASCII(f[i:]) && (!isUpperASCII(f[i-1:]) || i+1 < len(f) && !isUpperASCII(f[i+1:])) {
				words = append(words, strings.ToLower(f[start:i]))
				start = i
			}
		}
		words = append(words, strings.ToLower(f[start:]))
	}
	return words
}

func isVersionWord(word string) bool {
	digits := strings.TrimPrefix(word, "v")
	return digits != "" && strings.Trim(digits, "0123456789") == ""
}
