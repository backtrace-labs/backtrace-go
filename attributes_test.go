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
	chain := unwrapErrorChain(err, DefaultMaxErrorDepth)
	if len(chain) != 2 {
		t.Fatalf("chain length = %d", len(chain))
	}
	if chain[0].Type != "*bt.testWrapErr" || chain[0].Message != "outer" {
		t.Errorf("chain head = %+v", chain[0])
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
