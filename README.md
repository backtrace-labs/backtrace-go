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

Regular `Report*` calls never block on SDK-controlled network I/O or queue admission: reports are queued to a background worker, and a full queue drops the newest report and counts it (`client.Stats()`, `client.DroppedReports()`) instead of stalling the application. The SDK-controlled waits are explicitly bounded: `Flush`, `Close`, `ReportPanicValueAndFlush`, and `bt.ReportPanic` (one 5s deadline); `ReportAndRecoverPanic` reports without waiting. The `…Context` variants obey the caller's context — passing `context.Background()` means no deadline.

See [Extension points and guarantees](#extension-points-and-guarantees) for the one caveat: user-supplied callbacks are trusted to return promptly.

Delivery lifecycle:

```go
client.Flush(5 * time.Second)  // wait for reports queued before the call; client stays usable
client.FlushContext(ctx)       // context-aware variant
client.Close()                 // drain and stop, bounded by ShutdownTimeout (default 5s)
client.CloseContext(ctx)       // explicit deadline; cancels in-flight sends when it expires
```

Flushing proves local processing and send completion, not backend acceptance. While the SDK is unconfigured, `bt.ReportPanic`/`bt.ReportAndRecoverPanic` do nothing — in particular they do not recover, so the application's panic proceeds unchanged.

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
client.AddBreadcrumb(bt.Breadcrumb{
	Message: "checkout started",
	Level:   bt.BreadcrumbInfo,
})
```

Values are normalised when you hand them over: the SDK keeps its own detached copy, made of plain JSON-native types, so you can reuse or change your maps and slices as soon as the call returns. Types with their own JSON or text form keep it (`time.Time`, a UUID, `net.IP`), an `error` becomes its message and a struct becomes a map of its exported fields. See [MIGRATION.md](MIGRATION.md#values-are-normalised-when-you-report) for the full table and the size limits.

### BeforeSend

`BeforeSend` runs on the SDK worker just before a report is sent. Return the report (changed or not) to send it, or `nil` to drop it.

```go
BeforeSend: func(r *bt.ReportData) *bt.ReportData {
	// Attribute values have one of these types, whatever you passed in:
	// nil, bool, string, int64, uint64, float64, []interface{}, map[string]interface{}
	if headers, ok := r.Attributes["headers"].(map[string]interface{}); ok {
		delete(headers, "Authorization")
	}
	r.SourceCode = nil // removals stick: no source text is sent
	return r
},
```

The hook works on its own copy of the report, so an edit never shows up in a later one. Everything can be changed except the report id; the SDK then repairs only what the submission format requires (a positive timestamp, at least one thread, a main thread among them).

Every report automatically includes: hostname, process ID and age, Go version, goroutine count, heap statistics, GC count, CPU architecture and model, OS version, `application.version` / `vcs.revision` (from Go build info), the Go module dependency list, and — on Linux — `/proc` memory and scheduler attributes. A stable machine identifier (`guid`) is opt-in via `SendMachineID`. Machine metadata is gathered once per process with native file, syscall, and registry reads — no shell pipelines (macOS additionally runs a single time-bounded `ioreg` probe for the opt-in machine identifier).

### net/http middleware

```go
import "github.com/backtrace-labs/backtrace-go/bthttp"

handler := bthttp.New(bthttp.Options{
	Client:          client, // omit to use the global reporter
	Repanic:         false,  // re-raise after reporting
	WaitForDelivery: true,   // block the failing request until delivered (bounded by FlushTimeout)
	SendDefaultPII:  false,  // raw path/host/remote addr/user agent are OPT-IN
})
http.ListenAndServe(":8080", handler.Handle(mux))
```

By default panic reports carry `request.method`, `request.proto`, and the registered route pattern (`request.route`). Raw URL path, host, remote address, and user agent require `SendDefaultPII: true`; use `RequestAttributes` to contribute application-approved metadata. When a swallowed panic left the response uncommitted, the middleware writes `500 Internal Server Error` instead of an empty `200`.

Handlers receive the middleware's own `http.ResponseWriter`. It offers `http.Flusher` and `http.Hijacker` whenever the writer underneath (or one behind its `Unwrap` method) supports them, and works with `http.NewResponseController`; a response that was flushed, or a connection that was hijacked, is never written to after a panic.

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
- `bt.FinishSendingReports()` now waits for queued reports **without** stopping the reporter (historically it killed the sender permanently): prefer `bt.Flush(timeout)`.
- Source capture now defaults to path/line metadata only — no source text leaves the host. Opt in with `Options.SourceCode = bt.SourceCodeContext` (snippets) or `bt.SourceCodeFile` (whole files), optionally constrained by `Options.SourceRoots`.
- The reporting API (`Client` methods, `bt.Report`, `bt.ReportPanic`, ...) never panics; `DebugBacktrace` only controls diagnostic logging. (The bcd tracing integration may panic on tracer kill failure unless `GlobalConfig.PanicOnKillFailure` is disabled via `bt.UpdateConfig`.)

## Thread-safety contract

`Client` methods, the package-level reporting functions, `SetAttribute`, `AddBreadcrumb`, `Flush`, and `Close` are safe for concurrent use. The `Options` struct and `Config` maps are read when reports are captured; mutate them only before reporting starts (or via `SetAttribute`).

## Extension points and guarantees

The SDK's guarantees are precise, and it is worth knowing exactly where they end:

- **SDK-controlled queue and network operations are bounded.** Submission carries an SDK-owned deadline, the queue never blocks callers, and shutdown is time-bounded.
- **Panics from your code are contained.** Panicking `Error()`, `String()`, `MarshalJSON`, `MarshalText`, `BeforeSend`, `Logger`, `RequestAttributes`, and `Tracer` implementations, and a panicking HTTP client or `RoundTripper` you supplied, are recovered and reported as errors or dropped reports — never a process crash.
- **Blocking is a different matter.** Go cannot forcibly stop a callback that never returns. Custom `Tracer`s, `BeforeSend`, `RequestAttributes`, `Logger`s, and `http.RoundTripper`s are **trusted extension points and must return promptly**; one that blocks forever will hold the goroutine that called it.
- **Credential redaction covers SDK-known credentials.** Backtrace submission tokens are stripped from every SDK diagnostic and error. Report *content* is yours: attributes, breadcrumbs, attachments, error messages, and (with `SendEnvVars`) environment values may contain secrets. Prefer `EnvVars` (exact allowlist) and scrub with `BeforeSend`; the built-in name/shape redaction is defense in depth, not a proof.
- **Values you pass are detached when the call returns.** Attribute and breadcrumb values are normalised into the SDK's own copy on your goroutine, so you may mutate your maps afterwards. The work is bounded (nesting, number of values, bytes): an oversized value is cut with a visible marker instead of being walked in full. Do not mutate a collection *concurrently with* the reporting call itself.
- **Redirects are never followed.** A report or snapshot is sent to the configured endpoint only. This also holds for an `http.Client` you pass in: the SDK works on a copy whose `CheckRedirect` it replaces.

## Migration and security

- [MIGRATION.md](MIGRATION.md) — upgrading from pre-1.1.0 (Go floor, source capture opt-in, panic-helper semantics, shutdown behavior).
- [SECURITY.md](SECURITY.md) — private vulnerability reporting.

## Scope

This SDK reports errors, messages, and recovered panics from within the process. Crashes that bypass Go panics (native/cgo faults, runtime aborts) require out-of-process capture: use the bcd tracer integration below, or the Backtrace Coresnap workflow, for robust fatal-crash coverage.

## bcd (out-of-process tracing)

The `bt` package also provides integration with out-of-process tracers.
Using the provided `Tracer` interface, applications may invoke tracer execution on demand: panic and signal handling integrations are provided.
A default `Tracer` implementation for the Backtrace platform (`BTTracer`, Linux/FreeBSD) is included.

See the [godoc](https://pkg.go.dev/github.com/backtrace-labs/backtrace-go)
and [examples/bcd/main.go](examples/bcd/main.go).

## Examples

- [examples/report](examples/report/main.go) — error reporting, breadcrumbs, BeforeSend, middleware.
- [examples/bcd](examples/bcd/main.go) — tracer integration: signals, panic recovery, snapshot upload.

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
