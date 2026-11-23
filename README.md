# backtrace-go

[Backtrace](https://backtrace.io/) error reporting SDK for Go.

Reports errors, messages, and panics — with goroutine stacks, source context, rich attributes, breadcrumbs, and file attachments to the Backtrace (Sauce Labs) platform.
The package also ships an integration with out-of-process tracers ([bcd](#bcd-out-of-process-tracing)).

## Installation

```
go get github.com/backtrace-labs/backtrace-go
```

Requires Go 1.25+. The only dependency is `golang.org/x/sys`.

## Quick start

```go
package main

import (
	"errors"

	bt "github.com/backtrace-labs/backtrace-go"
)

func main() {
	client, err := bt.NewClient(bt.Config{
		Endpoint: "https://submit.backtrace.io/{universe}/{token}/json",
	})
	if err != nil {
		// Endpoint missing or malformed.
		panic(err)
	}
	defer client.Close()

	client.Report(errors.New("something went wrong"), map[string]interface{}{
		"request.id": "abc-123",
	})
}
```

Two endpoint forms are supported:

- `https://submit.backtrace.io/{universe}/{token}/json` — set `Endpoint` only
- `https://{universe}.sp.backtrace.io` — set `Endpoint` + `Token`

`BACKTRACE_ENDPOINT` and `BACKTRACE_TOKEN` environment variables are used as fallbacks when the corresponding fields are empty.

## Reporting

```go
client.Report(err, nil)                          // error (type + unwrap chain captured)
client.ReportMessage("cache warmup skipped", nil) // plain message
client.ReportPanicValue(recovered, nil)           // recovered panic value

// Panic capture with defer:
defer bt.ReportPanic(nil)           // reports, waits (one 5s deadline), re-panics
defer bt.ReportAndRecoverPanic(nil) // reports without waiting; swallows the panic
```

Regular `Report*` calls never block on SDK-controlled network I/O or queue admission: reports are queued to a background worker, and a full queue drops the newest report and counts it (`client.Stats()`, `client.DroppedReports()`) instead of stalling the application. The SDK-controlled waits are explicitly bounded: `Flush`, `Close`, `ReportPanicValueAndFlush`, and `bt.ReportPanic` (one 5s deadline); `ReportAndRecoverPanic` reports without waiting. `FlushContext` and `CloseContext` follow the caller's context. With `context.Background()` they have no deadline. `ReportPanicValueAndFlush` waits until the deadline of its context, or 5s when it has none.

See [Extension points and guarantees](#extension-points-and-guarantees) for the one caveat: user-supplied callbacks are trusted to return promptly.

Delivery lifecycle:

```go
client.Flush(5 * time.Second)  // wait for reports queued before the call; client stays usable
client.FlushContext(ctx)       // context-aware variant
client.Close()                 // drain and stop, bounded by ShutdownTimeout (default 5s)
client.CloseContext(ctx)       // explicit deadline; cancels in-flight sends when it expires
```

Flushing proves local processing and send completion, not backend acceptance. While the SDK is unconfigured, `bt.ReportPanic`/`bt.ReportAndRecoverPanic` do nothing — in particular they do not recover, so the application's panic proceeds unchanged.

### Report contents

- The classifier is the error's Go type: the innermost type on its unwrap path, skipping the plain error and wrapper types of `errors`, `fmt` and `github.com/pkg/errors`. `fmt.Errorf("decode: %w", err)` around a `*json.SyntaxError` classifies as `*json.SyntaxError`. When the path holds only those types, the innermost one is used: `errors.New("x")` classifies as `*errors.errorString`. Message reports use `message`, and panics with a non-error value `panic`.
- `error.message` and each `Error Chain` message are capped at 1024 bytes. A longer `error.message` is sent in full in the `Message` annotation.
- A report over `MaxReportBytes` (default 8 MiB) is trimmed, source text and other goroutines first. `backtrace.truncated` lists what was cut. A report that still does not fit is dropped.

Backtrace groups reports by the frames of the main thread. An error that recorded its own stack (a `StackTrace()` method as in `github.com/pkg/errors`, or `Callers() []uintptr`) is grouped by where it was created. The innermost such stack in the unwrap graph becomes the main thread, named `error origin`, and the reporting goroutine is kept next to it.

### Delivery

- Request bodies are gzip-compressed. Set `DisableCompression` for a proxy that cannot take `Content-Encoding: gzip`.
- A network error before the request body was sent, and a 408, 500, 502, 503 or 504 response, are retried up to `MaxRetries` times (default 2, negative disables).
- `ReportsPerMinute` caps the reports a client accepts in any 60-second window. 0, the default, means no limit. Reports over the cap are dropped and counted in `Stats()`.

### Fatal crashes

`bt.MonitorCrashes` reports what `recover` cannot catch: an unrecovered panic in any goroutine, a runtime fatal error (`concurrent map writes`, out of memory, stack overflow) or a fatal signal.

```go
func main() {
	cfg := bt.Config{Endpoint: "https://submit.backtrace.io/{universe}/{token}/json"}
	if err := bt.MonitorCrashes(cfg); err != nil {
		log.Printf("crash monitor: %v", err) // the application runs on without it
	}
	client, err := bt.NewClient(cfg)
	// ...
}
```

Call it first thing in `main`. It re-runs the executable, with the same arguments, as a monitor process. The [`MonitorCrashes` godoc](https://pkg.go.dev/github.com/backtrace-labs/backtrace-go#MonitorCrashes) lists what crash reports carry and when the call returns an error.

- A `SIGKILL`, including one from the out-of-memory killer, is not reported. `os.Exit` and `log.Fatal` are not crashes.
- On Linux, `MonitorCrashes` returns an error when the application is PID 1, as in a container without an init. When PID 1 is an init such as `tini` (`docker --init`) that exits with the application, crashes are not reported.
- Core dumps need the Backtrace Coresnap workflow.

## Configuration

```go
client, err := bt.NewClient(bt.Config{
	Endpoint:             "https://submit.backtrace.io/{universe}/{token}/json",
	CaptureAllGoroutines: true,                 // include every goroutine's stack
	SourceCode:           bt.SourceCodeContext, // default: Metadata (no source text); Context/File are opt-in
	SourceRoots:          []string{"/srv/app"}, // restrict which files may be read for snippets
	ContextLineCount:     8,                    // lines above/below each frame
	Attributes: map[string]interface{}{         // stamped on every report
		"application.environment": "production",
	},
	AttachmentPaths: []string{"/var/log/app.log"}, // uploaded with every report (10 MiB/file cap)
	EnvVars:         []string{"APP_ENV", "REGION"}, // exact allowlist (preferred)
	SendEnvVars:     false, // or capture ALL env vars, best-effort redaction
	SampleRate:      1.0,   // fraction of reports sent (0 == 1.0)
	BeforeSend: func(r *bt.ReportData) *bt.ReportData {
		delete(r.Attributes, "secret") // scrub, enrich, or return nil to drop
		return r
	},
	Debug: false, // diagnostic logging; the SDK never panics either way
})
```

All zero values are sensible defaults: 30s HTTP timeout, queue of 128, 8 context lines, 64 breadcrumbs, error chains capped at 100.

### Attributes, breadcrumbs

```go
client.SetAttribute("user.id", "u-42")        // safe from any goroutine
client.SetAttribute("hostname", nil)          // nil removes an attribute, SDK defaults included
client.AddBreadcrumb(bt.Breadcrumb{
	Message: "checkout started",
	Level:   bt.BreadcrumbInfo,
})
```

Values are normalised when you hand them over: the SDK keeps its own detached copy, made of plain JSON-native types, so you can reuse or change your maps and slices as soon as the call returns. Types with their own JSON or text form keep it (`time.Time`, a UUID, `net.IP`), an `error` becomes its message and a struct becomes a map of its exported fields. Field names follow the struct's `json` tags. See [MIGRATION.md](MIGRATION.md#values-are-normalised-when-you-report) for the full table and the size limits.

### BeforeSend

`BeforeSend` runs on the SDK worker just before a report is sent. Return the report (changed or not) to send it, or `nil` to drop it.

```go
BeforeSend: func(r *bt.ReportData) *bt.ReportData {
	if errors.Is(r.Error, context.Canceled) { // r.Error is the reported error, never sent
		return nil
	}
	// Attribute values have one of these types, whatever you passed in:
	// nil, bool, string, int64, uint64, float64, []interface{}, map[string]interface{}
	if headers, ok := r.Attributes["headers"].(map[string]interface{}); ok {
		delete(headers, "Authorization")
	}
	r.SourceCode = nil // removals stick: no source text is sent
	return r
},
```

The hook works on its own copy of the report, so an edit never shows up in a later one. Everything can be changed except the report id; the SDK then repairs only what the submission format requires (a positive timestamp, at least one thread, a main thread among them). After the hook, attribute values are converted to the strings, booleans and non-negative integers the backend stores. Maps and slices move to the annotations.

Every report automatically includes: hostname, process ID and age, Go version, goroutine count, heap statistics, GC count, CPU architecture and model, OS version, `application.version` / `vcs.revision` (from Go build info), the Go module dependency list, and — on Linux — `/proc` memory and scheduler attributes. [Crash reports](#fatal-crashes) leave out the goroutine count, heap and GC statistics and `/proc` attributes. A stable machine identifier (`guid`) is opt-in via `SendMachineID`. Machine metadata is gathered once per process with native file, syscall, and registry reads — no shell pipelines (macOS additionally runs a single time-bounded `ioreg` probe for the opt-in machine identifier).

### Request scopes

Client attributes and breadcrumbs apply to every report. Data that belongs to one request goes on a scope in the request's context:

```go
ctx = bt.WithScope(ctx) // child of any scope ctx already carries
scope := bt.ScopeFromContext(ctx)
scope.SetAttribute("user.id", "u-42")
scope.AddBreadcrumb(bt.Breadcrumb{Message: "checkout started", Type: bt.BreadcrumbTypeUser})
scope.AddAttachment("/var/log/shop/tenant-42.log")

client.ReportContext(ctx, err, nil)
```

Reports made with that context (`ReportContext`, `ReportPanicValueContext`, `ReportPanicValueAndFlush`) carry the data of the scope and of its parent scopes. When attributes share a key, the later one in this list wins: SDK defaults, `Config.Attributes`, client attributes, outer scopes, inner scopes, per-call attributes.

### net/http middleware

```go
import "github.com/backtrace-labs/backtrace-go/bthttp"

handler := bthttp.New(bthttp.Options{
	Client:          client, // omit to use the global reporter
	Repanic:         false,  // re-raise after reporting
	WaitForDelivery: true,   // block the failing request until delivered (bounded by FlushTimeout)
	SendDefaultPII:  false,  // raw path/host/remote addr/user agent are OPT-IN
	ReportStatus: func(status int, r *http.Request) bool {
		return status >= 500 // also report 5xx responses without a panic
	},
})
http.ListenAndServe(":8080", handler.Handle(mux))
```

By default panic reports carry `request.method`, `request.proto`, and the registered route pattern (`request.route`). Raw URL path, host, remote address, and user agent require `SendDefaultPII: true`; use `RequestAttributes` to contribute application-approved metadata. Routers that serve a copy of the request can name the route with `bthttp.SetRoute(r.Context(), pattern)`. When a swallowed panic left the response uncommitted, the middleware writes `500 Internal Server Error` instead of an empty `200`. When the response was already committed, the middleware panics with `http.ErrAbortHandler`, and the client sees a broken connection instead of a truncated response.

Handlers receive the middleware's own `http.ResponseWriter`. It offers `http.Flusher` and `http.Hijacker` whenever the writer underneath (or one behind its `Unwrap` method) supports them, and works with `http.NewResponseController`; a response that was flushed, or a connection that was hijacked, is never written to after a panic.

- Each request runs under its own scope. Attributes and breadcrumbs that a handler adds with `bt.ScopeFromContext(r.Context())` reach that request's reports only.
- Panics with `http.ErrAbortHandler` (used by `httputil.ReverseProxy`) are re-raised without a report.

### net/http transport

`bthttp.NewTransport` records each outbound request as an `http` breadcrumb on the scope of the request's context: method, host, status or error, and duration. Path and query are added only with `SendDefaultPII`. A nil base uses the current `http.DefaultTransport`. Assigning the result to `http.DefaultTransport` instruments every client that uses it.

```go
outbound := &http.Client{Transport: bthttp.NewTransport(nil, bthttp.TransportOptions{})}
req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, inventoryURL, nil)
resp, err := outbound.Do(req)
```

## log/slog

```go
import "github.com/backtrace-labs/backtrace-go/btslog"

logger := slog.New(btslog.NewHandler(slog.NewJSONHandler(os.Stderr, nil), btslog.Options{
	Client:          client,          // omit to use bt.DefaultClient()
	ReportLevel:     slog.LevelError, // default
	BreadcrumbLevel: slog.LevelInfo,  // default
}))
logger.InfoContext(ctx, "charging card", "order", id) // log breadcrumb
logger.ErrorContext(ctx, "charge failed", "err", err) // report
```

- Every record still goes to the wrapped handler.
- Records at or above `ReportLevel` are reported with the record's context. The reported error is the first error-valued attribute, `err` and `error` keys first, and the message goes to `log.message`. A record without one reports its message. Other attributes become report attributes, prefixed by their group names (`request.method`).
- Records below `ReportLevel` and at or above `BreadcrumbLevel` become `log` breadcrumbs on the context's scope, or on the client when the context has none.
- Without `CaptureAllGoroutines`, the report's stack starts at the logging call when the record carries its PC. For `log` output sent to slog by `slog.SetDefault`, this needs `log.Lshortfile` or `log.Llongfile` set before that call.

## Legacy global API

The historical package-level API keeps working unchanged:

```go
import bt "github.com/backtrace-labs/backtrace-go"

func init() {
	bt.Options.Endpoint = "https://submit.backtrace.io/{universe}/{token}/json"
}

func foo() {
	if err := doWork(); err != nil {
		bt.Report(err, nil)
	}
}
```

Notes:

- Configure `bt.Options` before the first report. For attribute changes at runtime use `bt.SetAttribute` / `bt.SetAttributes`, which are safe for concurrent use.
- `bt.SetDefaultClient(client)` makes the package-level functions, and libraries that call them, use a `NewClient` client. `bt.DefaultClient()` returns the client they use, or nil when reporting is not configured.
- `bt.FinishSendingReports()` now waits for queued reports **without** stopping the reporter (historically it killed the sender permanently): prefer `bt.Flush(timeout)`.
- Source capture now defaults to path/line metadata only — no source text leaves the host. Opt in with `Options.SourceCode = bt.SourceCodeContext` (snippets) or `bt.SourceCodeFile` (whole files), optionally constrained by `Options.SourceRoots`.
- The reporting API (`Client` methods, `bt.Report`, `bt.ReportPanic`, ...) never panics; `DebugBacktrace` only controls diagnostic logging. (The bcd tracing integration may panic on tracer kill failure unless `GlobalConfig.PanicOnKillFailure` is disabled via `bt.UpdateConfig`.)

## Thread-safety contract

`Client` methods, the package-level reporting functions, `SetAttribute`, `AddBreadcrumb`, `Flush`, and `Close` are safe for concurrent use. `Scope` methods are also safe for concurrent use. The `Options` struct is read when reports are captured; mutate it only before reporting starts (or via `SetAttribute`). `NewClient` copies `Config` with its maps and slices.

## Extension points and guarantees

The SDK's guarantees are precise, and it is worth knowing exactly where they end:

- **SDK-controlled queue and network operations are bounded.** Submission carries an SDK-owned deadline, the queue never blocks callers, and shutdown is time-bounded.
- **Panics from your code are contained.** Panicking `Error()`, `String()`, `MarshalJSON`, `MarshalText`, `IsZero`, `BeforeSend`, `Logger`, `RequestAttributes`, `ReportStatus`, and `Tracer` implementations, and a panicking HTTP client or `RoundTripper` you supplied, are recovered and reported as errors or dropped reports — never a process crash.
- **Blocking is a different matter.** Go cannot forcibly stop a callback that never returns. Custom `Tracer`s, `BeforeSend`, `RequestAttributes`, `ReportStatus`, `Logger`s, and `http.RoundTripper`s are **trusted extension points and must return promptly**; one that blocks forever will hold the goroutine that called it.
- **Credential redaction covers SDK-known credentials.** Backtrace submission tokens are stripped from every SDK diagnostic and error. Report *content* is yours: attributes, breadcrumbs, attachments, error messages, and (with `SendEnvVars`) environment values may contain secrets. Prefer `EnvVars` (exact allowlist) and scrub with `BeforeSend`; the built-in name/shape redaction is defense in depth, not a proof.
- **Values you pass are detached when the call returns.** Attribute and breadcrumb values are normalised into the SDK's own copy on your goroutine, so you may mutate your maps afterwards. The work is bounded (nesting, number of values, bytes): an oversized value is cut with a visible marker instead of being walked in full. Do not mutate a collection *concurrently with* the reporting call itself.
- **Redirects are never followed.** A report or snapshot is sent to the configured endpoint only. This also holds for an `http.Client` you pass in: the SDK works on a copy whose `CheckRedirect` it replaces.

## Migration

- [MIGRATION.md](MIGRATION.md) — upgrading from pre-1.1.0 (Go floor, source capture opt-in, panic-helper semantics, shutdown behavior).

## bcd (out-of-process tracing)

The `bt` package also provides integration with out-of-process tracers.
Using the provided `Tracer` interface, applications may invoke tracer execution on demand: panic and signal handling integrations are provided.
A default `Tracer` implementation for the Backtrace platform (`BTTracer`, Linux/FreeBSD) is included.
On other platforms `BTTracer` is a stub, and `bt.Trace` returns an error wrapping `bt.ErrUnsupportedPlatform`.

Faults in cgo code, runtime fatal errors and unrecovered panics end the process before a tracer can run. `MonitorCrashes` reports those.

See the [godoc](https://pkg.go.dev/github.com/backtrace-labs/backtrace-go)
and [examples/bcd/main.go](examples/bcd/main.go).

## Examples

- [examples/report](examples/report/main.go) — error reporting, breadcrumbs, BeforeSend, middleware.
- [examples/bcd](examples/bcd/main.go) — tracer integration: signals, panic recovery, snapshot upload.

Both send to a local server that prints what it receives unless `BACKTRACE_ENDPOINT` is set (bcd also needs `BACKTRACE_TOKEN`). The [godoc](https://pkg.go.dev/github.com/backtrace-labs/backtrace-go) has examples for `bt`, `bthttp` and `btslog`.

## Development

### Getting started (macOS)

1. Install Go (1.25+ required; CI also tests 1.26 and 1.27) with [Homebrew](https://brew.sh):

   ```sh
   brew install go
   go version
   ```

2. Add Go's tool directory to your `PATH` so that installed tools such as `golangci-lint` and `govulncheck` are found. Add this line to `~/.zshrc`:

   ```sh
   export PATH="$PATH:$(go env GOPATH)/bin"
   ```

   Then reload your shell config: `source ~/.zshrc`.

3. Install the lint and vulnerability tools at the versions CI uses:

   ```sh
   make tools   # golangci-lint v2.15.0 + govulncheck v1.1.4
   ```

   You can also run `brew install golangci-lint`, but that installs the latest release, which may report different findings than CI.

4. Fetch dependencies and verify the setup:

   ```sh
   go mod download
   make check   # gofmt, vet, race tests, lint, go.mod tidiness — same as CI
   ```

### Make targets

```
make help    # list targets
make check   # run the main CI checks locally
make race    # go test -race ./...
make lint    # golangci-lint v2
make vuln    # govulncheck ./...
make cross   # cross-compile all supported platforms
```
