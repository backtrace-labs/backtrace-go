package bt

import (
	"reflect"
	"strings"
	"testing"
)

func TestGetEnvVarsSplitAndScrub(t *testing.T) {
	t.Setenv("BT_ATTR_TEST_PLAIN", "a=b=c")
	t.Setenv("BT_ATTR_TEST_MY_SECRET", "sensitive")
	t.Setenv("BT_ATTR_TEST_API_KEY", "sensitive")
	t.Setenv("BT_ATTR_TEST_CUSTOM", "sensitive")

	env := getEnvVars([]string{"BT_ATTR_TEST_CUSTOM"})

	if env["BT_ATTR_TEST_PLAIN"] != "a=b=c" {
		t.Errorf("value truncated at '=': %q", env["BT_ATTR_TEST_PLAIN"])
	}
	if env["BT_ATTR_TEST_MY_SECRET"] != redactedValue {
		t.Errorf("SECRET not redacted: %q", env["BT_ATTR_TEST_MY_SECRET"])
	}
	if env["BT_ATTR_TEST_API_KEY"] != redactedValue {
		t.Errorf("API_KEY not redacted: %q", env["BT_ATTR_TEST_API_KEY"])
	}
	if env["BT_ATTR_TEST_CUSTOM"] != redactedValue {
		t.Errorf("extra pattern not honored: %q", env["BT_ATTR_TEST_CUSTOM"])
	}
}

// TestGetEnvVarsScrubsCommonSecretShapes covers the broadened name patterns
// and the value-shape detection of URL-embedded credentials.
func TestGetEnvVarsScrubsCommonSecretShapes(t *testing.T) {
	redactedNames := []string{
		"BT_SHAPE_ENCRYPTION_KEY", "BT_SHAPE_SIGNING_KEY", "BT_SHAPE_DB_PASS",
		"BT_SHAPE_PASSPHRASE", "BT_SHAPE_SENTRY_DSN", "BT_SHAPE_SESSION_COOKIE",
		"BT_SHAPE_CONNECTION_STRING", "BT_SHAPE_BEARER_HEADER",
	}
	for _, name := range redactedNames {
		t.Setenv(name, "sensitive")
	}
	// Connection strings with embedded credentials are caught by value
	// shape, regardless of the variable name.
	t.Setenv("BT_SHAPE_DATABASE_URL", "postgres://user:hunter2@db/prod?sslmode=require")
	// Plain URLs without credentials survive.
	t.Setenv("BT_SHAPE_HOMEPAGE", "https://example.com/path")

	env := getEnvVars(nil)
	for _, name := range redactedNames {
		if env[name] != redactedValue {
			t.Errorf("%s not redacted: %q", name, env[name])
		}
	}
	if env["BT_SHAPE_DATABASE_URL"] != redactedValue {
		t.Errorf("URL-embedded credentials not redacted: %q", env["BT_SHAPE_DATABASE_URL"])
	}
	if env["BT_SHAPE_HOMEPAGE"] != "https://example.com/path" {
		t.Errorf("credential-free URL over-redacted: %q", env["BT_SHAPE_HOMEPAGE"])
	}
}

// TestGetEnvVarsRedactsSubmissionURLs pins the fix for the SDK's own
// credential: BACKTRACE_ENDPOINT (or any variable holding a tokenized
// submission URL) must not ship its token in the env annotation.
func TestGetEnvVarsRedactsSubmissionURLs(t *testing.T) {
	t.Setenv("BACKTRACE_ENDPOINT", "https://submit.backtrace.io/universe/SECRETSUBMITTOKEN/json")
	t.Setenv("BT_SHAPE_LEGACY_ENDPOINT_URL", "https://uni.sp.backtrace.io/post?format=json&token=SECRETQUERYTOKEN")

	env := getEnvVars(nil)
	if strings.Contains(env["BACKTRACE_ENDPOINT"], "SECRETSUBMITTOKEN") {
		t.Errorf("submit-path token leaked: %q", env["BACKTRACE_ENDPOINT"])
	}
	if !strings.Contains(env["BACKTRACE_ENDPOINT"], "submit.backtrace.io") {
		t.Errorf("redaction should keep the URL readable: %q", env["BACKTRACE_ENDPOINT"])
	}
	if strings.Contains(env["BT_SHAPE_LEGACY_ENDPOINT_URL"], "SECRETQUERYTOKEN") {
		t.Errorf("query token leaked: %q", env["BT_SHAPE_LEGACY_ENDPOINT_URL"])
	}
}

func TestStaticAttributes(t *testing.T) {
	attrs := staticAttributes()
	for _, key := range []string{
		"backtrace.version", "backtrace.agent", "hostname", "uname.sysname",
		"cpu.arch", "cpu.count", "process.id", "application",
		"application.session", "go.version",
	} {
		if _, ok := attrs[key]; !ok {
			t.Errorf("static attribute %q missing", key)
		}
	}
	if attrs["backtrace.version"] != Version {
		t.Errorf("backtrace.version = %v", attrs["backtrace.version"])
	}
	if !strings.HasPrefix(attrs["go.version"].(string), "go") {
		t.Errorf("go.version = %v", attrs["go.version"])
	}
}

func TestRuntimeAttributes(t *testing.T) {
	attrs := map[string]interface{}{}
	runtimeAttributes(attrs)
	for _, key := range []string{
		"runtime.goroutines", "runtime.gomaxprocs", "process.age",
		"memory.heap.alloc", "memory.heap.sys", "gc.count",
	} {
		if _, ok := attrs[key]; !ok {
			t.Errorf("runtime attribute %q missing", key)
		}
	}
	if n := attrs["runtime.goroutines"].(int); n < 1 {
		t.Errorf("runtime.goroutines = %d", n)
	}
}

func TestMachineAttributesCachedOnce(t *testing.T) {
	first := machineAttributes(diag{})
	second := machineAttributes(diag{})
	// The maps must be the same instance (sync.Once semantics).
	if reflect.ValueOf(first).Pointer() != reflect.ValueOf(second).Pointer() {
		t.Error("machineAttributes returned different map instances; caching broken")
	}
}

func TestBuildInfoAttributesDoesNotPanic(t *testing.T) {
	attrs, modules := buildInfoAttributes()
	if attrs == nil {
		t.Error("buildInfoAttrs is nil")
	}
	// In `go test` binaries the x/sys dependency must appear.
	found := false
	for _, m := range modules {
		if strings.HasPrefix(m, "golang.org/x/sys@") {
			found = true
		}
	}
	if !found && len(modules) > 0 {
		t.Errorf("x/sys missing from module list: %v", modules)
	}
}

func TestUnwrapErrorChainTypes(t *testing.T) {
	err := &testWrapErr{msg: "outer", inner: &testWrapErr{msg: "inner"}}
	chain := unwrapErrorChain(err, DefaultMaxErrorDepth, DefaultMaxErrorNodes)
	if len(chain) != 2 {
		t.Fatalf("chain length = %d", len(chain))
	}
	if chain[0].Type != "*bt.testWrapErr" || chain[0].Message != "outer" {
		t.Errorf("chain head = %+v", chain[0])
	}
	if chain[1].ParentID == nil || *chain[1].ParentID != 0 || chain[1].Source != "unwrap" {
		t.Errorf("chain link parent metadata = %+v", chain[1])
	}
}

func TestBreadcrumbRingDisabled(t *testing.T) {
	var r *breadcrumbRing // negative MaxBreadcrumbs => nil ring
	r.add(Breadcrumb{Message: "ignored"})
	if got := r.snapshot(); got != nil {
		t.Errorf("nil ring returned crumbs: %v", got)
	}
	if newBreadcrumbRing(-1) != nil {
		t.Error("negative capacity should produce nil ring")
	}
}
