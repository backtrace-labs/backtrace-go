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

- `https://submit.backtrace.io/{universe}/{token}/json` | `Endpoint` only |
- `https://{universe}.sp.backtrace.io` | `Endpoint` + `Token` |

`BACKTRACE_ENDPOINT` and `BACKTRACE_TOKEN` environment variables are used as fallbacks when the corresponding fields are empty.

## Reporting

```go
client.Report(err, nil)                          // error (type + unwrap chain captured)
client.ReportMessage("cache warmup skipped", nil) // plain message
client.ReportPanicValue(recovered, nil)           // recovered panic value

// Panic capture with defer:
defer bt.ReportPanic(nil)           // reports, flushes, re-panics
defer bt.ReportAndRecoverPanic(nil) // reports and swallows the panic
```

Reporting never blocks the caller on network I/O: reports are queued to a background worker, and when the queue is full new reports are dropped and counted (`client.DroppedReports()`) instead of stalling the application.

Delivery lifecycle:

```go
client.Flush(5 * time.Second) // wait for queued reports; client stays usable
client.Close()                // drain, stop the worker, release the client
```

## Configuration

```go
client, err := bt.NewClient(bt.Config{
	Endpoint:             "https://submit.backtrace.io/{universe}/{token}/json",
	CaptureAllGoroutines: true,                 // include every goroutine's stack
	SourceCode:           bt.SourceCodeContext, // context lines (default), File, or None
	ContextLineCount:     8,                    // lines above/below each frame
	Attributes: map[string]interface{}{         // stamped on every report
		"application.environment": "production",
	},
	AttachmentPaths: []string{"/var/log/app.log"}, // uploaded with every report
	SendEnvVars:     true,  // env vars as annotation, secrets redacted
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

Every report automatically includes: hostname, process ID and age, Go version, goroutine count, heap statistics, GC count, CPU architecture and model, OS version, machine GUID, `application.version` / `vcs.revision` (from Go build info), the Go module dependency list, and — on Linux —`/proc` memory and scheduler attributes.

### net/http middleware

```go
import "github.com/backtrace-labs/backtrace-go/bthttp"

handler := bthttp.New(bthttp.Options{
	Client:          client, // omit to use the global reporter
	Repanic:         false,  // re-raise after reporting
	WaitForDelivery: true,   // block the failing request until delivered
})
http.ListenAndServe(":8080", handler.Handle(mux))
```

Panics in handlers are reported with `request.url`, `request.method`,
`request.remote_addr`, and `request.user_agent` attributes.

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
- Source capture now defaults to context lines around each frame instead of whole files: opt back in with `Options.SourceCode = bt.SourceCodeFile`.
- The SDK never panics. `DebugBacktrace` only controls diagnostic logging.

## Thread-safety contract

`Client` methods, the package-level reporting functions, `SetAttribute`,
`AddBreadcrumb`, `Flush`, and `Close` are safe for concurrent use. The
`Options` struct and `Config` maps are read when reports are captured;
mutate them only before reporting starts (or via `SetAttribute`).

# bcd (out-of-process tracing)

The `bt` package also provides integration with out-of-process tracers.
Using the provided `Tracer` interface, applications may invoke tracer execution on demand: panic and signal handling integrations are provided.
A default `Tracer` implementation for the Backtrace platform (`BTTracer`, Linux/FreeBSD) is included.

See the [godoc](https://pkg.go.dev/github.com/backtrace-labs/backtrace-go)
and [examples/bcd/main.go](examples/bcd/main.go).

## Examples

- [examples/report](examples/report/main.go) — error reporting, breadcrumbs, BeforeSend, middleware.
- [examples/bcd](examples/bcd/main.go) — tracer integration: signals, panic recovery, snapshot upload.

## Development

```
make help    # list targets
make race    # go test -race ./...
make lint    # golangci-lint v2
make cross   # cross-compile all supported platforms
```
