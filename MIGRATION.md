# Migrating from 1.0.x to 1.1.0

The 1.0.x API compiles unchanged: `Report`, `ReportPanic`, `ReportAndRecoverPanic`, `FinishSendingReports`, `Options` and the bcd tracer API. This page lists what behaves differently after the upgrade. New APIs are described in the [README](README.md).

## Requirements

Go 1.25 or later (1.0.x: Go 1.22).

## Package-level functions

- `Report` never blocks. Reports go to a queue of 128 (`Options.QueueSize`). A report that finds it full is dropped and counted in `bt.DefaultClient().Stats()`. 1.0.x blocked the caller once 50 reports were waiting.
- The package-level functions no longer write `report_type` into the caller's attribute map.
- `FinishSendingReports` waits at most 30 seconds, and reporting keeps working afterwards. 1.0.x stopped the reporter for good. `bt.Flush(timeout)` takes a timeout of your choice.
- `ReportPanic` waits at most 5 seconds (`DefaultFlushTimeout`) for the panic report before it panics again. 1.0.x waited without a time limit.
- `DebugBacktrace` never panics and never prints report payloads. It logs one-line summaries to `Options.Logger` (stderr by default). 1.0.x panicked on a missing endpoint or token and on send errors, and printed every report.
- An endpoint that is set but invalid turns reporting off and is logged once to stderr, also without `DebugBacktrace`. An empty endpoint is still a silent no-op.
- These settings were accepted by 1.0.x and now turn reporting off the same way:
  - An endpoint with user info (`https://user:pass@host`). 1.0.x sent it as Basic auth. An `Options.HTTPClient` whose transport sets the `Authorization` header replaces it.
  - A negative `TabWidth` or `ContextLineCount`.
  - A `submit.backtrace.io` endpoint without universe, token and format in its path (`/{universe}/{token}/json`).
  - A `{universe}.sp.backtrace.io` endpoint without a token or a path.
- `BACKTRACE_ENDPOINT` and `BACKTRACE_TOKEN` are used when `Options.Endpoint` and `Options.Token` are empty. A process with `BACKTRACE_ENDPOINT` in its environment reports through the package-level functions.
- `Options.Attributes` starts empty. 1.0.x filled it with the default attributes when the package was imported. The defaults are now added to each report. `bt.SetAttribute("hostname", nil)` removes one. Deleting the key from `Options.Attributes` does not.

## Report contents

- Classifiers name the Go error type, for example `*strconv.NumError`. 1.0.x sent `error`. The type is the innermost one on the error's unwrap path that is not `*errors.errorString`, `*fmt.wrapError`, `*fmt.wrapErrors`, `*errors.joinError`, or one of the `github.com/pkg/errors` types `*errors.fundamental`, `*errors.withStack` and `*errors.withMessage`. When the path holds only those types, the innermost one is used. Errors from `errors.New`, and from `fmt.Errorf` without `%w`, classify as `*errors.errorString`. Panics with a non-error value use `panic` (1.0.x: `message`). Message reports keep `message`. Queries and alerts that filter on classifiers need updating.
- New `error.type` attribute: `Exception` for errors, `Message` for messages, `Unhandled exception` for panics, unless an attribute sets it. `report_type` keeps its 1.0.x values.
- Attribute values are copied when you report them and converted to the types the backend stores when the report is sent. See [Values are normalised when you report](#values-are-normalised-when-you-report).
- A nil attribute value removes the attribute, SDK defaults included.
- `error.message` is capped at 1024 bytes. A longer message is sent in full in the `Message` annotation.
- Errors add an `Error Chain` annotation with the type and message of each error in the unwrap graph. Reports add a `Dependencies` annotation with the module dependencies of the build.
- A report over 8 MiB (`Config.MaxReportBytes`) is trimmed step by step and gets a `backtrace.truncated` attribute. A report that still does not fit is dropped.
- The `guid` attribute (machine ID) is opt-in with `Options.SendMachineID`. 1.0.x always sent it, and fell back to the hostname on Linux.
- `cpu.brand`, `uname.version` and `guid` are read from files, syscalls and the registry at the first report. 1.0.x ran shell pipelines, `lscpu` and `wmic` at import time.
- More default attributes: `go.version`, `cpu.count`, `process.age`, the goroutine count, heap and GC statistics, `runtime.gogc`, `runtime.gomemlimit`, and `application.version` and `vcs.*` from the Go build info.
- `SendEnvVars` redacts the values of variables whose names contain `TOKEN`, `SECRET`, `PASS`, `KEY` and similar words. A value holding a URL with a password is replaced whole. Secret query values in other URL values are replaced. 1.0.x sent every value as is, cut at its first `=`.
- `SendEnvVars` sends at most 1024 variables and 256 KiB of names and values (`Config.MaxEnvVars`, `Config.MaxEnvBytes`). `Options.EnvVars` sends only the variables it names, with the same redaction. `SendEnvVars` is ignored when `Options.EnvVars` is set.
- Request bodies are gzip-compressed. A proxy in front of Backtrace that cannot handle `Content-Encoding: gzip` needs `Options.DisableCompression`.

## Values are normalised when you report

1.0.x sent attribute values unchanged and kept references to the caller's maps. The backend dropped the values it cannot store.

`Report*` calls, `SetAttribute(s)`, `AddBreadcrumb`, the scope methods and `NewClient` (for `Config.Attributes`) now copy the values you pass before they return. Your maps and slices can change afterwards. The copy holds only `nil`, `bool`, `string`, `int64`, `uint64`, `float64`, `[]any` and `map[string]any`, and `BeforeSend` sees these types. The conversion follows `encoding/json` v1, the implementation in Go 1.26 and earlier:

| Value passed | Value kept |
| --- | --- |
| `int`, `int8`, `int16`, `int32`, `int64` | `int64` |
| `uint`, `uint8`, `uint16`, `uint32`, `uint64`, `uintptr` | `uint64` |
| `float32` | `float64` with the shortest decimal form (`0.1` stays `0.1`) |
| `[]byte` | standard base64 text |
| other slices and arrays | `[]any` |
| maps | `map[string]any`. String keys stay as they are, `encoding.TextMarshaler` keys use `MarshalText`, integer keys are written in decimal, other keys are formatted with `fmt` |
| structs | `map[string]any` with the fields and names `encoding/json` v1 writes, `json` tag options and embedded structs included |
| pointers | the value they point to. A nil pointer becomes `nil` |
| `time.Time` | RFC 3339 text with nanoseconds |
| `json.Marshaler` | its JSON, decoded into the types above |
| `encoding.TextMarshaler` | its text |
| `error` | its message |
| `NaN` and infinities | a marker such as `[bt: non-finite float NaN]` |
| funcs, channels, complex numbers | a marker such as `[bt: unsupported value of type complex128]` |
| a value that contains itself | a `[bt: cycle]` marker at the repeat |

`MarshalJSON` is used before `MarshalText`, and both before `Error`, with the pointer-receiver and interface rules of `encoding/json` v1. A `MarshalJSON` or `MarshalText` method that fails or panics leaves a `[bt: MarshalJSON failed for T]` or `[bt: MarshalText failed for T]` marker. These methods run on the caller's goroutine during the call.

Each call has limits: 32 levels of nesting, 2048 values, 64 KiB per string, and `MaxReportBytes` in total (8 MiB by default). Scope methods always get 8 MiB in total. A breadcrumb is limited to 4 KiB of JSON. Its attributes are dropped when they do not fit even without the message. `Config.Attributes` get 1 MiB in total unless `MaxReportBytes` is set. A longer string is cut and ends in `…[truncated]`. A map or slice over the limit is cut, and a `[bt: truncated]` key or a `[bt: 12 of 40 elements omitted]` element marks the cut. Deeper nesting becomes `[bt: value truncated: max depth]`.

When the report is sent, after `BeforeSend`, attribute values are converted to the types the backend stores: strings, booleans and non-negative integers. Negative numbers, fractional floats and integers above the `int64` range become strings. Whole non-negative floats below 2^53 become integers, and larger ones become strings. Maps and slices move to the annotations under the same key, unless an annotation of that name exists.

Without `SetDefaultClient`, `bt.SetAttribute` stores the normalised copy in `Options.Attributes`. Reading it back gives an `int64` for an `int`.

## Source code

- Reports carry file paths and line numbers only. 1.0.x read every source file on the stack and sent its whole text. `Options.SourceCode = bt.SourceCodeFile` restores that within size limits, `bt.SourceCodeContext` sends the lines around each frame, and `Options.SourceRoots` limits the files that are read.
- `ParseThreadsFromStack` follows `Options.SourceCode` too.

## Stacks and grouping

Backtrace groups reports by the frames of the main thread. These changes regroup some reports after the upgrade:

- Panic reports mark the frame that called `panic` as faulted. The backend groups them from that frame and leaves out the deferred recovery code.
- An error that carries its own stack (a `StackTrace()` method as in `github.com/pkg/errors`, or `Callers() []uintptr`) is grouped by where it was created. That stack becomes the main thread, named `error origin`, and the reporting goroutine is kept as a second thread.
- The reporting goroutine's stack keeps up to 512 frames. 1.0.x kept at most 100.
- `ParseThreadsFromStack` handles elided-frame lines and skips `runtime stack:` sections.

## Delivery

- Each submission attempt times out after 30 seconds (`Options.Timeout`). 1.0.x used `http.Post` without a timeout.
- Redirects are not followed, also with `Options.HTTPClient`. A 3xx response counts as a rejected report (`ServerReject`) and is logged once. 1.0.x followed redirects. A 301, 302 or 303 lost the report.
- Without `Options.HTTPClient` the SDK uses its own clone of `http.DefaultTransport` instead of `http.DefaultClient`.
- Network errors before the request body was sent, and 408, 500, 502, 503 and 504 responses, are retried twice with backoff.
- A 429 response pauses submissions for its `Retry-After` (one minute without one, at most 5 minutes). 5 network failures in a row pause them for 30 seconds. Reports are dropped and counted while submissions are paused.

## bcd tracer

- `BTTracer`, `bt.New`, `NewOptions` and `PutOptions` exist on every platform. Outside Linux and FreeBSD `BTTracer` is a stub, and `Trace` returns an error wrapping `bt.ErrUnsupportedPlatform`. 1.0.x did not build these types on Windows, and `Trace` with the macOS stub crashed the process.
- Outside Linux and FreeBSD, `ConfigurePut`, `SetOutputPath`, `Put` and `PutDir` return errors wrapping `bt.ErrUnsupportedPlatform`. The 1.0.x macOS stub returned nil. Check them with `errors.Is` where macOS builds must keep starting.
- `DefaultTraceOptions` returns a copy of the defaults. Changes made through the returned pointer are ignored. 1.0.x returned the tracer's own defaults. `SetDefaultTraceOptions` replaces them.
- `EnableTracing` returns an error wrapping `bt.ErrUnsupportedPlatform` outside Linux. 1.0.x returned nil.
- Signal traces from `bt.Register` mark no thread as faulted, include every thread and use the signal name (`SIGABRT`) as classifier instead of `*bt.signalError`.
- `bt.Register` with an empty, non-nil signal set logs an error and does nothing. 1.0.x subscribed to every signal.
- `GlobalConfig.ResendSignal` is read when each signal arrives. 1.0.x read it once in `bt.Register`. A later `bt.UpdateConfig` whose `GlobalConfig` does not set `ResendSignal` to true keeps the process running after the signal.
- When neither `TraceOptions.Timeout` nor the tracer's default sets a timeout (both 0), traces time out after 120 seconds. A per-call 0 with a negative tracer default means no timeout. 1.0.x timed out at once in both cases.
- A panic in a `Tracer` method other than `Logf`, or in the HTTP client used for snapshot uploads, becomes an error wrapping `bt.ErrTracerCallback`. A panicking `Logf` is ignored. 1.0.x let these panics through.
- `Trace` returns an error for a nil or typed-nil tracer, and `Register` ignores one. 1.0.x panicked on a nil tracer.
- `Recover` with `repanic` set re-panics with the application's panic, also when the tracer panics or cannot be killed. In 1.0.x that failure replaced the application's panic.
- `PutDir` keeps walking after a failed file and returns all failures joined with `errors.Join`, each naming its file. 1.0.x stopped at the first error. `Put` and `PutDir` refuse symbolic links and non-regular files.
- Snapshot uploads time out after 30 seconds (`PutOptions.Timeout`), are capped at 1 GiB (`PutOptions.MaxSnapshotBytes`) and do not follow redirects. 1.0.x had no timeout or size cap by default and followed redirects. `PutOptions.Client` is deprecated in favor of `PutOptions.HTTPClient`.
