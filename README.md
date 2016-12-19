# backtrace-go

[Backtrace](http://backtrace.io/) error reporting tool for Go.

## Installation

```
go get github.com/backtrace-labs/backtrace-go
```

## Usage

In Go there are three ways errors can happen:

 * An operation produces an `error` return value.
 * A goroutine calls `panic`.
 * A native library crashes or the Go runtime itself crashes.

backtrace-go handles `error` and `panic` situations. However, there are some
caveats with handling panics:

 * In order to capture error reports in a panic scenario, every goroutine must
   make an API call to set up panic handling.
 * It's possible to forget to do this setup, and you might not know when a
   callback is executed as a goroutine.
 * If a Go application makes any calls into native libraries, a crash in a
   native library will crash without causing a panic.

Fortunately, there is a robust solution which can capture an error report
in all of these circumstances. This is a Backtrace product called
[Coresnap](https://documentation.backtrace.io/coresnapintro/) which supports
deep introspection into the state of Go applications.

The recommended way to capture error reports in a Go application is to use
coresnap to handle panics and crashes, and to use backtrace-go to report
non-fatal error conditions.

```
import "bt"
import "http"

func init() {
    bt.Options.Endpoint = "https://console.backtrace.io"
    bt.Options.Token = "51cc8e69c5b62fa8c72dc963e730f1e8eacbd243aeafc35d08d05ded9a024121"
}

func foo() {
	response, err := http.Get("https://doesnotexistexample.com")
    if err != nil {
        bt.Report(err, nil)
    }
}
```

## Documentation

### bt.Report(msg interface{}, attributes map[string]string)

msg can be an `error` or something that can be converted to a `string`.
`attributes` are added to the report.

### bt.ReportPanic(attributes map[string]string)

Sends an error report in the event of a panic.

```go
defer bt.ReportPanic(nil)
somethingThatMightPanic()
```

### bt.ReportAndRecoverPanic(attributes map[string]string)

This is the same as `bt.ReportPanic` but it recovers from the
panic and the goroutine lives on.

### bt.FinishSendingReports()

backtrace-go sends reports in a goroutine to avoid blocking.
When your application shuts down it will abort any ongoing sending of
reports. Call this function to block until all queued reports are done
sending.
