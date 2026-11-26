package bt

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGetEnvVarsSplitAndScrub(t *testing.T) {
	t.Setenv("BT_ATTR_TEST_PLAIN", "a=b=c")
	t.Setenv("BT_ATTR_TEST_MY_SECRET", "sensitive")
	t.Setenv("BT_ATTR_TEST_API_KEY", "sensitive")
	t.Setenv("BT_ATTR_TEST_CUSTOM", "sensitive")

	env := getEnvVars(envCaptureOptions{extraPatterns: []string{"BT_ATTR_TEST_CUSTOM"}})

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

func TestEnvEntrySkipsHiddenWindowsEntries(t *testing.T) {
	type entry struct{ key, value string }
	var got []entry
	for _, line := range []string{
		"=C:=C:\\work", "=ExitCode=00000001", "=::=::\\", "=", "=NOVALUE",
		"PATH=C:\\Windows", "PLAIN=a=b=c", "EMPTY=", "NOEQUALS",
	} {
		if key, value, ok := envEntry(line); ok {
			got = append(got, entry{key, value})
		}
	}
	want := []entry{{"PATH", "C:\\Windows"}, {"PLAIN", "a=b=c"}, {"EMPTY", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %q, want %q", got, want)
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

	env := getEnvVars(envCaptureOptions{})
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

	env := getEnvVars(envCaptureOptions{})
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

func TestGetEnvVarsHidesClientToken(t *testing.T) {
	const token = "prefixed-submission-token"
	t.Setenv("BACKTRACE_ENDPOINT", "https://proxy.corp/backtrace/universe/"+token+"/json")
	t.Setenv("BT_SHAPE_QUOTED", `"https://proxy.corp/backtrace/universe/`+token+`/json"`)
	t.Setenv("BT_SHAPE_SHORT", "ab-cd-ef")

	env := getEnvVars(envCaptureOptions{secrets: submissionSecrets("https://proxy.corp/backtrace/universe/"+token+"/json", "ab-cd")})
	for _, name := range []string{"BACKTRACE_ENDPOINT", "BT_SHAPE_QUOTED"} {
		if strings.Contains(env[name], token) || !strings.Contains(env[name], "proxy.corp/backtrace/universe/REDACTED/json") {
			t.Errorf("%s = %q, want the token hidden and the rest kept", name, env[name])
		}
	}
	if env["BT_SHAPE_SHORT"] != "ab-cd-ef" {
		t.Errorf("a secret shorter than %d bytes rewrote a value: %q", minEnvSecretLength, env["BT_SHAPE_SHORT"])
	}
}

func TestReportEnvHidesClientEndpointToken(t *testing.T) {
	const token = "prefixed-submission-token"
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.Endpoint += "/backtrace/universe/" + token + "/json"
		cfg.Token = ""
		cfg.EnvVars = []string{"BT_SHAPE_RELAY"}
	})
	t.Setenv("BT_SHAPE_RELAY", "relay at /backtrace/universe/"+token+"/json")

	c.ReportMessage("env", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if got := envAnnotation(t, rs)["BT_SHAPE_RELAY"]; got != "relay at /backtrace/universe/REDACTED/json" {
		t.Errorf("BT_SHAPE_RELAY = %q, want the endpoint token hidden", got)
	}
}

// TestGetEnvVarsAllowlist pins exact-name capture: only listed variables are
// read, everything else is omitted.
func TestGetEnvVarsAllowlist(t *testing.T) {
	t.Setenv("BT_ALLOW_WANTED", "keep")
	t.Setenv("BT_ALLOW_UNWANTED", "drop")

	env := getEnvVars(envCaptureOptions{allowlist: []string{"BT_ALLOW_WANTED", "BT_ALLOW_MISSING"}})
	if env["BT_ALLOW_WANTED"] != "keep" {
		t.Errorf("allowlisted var missing: %q", env["BT_ALLOW_WANTED"])
	}
	if _, present := env["BT_ALLOW_UNWANTED"]; present {
		t.Error("non-allowlisted var captured")
	}
	if _, present := env["BT_ALLOW_MISSING"]; present {
		t.Error("unset allowlisted var should be absent, not empty")
	}
}

// TestGetEnvVarsBudgets pins the two limits of whole-environment capture
// and that the exact allow-list is exempt from the count cap, as documented.
func TestGetEnvVarsBudgets(t *testing.T) {
	t.Setenv("BT_BUDGET_SMALL_A", "a")
	t.Setenv("BT_BUDGET_HUGE", strings.Repeat("x", 4096))
	t.Setenv("BT_BUDGET_SMALL_B", "b")

	if env := getEnvVars(envCaptureOptions{maxVars: 3}); len(env) != 3 {
		t.Errorf("maxVars=3 captured %d variables", len(env))
	}

	// The count cap applies to whole-environment capture only.
	allow := []string{"BT_BUDGET_SMALL_A", "BT_BUDGET_HUGE", "BT_BUDGET_SMALL_B"}
	if env := getEnvVars(envCaptureOptions{allowlist: allow, maxVars: 1}); len(env) != 3 {
		t.Errorf("allow-list was cut by maxVars: %d of 3 captured", len(env))
	}

	// One oversized variable is skipped; the ones after it still arrive.
	env := getEnvVars(envCaptureOptions{allowlist: allow, maxBytes: 256})
	if _, present := env["BT_BUDGET_HUGE"]; present {
		t.Error("variable larger than the byte budget was captured")
	}
	if env["BT_BUDGET_SMALL_A"] != "a" || env["BT_BUDGET_SMALL_B"] != "b" {
		t.Errorf("an oversized variable ended capture of the others: %v", env)
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
		if m.path == "golang.org/x/sys" && m.installed != "" {
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

func TestGetEnvVarsRedactsURLSecrets(t *testing.T) {
	cases := []struct{ in, want string }{
		{"redis://:s3cretpw@redis:6379/0", redactedValue},
		{"https://ghp_TOKEN:x-oauth-basic@github.com/org/repo.git", redactedValue},
		{"https://u:pw@goproxy.corp,https://ghp_SECOND@goproxy2.corp,direct", redactedValue},
		{"amqp://user:p@ss@broker/vhost", redactedValue},
		{"jdbc:mysql://root:hunter2@db/app", redactedValue},
		{"postgres://user:pw@bad host/db", redactedValue},
		{"-Ddb.url=postgres://u:p@h/db -Xmx1g", redactedValue},
		{"https://proxy/?target=postgres://u:p@h/db", redactedValue},
		{"smtp://alerts@example.com:hunter2@smtp.example.com:587", redactedValue},
		{"https://proxy.golang.org,https://me@corp.com:TOK@goproxy.corp,direct", redactedValue},
		{"postgres://user:pa?ss@db/app", redactedValue},
		{"redis://:s3c#ret@redis:6379/0", redactedValue},
		{"https://user:@example.com/path", "https://user:@example.com/path"},
		{"smtps://smtp.example.com:465?to=oncall@example.com", "smtps://smtp.example.com:465?to=oncall@example.com"},
		{"http://[::1]:8080?email=a@b.example.com", "http://[::1]:8080?email=a@b.example.com"},
		{"ssh://git@github.com:22/org/repo.git", "ssh://git@github.com:22/org/repo.git"},
		{"redis://:@redis:6379/0", "redis://:@redis:6379/0"},
		{"smtps://alerts@smtp.example.com:465?to=oncall@example.com", "smtps://alerts@smtp.example.com:465?to=oncall@example.com"},
		{"postgresql://db/app?user=app&password=s3cretpw", "postgresql://db/app?user=app&password=REDACTED"},
		{"jdbc:mysql://db:3306/app?user=root&password=hunter2", "jdbc:mysql://db:3306/app?user=root&password=REDACTED"},
		{"jdbc:postgresql://db/app?password=hunter2#frag", "jdbc:postgresql://db/app?password=REDACTED#frag"},
		{"https://h/p?b=1&access_token=x&X-Amz-Signature=y&sig=z&api_key=k&pwd=p&Auth=a&a=2", "https://h/p?b=1&access_token=REDACTED&X-Amz-Signature=REDACTED&sig=REDACTED&api_key=REDACTED&pwd=REDACTED&Auth=REDACTED&a=2"},
		{"mongodb://db1:27017/app?replicaSet=rs0&authSource=admin&authMechanism=SCRAM-SHA-256", "mongodb://db1:27017/app?replicaSet=rs0&authSource=admin&authMechanism=SCRAM-SHA-256"},
		{"https://jira.example.com/issues/?assignee=bob&project=OPS", "https://jira.example.com/issues/?assignee=bob&project=OPS"},
		{"https://h/p?token=", "https://h/p?token="},
		{"https://submit.backtrace.io/universe/SECRETSUBMITTOKEN/json", "https://submit.backtrace.io/universe/REDACTED/json"},
		{"https://user@example.com/path", "https://user@example.com/path"},
		{"https://example.com:8443/path", "https://example.com:8443/path"},
		{"plain value", "plain value"},
	}
	for _, c := range cases {
		t.Setenv("BT_SHAPE_VALUE", c.in)
		if got := getEnvVars(envCaptureOptions{allowlist: []string{"BT_SHAPE_VALUE"}})["BT_SHAPE_VALUE"]; got != c.want {
			t.Errorf("%q captured as %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSecretQueryKey(t *testing.T) {
	secret := []string{
		"password", "sslpassword", "PassWord", "pass", "dbPass", "pwd", "passphrase", "myPassPhrase",
		"passcode", "secret", "client_secret", "secretkey", "privatekey", "privateKey",
		"token", "access_token", "Token", "_authToken", "X-Amz-Security-Token",
		"auth", "basicAuth", "authorization", "Authorization",
		"key", "api_key", "apiKey", "APIKey", "HMACKey", "accessKey", "apikey", "api key", "sentry_key",
		"sig", "xSig", "signature", "X-Amz-Signature", "X-Goog-Signature",
		"token2", "password1", "token_v2", "api_key_2", "apiKeyV2", "client_secret_2", "token[0]",
	}
	kept := []string{
		"authSource", "authMechanism", "allowPublicKeyRetrieval", "allowNativePasswords",
		"tlsCertificateKeyFile", "sslkey", "passfile", "X-Amz-SignedHeaders", "X-Amz-Credential",
		"assignee", "design", "signal", "signup", "insights", "keyword", "keyspace", "monkey",
		"sortkey", "key_id", "AWSAccessKeyId", "passive", "compass", "bypass_cache", "author",
		"oauth_callback", "tokenizer", "max_tokens", "token_type", "secretary", "secret_name",
		"v2", "2", "",
	}
	for _, name := range secret {
		if !secretQueryKey(name) {
			t.Errorf("secretQueryKey(%q) = false, want true", name)
		}
	}
	for _, name := range kept {
		if secretQueryKey(name) {
			t.Errorf("secretQueryKey(%q) = true, want false", name)
		}
	}
}
