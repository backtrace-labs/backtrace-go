# Security Policy

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it privately on GitHub: open the **Security** tab of this repository and choose **Report a vulnerability**, or go straight to the [advisory form](https://github.com/backtrace-labs/backtrace-go/security/advisories/new). We aim to acknowledge a report within five business days.

Please include:

- a description of the problem and its impact
- the smallest program or steps that reproduce it
- the SDK version and the Go version
- the configuration involved, with credentials removed

## Supported versions

Security fixes go into the latest minor release. Older releases may also get fixes, but we do not promise them.

## What we treat as a vulnerability

- A Backtrace submission token, or an endpoint URL that contains one, appears in an SDK log line or in an error the SDK returns.
- A report or a snapshot is sent to a host other than the configured endpoint. The SDK never follows redirects.
- With `Config.SourceRoots` set, the SDK reads a source file outside those directories.

## What is out of scope

- **The endpoint.** The SDK sends error data to the endpoint the application configures. That endpoint is trusted.
- **What the application adds to a report.** Attributes, breadcrumbs, attachments, environment variables and source text are sent because the application asked for them. The SDK redacts environment values that look like secrets, but this redaction is an extra protection and not a guarantee. Use `Config.EnvVars` to name the variables to send, and `Config.BeforeSend` to remove data before a report leaves the process.
- **Code the application supplies.** A custom `Tracer`, `BeforeSend`, `Logger`, `RequestAttributes`, `ReportStatus` or `http.RoundTripper` is trusted code. The SDK recovers from a panic in the hooks it calls while it reports, but it cannot stop code that never returns.
