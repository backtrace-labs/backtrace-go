# Migration guide

## Upgrading to v1.1.0 from v1.0.x

The 1.0.x API (`bt.Options`, `bt.Report`, `bt.ReportPanic`, `bt.ReportAndRecoverPanic`, `bt.FinishSendingReports` and the bcd tracer API) compiles and works unchanged. The sections below list what behaves differently after the upgrade. The [README](README.md) describes the new APIs.

### Go version

The module requires Go 1.25 or later. 1.0.x required Go 1.22. If you must build with an older Go version, stay on 1.0.x.

### Source code capture is opt-in

1.0.x read every source file on the stack and sent its whole text with each report. Reports now carry file paths and line numbers only, and no source text leaves the host.

```go
// Send the lines around each frame (recommended when you opt in):
bt.Options.SourceCode = bt.SourceCodeContext
bt.Options.SourceRoots = []string{"/srv/app/src"} // optional: read files under these directories only

// Send whole files, as 1.0.x did, within size limits:
bt.Options.SourceCode = bt.SourceCodeFile
```

- With `bt.SourceCodeContext`, `Options.ContextLineCount` sets how many lines are sent above and below the line of each frame. 1.0.x ignored it.
- With `SourceRoots` set, the SDK reads source files inside those directories only. A root and a stack frame may name the same directory through different symbolic links (`/tmp` and `/private/tmp` on macOS). A symbolic link inside a root must be relative and stay inside the root.
- `ParseThreadsFromStack` follows `Options.SourceCode` too.

### Panic helpers

- While reporting is not configured, `bt.ReportPanic` and `bt.ReportAndRecoverPanic` do nothing, as in 1.0.x. They do not recover, so the application's panic continues.
- When reporting is configured, `bt.ReportPanic` reports the panic, waits at most 5 seconds (`DefaultFlushTimeout`) for the report and then panics again. 1.0.x waited without a time limit.
- `bt.ReportAndRecoverPanic` reports the panic without waiting and recovers it.

### Reporting and delivery

- `Report` never blocks. Reports go to a queue that holds 128 reports (`Options.QueueSize`). A report that arrives when the queue is full is dropped and counted in `bt.DefaultClient().Stats()`. 1.0.x blocked the caller once 50 reports were waiting.
- The package-level functions no longer write `report_type` into the caller's attribute map.
- `FinishSendingReports` waits at most 30 seconds for the queued reports, and reporting keeps working after the call. 1.0.x stopped the reporter permanently. New code should use `bt.Flush(timeout)`, which takes a timeout of your choice.
- A flush proves that the queued reports were processed and sent. It does not prove that the backend accepted them.
- Each submission attempt times out after 30 seconds (`Options.Timeout`). 1.0.x used `http.Post` without a timeout.
- Redirects are not followed. A 3xx response counts as a rejected report (`ServerReject`) and is logged once. Set the final URL as the endpoint. 1.0.x followed redirects, and a 301, 302 or 303 lost the report.
- A submission is retried twice, with a pause between attempts, after a network error that happened before the request body was sent, and after a 408, 500, 502, 503 or 504 response.
- A 429 response pauses submissions for the time in its `Retry-After` header, at most 5 minutes, or for 1 minute when the header is missing. Five network failures in a row pause submissions for 30 seconds. Reports are dropped and counted while submissions are paused.
- Request bodies are gzip-compressed. A proxy in front of Backtrace that cannot handle `Content-Encoding: gzip` needs `Options.DisableCompression`.

### Custom HTTP clients

- Without `Options.HTTPClient` the SDK uses its own clone of `http.DefaultTransport` instead of `http.DefaultClient`.
- An `http.Client` you pass in `Options.HTTPClient` is copied, and the original is never changed. Its `CheckRedirect` function is not used, because the SDK never follows redirects.
- Every submission request carries a deadline set by the SDK, also with your own HTTP client. A `RoundTripper` that ignores the request context can hold the delivery worker, and `Close`, past that deadline. The SDK cannot stop it.

### Report contents

- Classifiers name the Go error type, for example `*strconv.NumError`. 1.0.x sent `error`. The type is the innermost one on the error's unwrap path that is not `*errors.errorString`, `*fmt.wrapError`, `*fmt.wrapErrors`, `*errors.joinError`, or one of the `github.com/pkg/errors` types `*errors.fundamental`, `*errors.withStack` and `*errors.withMessage`. When the path holds only those types, the innermost one is used. Errors from `errors.New`, and from `fmt.Errorf` without `%w`, classify as `*errors.errorString`.
- Panics with a non-error value use the classifier `panic`. 1.0.x sent `message`. Message reports keep `message`.
- Saved queries and alerts that filter on classifiers need updating.
- New `error.type` attribute: `Exception` for errors, `Message` for messages and `Unhandled exception` for panics, unless an attribute sets it. `report_type` keeps its 1.0.x values.
- A nil attribute value removes the attribute. This also works for the default attributes.
- `Options.Attributes` starts empty. 1.0.x filled it with the default attributes when the package was imported. The default attributes are now added to each report. `bt.SetAttribute("hostname", nil)` removes one. Deleting the key from `Options.Attributes` does not.
- `error.message` is capped at 1024 bytes. A longer message is sent in full in the `Message` annotation.
- Errors add an `Error Chain` annotation with the type and message of each error in the unwrap graph. Reports add a `Dependencies` annotation with the module dependencies of the build.
- A report over 8 MiB (`Config.MaxReportBytes`) is trimmed step by step and gets a `backtrace.truncated` attribute. A report that still does not fit is dropped.
- More default attributes: `go.version`, `cpu.count`, `process.age`, the goroutine count, heap and GC statistics, `runtime.gogc`, `runtime.gomemlimit`, and `application.version` and `vcs.*` from the Go build info.
- `cpu.brand` and `uname.version` are read from files, syscalls and the registry when the first report is processed. 1.0.x ran shell pipelines, `lscpu` and `wmic` when the package was imported.

### Machine identifier is opt-in

The `guid` attribute (machine ID) is no longer sent by default. Set `Options.SendMachineID` to send it. 1.0.x always sent it, and fell back to the hostname on Linux.

### Environment variables

- `SendEnvVars` redacts the values of variables whose names contain `TOKEN`, `SECRET`, `PASS`, `KEY` and similar words. A value holding a URL with a password is replaced whole. 1.0.x sent every value without redaction and cut it at its first `=`.
- `SendEnvVars` sends at most 1024 variables and 256 KiB of names and values (`Config.MaxEnvVars`, `Config.MaxEnvBytes`). A variable larger than the byte limit is skipped on its own.
- Prefer the new `Options.EnvVars`. It sends only the variables it names, with the same redaction. The limit on the number of variables does not apply to it. `SendEnvVars` is ignored when `Options.EnvVars` is set.

### Configuration validation

`NewClient` returns an error for an invalid setting. It does not repair the setting. The package-level functions check `bt.Options` the same way: an invalid setting turns reporting off. When an endpoint was set but rejected, the SDK prints one warning line to stderr. An empty endpoint is still a silent no-op.

These settings are invalid:

- A sample rate outside 0 to 1, or NaN. A legacy application with `SampleRate: -1` in `bt.Options` has reporting turned off.
- A negative queue size, timeout, `TabWidth` or `ContextLineCount`.
- An unknown source code mode.
- An endpoint without a host, with user info (`https://user:pass@host`) or with a fragment. 1.0.x sent user info as Basic auth. An `Options.HTTPClient` whose transport sets the `Authorization` header replaces it.
- A `submit.backtrace.io` endpoint without universe, token and format in its path (`/{universe}/{token}/json`).
- A `{universe}.sp.backtrace.io` endpoint without a token.
- A limit above its allowed maximum, for example `MaxBreadcrumbs: 10_000_000`. A negative `MaxErrorDepth` keeps its meaning: no error chain is captured.

Two more changes for the package-level API:

- `BACKTRACE_ENDPOINT` and `BACKTRACE_TOKEN` are used when `Options.Endpoint` and `Options.Token` are empty. A process with `BACKTRACE_ENDPOINT` in its environment reports through the package-level functions.
- `DebugBacktrace` never panics and never prints report payloads. It logs one-line summaries to `Options.Logger` (stderr by default), and names attachments by file name, not by path. 1.0.x panicked on a missing endpoint or token and on send errors, and printed every report.

### Values are normalised when you report

1.0.x sent attribute values unchanged and kept references to the caller's maps. The backend dropped the values it could not store.

`Report*` calls, `SetAttribute(s)`, `AddBreadcrumb`, the scope methods and `NewClient` (for `Config.Attributes`) now copy the values you pass before they return. You can reuse or change your maps and slices as soon as the call returns. The copy holds only these types, and `BeforeSend` sees them:

```
nil, bool, string, int64, uint64, float64, []interface{}, map[string]interface{}
```

The conversion follows `encoding/json`:

| You pass | The report carries |
| --- | --- |
| `int`, `int8`, `int16`, `int32`, `int64` | `int64` |
| `uint`, `uint8`, `uint16`, `uint32`, `uint64`, `uintptr` | `uint64` |
| `float32`, `float64` | `float64` |
| `NaN` and infinities | a marker such as `[bt: non-finite float NaN]` |
| `time.Time` | RFC 3339 text |
| a `json.Marshaler` | what its `MarshalJSON` produces, decoded into the types above |
| an `encoding.TextMarshaler` (`net.IP`, `netip.Addr`, most UUID types) | its text |
| an `error` | its message |
| a struct, or a pointer to one | a map of its exported fields, named by their `json` tags |
| a nil pointer | `nil` |
| `[]byte` | base64 text |
| maps, other slices, arrays | `map[string]interface{}`, `[]interface{}` |
| functions, channels, complex numbers | a marker such as `[bt: unsupported value of type complex128]` |
| a value that contains itself | a `[bt: cycle]` marker at the repeat |

`MarshalJSON`, `MarshalText` and `Error` are your code. They run on the goroutine that reports, during the call. The SDK recovers from a panic in one of them and leaves a marker such as `[bt: MarshalJSON failed for T]`.

Each call has limits: 32 levels of nesting, 2048 values, 64 KiB per string, and `MaxReportBytes` in total (8 MiB by default). Past a limit the SDK stops reading the value:

- A longer string is cut and ends in `…[truncated]`.
- A list that is cut ends in one element such as `[bt: 12 of 40 elements omitted]`.
- A map that is cut carries one `[bt: truncated]` key.
- Deeper nesting becomes `[bt: value truncated: max depth]`.

When the report is sent, after `BeforeSend`, attribute values are converted to the types the backend stores: strings, booleans and non-negative integers. Negative numbers and fractional floats become strings. Maps and slices move to the annotations under the same key, unless an annotation of that name exists.

### BeforeSend: what a hook sees and may change

`BeforeSend` is new in 1.1.0. These rules matter when you move scrubbing code into a hook.

- **Types.** Attribute values arrive in the types listed above. This also holds for the attributes the SDK adds. A hook written against the types you passed in (`map[string]string`, `[]string`, `int`) does not match. Assert the listed types:

  ```go
  BeforeSend: func(r *bt.ReportData) *bt.ReportData {
  	if headers, ok := r.Attributes["headers"].(map[string]interface{}); ok {
  		delete(headers, "Authorization")
  	}
  	return r
  },
  ```

- **Annotations and breadcrumbs.** Annotations keep the SDK's own types: `Environment Variables` is a `map[string]string`, and `Dependencies` is a `map[string]map[string]string` keyed by module path. Breadcrumbs are in `r.Breadcrumbs`, a `[]bt.Breadcrumb`.
- **Removals are kept.** `r.SourceCode = nil`, a trimmed `r.Threads` and a changed `r.Timestamp` are sent as the hook returned them.
- **One report at a time.** The hook edits its own copy. Nothing it changes appears in a later report.
- **What the SDK still enforces.** The report ID (`UUID`) is restored. The SDK then repairs only what the submission format requires: a positive timestamp, a classifier, at least one thread, a main thread that exists, and no frame that refers to a removed source entry.
- **Attachments.** Files you attach are added before the breadcrumb file. The breadcrumb file counts toward `MaxAttachments` and the attachment size limits.

### Stacks and grouping

Backtrace groups reports by the frames of the main thread. These changes regroup some reports after the upgrade:

- Panic reports mark the frame that called `panic` as faulted. The backend groups them from that frame and leaves out the deferred recovery code.
- An error that carries its own stack (a `StackTrace()` method as in `github.com/pkg/errors`, or `Callers() []uintptr`) is grouped by where it was created. That stack becomes the main thread, named `error origin`, and the reporting goroutine is kept as a second thread.
- The reporting goroutine's stack keeps up to 512 frames. 1.0.x kept at most 100.
- `ParseThreadsFromStack` handles elided-frame lines and skips `runtime stack:` sections.

### HTTP middleware (`bthttp`)

`bthttp` is new in 1.1.0. If it replaces your own recovery middleware, expect these defaults:

- Reports carry the request method, the protocol and the registered route pattern. The raw path, host, remote address and user agent are sent only with `Options.SendDefaultPII: true`.
- When a handler panics before it starts the response, the middleware answers `500`. When the response has already started, the middleware panics with `http.ErrAbortHandler`, and the program that sent the request sees a broken connection.
- Handlers receive the middleware's own `http.ResponseWriter`. It implements `http.Flusher` and `http.Hijacker` when the writer underneath, or a writer behind its `Unwrap` method, supports them. It implements `http.Pusher` and `io.ReaderFrom` when the writer underneath implements them itself. It never implements the deprecated `http.CloseNotifier`: use `Request.Context().Done()`.
- A handler that type-asserts the writer to a concrete type should switch to `http.NewResponseController`.

### Tracer (bcd) changes

- `BTTracer`, `bt.New`, `NewOptions` and `PutOptions` exist on every platform. Outside Linux and FreeBSD `BTTracer` is a stub, and `Trace` returns an error wrapping `bt.ErrUnsupportedPlatform`. 1.0.x did not build these types on Windows, and `Trace` with the macOS stub crashed the process.
- Outside Linux and FreeBSD, `ConfigurePut`, `SetOutputPath`, `Put` and `PutDir` return errors wrapping `bt.ErrUnsupportedPlatform`. `EnableTracing` does the same outside Linux. 1.0.x returned nil. Check these errors with `errors.Is(err, bt.ErrUnsupportedPlatform)` where such builds must keep starting.
- `DefaultTraceOptions` returns a copy of the defaults. Changes made through the returned pointer are ignored. Use the new `SetDefaultTraceOptions` to replace the defaults.
- A panic in a `Tracer` method other than `Logf`, or in the HTTP client used for snapshot uploads, becomes an error wrapping `bt.ErrTracerCallback`. A panic in `Logf` is ignored. 1.0.x let these panics through.
- `Trace` returns an error for a nil or typed-nil tracer, and `Register` ignores one. 1.0.x panicked on a nil tracer.
- `Recover` with `repanic` set panics again with the application's panic value, even when the tracer panics or cannot be killed. In 1.0.x that failure replaced the application's panic.
- `GlobalConfig.PanicOnKillFailure` is unchanged: `Trace` panics when a tracer cannot be killed, and so does `Recover` when `repanic` is not set.
- Snapshot uploads time out after 30 seconds (`PutOptions.Timeout`), are capped at 1 GiB (`PutOptions.MaxSnapshotBytes`) and do not follow redirects. 1.0.x had no timeout or size cap by default and followed redirects. Use `PutOptions.HTTPClient` instead of `PutOptions.Client`, which is deprecated but still used when `HTTPClient` is nil.
- `PutDir` keeps going after a failed file and returns all failures joined with `errors.Join`, each naming its file. 1.0.x stopped at the first error. `Put` and `PutDir` refuse symbolic links and non-regular files.
- Signal traces from `bt.Register` mark no thread as faulted, include every thread and use the signal name (`SIGABRT`) as the classifier.
- `bt.Register` with an empty, non-nil signal set logs an error and does nothing. 1.0.x subscribed to every signal.
- `GlobalConfig.ResendSignal` is read when each signal arrives. 1.0.x read it once in `bt.Register`. A later `bt.UpdateConfig` whose `GlobalConfig` does not set `ResendSignal` to true keeps the process running after the signal.
- When neither `TraceOptions.Timeout` nor the tracer's default sets a timeout (both 0), traces time out after 120 seconds. 1.0.x timed out at once in that case.

### Moving from the package-level API to a client

The package-level API keeps working. To move to a client:

| Before | After |
| --- | --- |
| `bt.Options.Endpoint = url` | `client, err := bt.NewClient(bt.Config{Endpoint: url})` |
| `bt.Report(err, attrs)` | `client.Report(err, attrs)` |
| `bt.FinishSendingReports()` | `client.Flush(5 * time.Second)` |
| | `defer client.Close()` |

`bt.SetDefaultClient(client)` makes the package-level functions, and libraries that call them, use that client.
