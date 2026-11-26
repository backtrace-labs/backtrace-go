# Backtrace Go Release Notes

## Version 1.1.0 (unreleased)

This release rebuilds the SDK around a client that does not block or crash the application. It adds request scopes, crash monitoring, an HTTP middleware and a `log/slog` handler.

The 1.0.x API compiles unchanged, but some defaults and report contents are different. Read the [migration guide](MIGRATION.md) before you upgrade. The [README](README.md) describes the new APIs.

### Requirements

- Go 1.25 or later. 1.0.x needed Go 1.22.
- `golang.org/x/sys` is the only dependency. `github.com/google/uuid` and `github.com/stretchr/testify` are no longer used.

### Client

- `bt.NewClient(bt.Config{...})` returns a `*bt.Client` with its own settings, queue and statistics. It reports with `Report`, `ReportError`, `ReportMessage`, `ReportPanicValue`, `ReportPanic` and `ReportAndRecoverPanic`. Several clients can run in one process. Client methods are safe for concurrent use and do nothing on a nil client.
- The package-level functions (`bt.Report`, `bt.ReportPanic`, `bt.ReportAndRecoverPanic`, `bt.FinishSendingReports`) now run on a default client built from `bt.Options`. `bt.SetDefaultClient` makes them use a client you created. `bt.DefaultClient` returns the client in use.
- New package-level functions: `bt.SetAttribute`, `bt.SetAttributes`, `bt.AddBreadcrumb`, `bt.Flush`, `bt.ReportPanicValue` and `bt.ReportPanicValueAndFlush`.
- `bt.Options` has new fields for the new settings, such as `SourceCode`, `BeforeSend`, `SampleRate` and `HTTPClient`. They work like the `Config` fields of the same name.
- The environment variables `BACKTRACE_ENDPOINT` and `BACKTRACE_TOKEN` are used when the endpoint and the token are not set in code.
- `NewClient` checks the configuration and returns an error for an invalid value, such as an endpoint without a host or a limit above its allowed maximum. The package-level functions check `bt.Options` the same way. An invalid setting there turns reporting off, and the reason is logged once to stderr.
- `Config.BeforeSend` is a hook that can change or drop a report before it is sent. The hook works on its own copy of the report. Data that the hook removes is not added back, except for four things that every report needs: the report ID, a timestamp, one classifier and one thread. A hook that panics drops the report.
- `Config.SampleRate` sends a fraction of the error and message reports. Sampling does not apply to panic reports. `Config.ReportsPerMinute` caps the reports a client accepts in any 60-second window.
- `Client.Stats()` returns the number of reports accepted, the number delivered, and the number dropped for each reason. `Client.DroppedReports()` returns the total number of dropped reports.
- `Config.Debug` turns on short diagnostic lines, and `Config.Logger` sets where they go (stderr by default). They never contain report contents, submission tokens or URL credentials.
- The reporting API never panics. The SDK recovers from a panic in code the application supplies (`Error`, `String`, `MarshalJSON`, `BeforeSend`, `Logger`, an HTTP client).

### Request scopes

- `bt.WithScope(ctx)` puts a scope in a context, and `bt.ScopeFromContext(ctx)` returns it. A scope holds the attributes, breadcrumbs and attachments of one request.
- `ReportContext`, `ReportPanicValueContext` and `ReportPanicValueAndFlush` add the data of the scope, and of its parent scopes, to the report.

### Report contents

- Classifiers name the Go type of the error, for example `*strconv.NumError`. 1.0.x sent `error`. For a wrapped error, the type is the innermost one on the unwrap path that is not a plain `errors` or `fmt` type, such as `*errors.errorString` or `*fmt.wrapError`. When the path holds only such types, the innermost one is used.
- Panics with a non-error value use the classifier `panic`, and message reports keep `message`. `report_type` keeps its 1.0.x values.
- New `error.type` attribute: `Exception` for errors, `Message` for messages and `Unhandled exception` for panics.
- Errors add an `Error Chain` annotation with the type and message of each error in the unwrap graph, `errors.Join` trees included.
- An error that carries its own stack is grouped by where it was created. The stack comes from a `Callers() []uintptr` method, or from a `StackTrace()` method that returns program counters. It becomes the main thread of the report, and the reporting goroutine is kept as a second thread.
- Panic reports mark the frame that called `panic` as faulted.
- Values are copied when you report them. You can change your maps and slices as soon as the call returns. Each call has limits on nesting, number of values and size, and a value over a limit is cut and marked.
- Attribute values are sent as strings, booleans and non-negative integers, the types the backend stores. Negative and fractional numbers are sent as strings. Maps and slices move to the annotations.
- A nil attribute value removes the attribute from the report. This also works for the default attributes.
- `Options.Attributes` starts empty. 1.0.x filled it with the default attributes when the package was imported. The default attributes are now added to each report.
- `error.message` is capped at 1024 bytes. A longer message is sent in full in the `Message` annotation.
- A report over 8 MiB (`Config.MaxReportBytes`) is trimmed and gets a `backtrace.truncated` attribute. A report that still does not fit is dropped.
- `AddBreadcrumb` keeps the last 64 breadcrumbs (`Config.MaxBreadcrumbs`). They are sent as a JSON attachment named `bt-breadcrumbs-0`, the file the Backtrace breadcrumb view reads.
- `Config.AttachmentPaths`, `ReportData.Attachments` and `Scope.AddAttachment` send files as attachments of a report. The defaults allow 16 attachments, 10 MiB per file and 25 MiB per report. The breadcrumb file counts toward these limits and is added last. FIFOs and other non-regular files are skipped.
- More default attributes: `go.version`, `cpu.count`, `process.age`, the goroutine count, heap and GC statistics, and `application.version` and `vcs.*` from the Go build info. Reports add a `Dependencies` annotation with the module dependencies of the build.
- The attributes read from `/proc` on Linux, such as `system.memory.*` and `vm.*`, are numbers. 1.0.x sent them as strings.
- `cpu.brand` and `uname.version` are read from files, syscalls and the registry when the first report is processed. 1.0.x ran external commands when the package was imported.

### Privacy

- By default, reports contain no source text. Each stack frame carries its file path and line number only. 1.0.x sent the whole text of every source file on the stack. `Config.SourceCode` turns source text back on, either as the lines around each frame (`bt.SourceCodeContext`) or as whole files (`bt.SourceCodeFile`). `Config.SourceRoots` limits the directories that are read.
- The `guid` attribute (machine ID) is sent only when `Config.SendMachineID` is set. 1.0.x always sent it. On macOS it comes from one run of `ioreg`, without a shell.
- `Config.EnvVars` sends only the environment variables it names. `Config.SendEnvVars` still sends every environment variable, up to 1024 variables and 256 KiB (`Config.MaxEnvVars`, `Config.MaxEnvBytes`). `Config.SendEnvVars` is ignored when `Config.EnvVars` is set.
- Environment values are redacted when the variable name contains `TOKEN`, `SECRET`, `PASS`, `KEY` or a similar word, or when the value holds a URL with a password. `Config.ScrubEnvVars` adds name patterns. 1.0.x redacted nothing.
- `DebugBacktrace` no longer prints reports. It logs short summaries to `Options.Logger`. 1.0.x printed every report, and the submission URL with its token.

### Delivery

- Report calls never wait for the network or for room in the queue. Reports go to a queue that holds 128 reports (`Config.QueueSize`). A report that arrives when the queue is full is dropped and counted. 1.0.x blocked the caller once 50 reports were waiting.
- Each submission attempt is limited to 30 seconds (`Config.Timeout`). 1.0.x used `http.Post` without a timeout, so an endpoint that never answered stopped all reporting.
- The SDK sends through its own HTTP client. An `http.Client` passed in `Config.HTTPClient` is copied, and the original is never changed.
- Request bodies are gzip-compressed. `Config.DisableCompression` turns that off.
- A submission is retried up to `Config.MaxRetries` times (2 by default) after a network error that happened before the request body was sent, and after a 408, 500, 502, 503 or 504 response.
- A 429 response pauses submissions for the time in its `Retry-After` header, at most 5 minutes, or for 1 minute when the header is missing. Five network failures in a row pause submissions for 30 seconds. Reports are dropped and counted while submissions are paused.
- Redirects are never followed. A report is sent to the configured endpoint only, and a 3xx response counts as a rejected report.
- `Flush(timeout)` and `FlushContext(ctx)` wait for the reports queued before the call. `Close()` sends what is queued and stops the client. It waits at most `Config.ShutdownTimeout` (5 seconds by default) and drops the reports that are not sent by then. `CloseContext(ctx)` takes its deadline from the context.
- `ReportPanic` sends the panic report ahead of the queued reports. It waits up to 5 seconds for that report and then panics again. `ReportPanicValueAndFlush` sends a report in the same way for a panic that the application recovered itself. Its wait ends at the deadline of its context, or after 5 seconds when the context has no deadline.

### Crash monitoring

- `bt.MonitorCrashes(cfg)` reports what `recover` cannot catch: an unrecovered panic in any goroutine, a runtime fatal error such as `concurrent map writes`, and a fatal signal such as `SIGSEGV`. It re-runs the executable as a monitor process that reads the runtime's crash output. A `SIGKILL` is not reported.
- `bt.MonitorCrashes` returns an error in four cases: the configuration is invalid, a monitor is already running, the application is PID 1 on Linux, or the monitor process does not start within 5 seconds. The application then continues without a monitor.

### HTTP and logging integrations

- New `bthttp` package. `bthttp.New(opts).Handle(next)` reports panics in HTTP handlers with the request method and protocol, and with the route pattern when one is known. The raw path, host, remote address and user agent are sent only with `SendDefaultPII`.
- Each request runs under its own scope. `RequestAttributes` adds attributes the application chooses. `ReportStatus` is a hook that decides, from the status code, whether a response without a panic is reported too. For example, it can report every 5xx response.
- With `Repanic`, the middleware panics again after the report. With `WaitForDelivery`, it waits until the report is delivered, up to `FlushTimeout` (2 seconds by default).
- `bthttp.SetRoute` names the route for routers that do not set it.
- A panic with `http.ErrAbortHandler` is not reported. The middleware panics again with the same value.
- When a handler panics before it starts the response, the middleware answers `500`, unless `Repanic` is set. When the response has already started, the middleware panics with `http.ErrAbortHandler`, so the program that sent the request sees a broken connection instead of a truncated response.
- The response writer that handlers receive implements `http.Flusher`, `http.Hijacker`, `http.Pusher` and `io.ReaderFrom` when the writer it wraps supports them.
- `bthttp.NewTransport` records each outbound request as an `http` breadcrumb on the scope of the request's context.
- New `btslog` package. `btslog.NewHandler` wraps a `slog.Handler`. Records at `slog.LevelError` or above are reported. Records below that level and at `slog.LevelInfo` or above become breadcrumbs. Both levels can be changed.

### Fixes

- `FinishSendingReports` can be called more than once and waits at most 30 seconds. Reporting keeps working after the call. In 1.0.x the first call stopped the reporter permanently, and a second call never returned.
- In 1.0.x `ReportPanic` could panic again while the panic report was still in the queue behind other reports, and the report was lost. `ReportPanic` now waits for its own report first.
- Report IDs are random version 4 UUIDs from `crypto/rand`. 1.0.x drew them from a generator with a 15-bit seed, so two processes could send the same IDs.
- The SDK no longer panics when `DebugBacktrace` is set. With that option, 1.0.x panicked on a missing endpoint or token and on send errors.
- The package-level functions no longer write `report_type` into the caller's attribute map.
- An environment value that contains `=` is no longer cut at the first `=`.
- The stack parser handles Windows paths, function names without a dot and elided-frame lines. 1.0.x panicked on a function name without a dot.
- With `bt.SourceCodeContext`, `ContextLineCount` sets how many source lines are sent above and below the line of each frame. 1.0.x ignored `ContextLineCount` and sent whole files.

### bcd tracer

- `BTTracer`, `bt.New`, `NewOptions` and `PutOptions` exist on every platform. Outside Linux and FreeBSD the tracer is a stub: `bt.Trace`, `ConfigurePut`, `Put`, `PutDir` and `SetOutputPath` return errors wrapping `bt.ErrUnsupportedPlatform`. `EnableTracing` does the same outside Linux. In 1.0.x the macOS stub methods and `EnableTracing` returned nil, and `bt.Trace` with the macOS stub crashed the process.
- A panic in a `Tracer` method other than `Logf`, or in the HTTP client used for snapshot uploads, becomes an error wrapping `bt.ErrTracerCallback`. A panic in `Logf` is ignored.
- `Recover` with `repanic` set always panics again with the application's panic value, even when the tracer fails.
- `Trace` returns an error when the timeout fires before the tracer process started. 1.0.x panicked with a nil pointer dereference.
- By default a snapshot upload times out after 30 seconds (`PutOptions.Timeout`) and is capped at 1 GiB (`PutOptions.MaxSnapshotBytes`). Uploads never follow redirects. Use `PutOptions.HTTPClient` instead of `PutOptions.Client`, which is deprecated.
- `PutDir` keeps going after a failed file and returns all failures joined with `errors.Join`. 1.0.x stopped at the first error. `Put` and `PutDir` refuse symbolic links and non-regular files.
- `DefaultTraceOptions` returns a copy. The new `SetDefaultTraceOptions` replaces the defaults.
- Signal traces from `bt.Register` use the signal name, such as `SIGABRT`, as the classifier. They mark no thread as faulted and include every thread.
- `bt.Register` with an empty, non-nil signal set logs an error and does nothing, as it already did for a nil set. 1.0.x subscribed to every signal in that case.
- `GlobalConfig.ResendSignal` is read when each signal arrives. 1.0.x read it once in `bt.Register`.
- `TraceOptions.Faulted` and `TraceOptions.CallerOnly` work on `linux/arm`. 1.0.x could not read the thread ID there and skipped both options.

### Development

- CI tests Go 1.25, 1.26 and 1.27 on Linux, macOS and Windows with the race detector. It also runs `govulncheck`, a short fuzz run, cross-builds, and `golangci-lint` for Linux, macOS, Windows and FreeBSD.
- `make check` runs the main CI checks locally, and `make tools` installs the lint and vulnerability tools at the versions CI uses.
- Fuzz tests cover the stack parser, value conversion, URL redaction, `Retry-After` parsing and multipart naming.
- The repository has an MIT `LICENSE` file.

## Version 1.0.2

- Fixed a data race when memory information from `/proc` is added to a report. ([#28](https://github.com/backtrace-labs/backtrace-go/pull/28))
- The SDK no longer ends the process when a `/proc` file cannot be opened. ([#28](https://github.com/backtrace-labs/backtrace-go/pull/28))

## Version 1.0.1

- Fixed the function name of stack frames whose argument list contains `...`, for example `main.f(...)`. The name was sent as `)`. ([#26](https://github.com/backtrace-labs/backtrace-go/pull/26))

## Version 1.0.0

- First tagged release: package-level `Options` API, JSON submission and the bcd tracer integration.
