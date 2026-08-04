package bt

import (
	"crypto/rand"
	"errors"
	"fmt"
	"runtime"
)

// ReportData is a fully assembled crash/error report, exposed to the
// Config.BeforeSend hook for scrubbing, enrichment, or dropping.
//
// Mutating maps and slices in place is safe: the hook runs on the SDK worker
// goroutine and the report is serialized immediately afterwards.
type ReportData struct {
	// UUID uniquely identifies the report (RFC 4122 version 4).
	UUID string

	// Timestamp is the report time in Unix seconds.
	Timestamp int64

	// Classifiers group the report in the Backtrace UI ("error", "panic",
	// "message", plus error-chain type names).
	Classifiers []string

	// Attributes are indexed key/value pairs used for search and
	// aggregation.
	Attributes map[string]interface{}

	// Annotations carry larger, non-indexed structured data
	// (environment variables, error chains, dependencies, breadcrumbs).
	Annotations map[string]interface{}

	// Threads maps thread IDs to captured goroutine stacks.
	Threads map[string]Thread

	// SourceCode maps source snippet IDs referenced by stack frames.
	SourceCode map[string]SourceCode

	// MainThread is the key in Threads of the faulting goroutine.
	MainThread string

	// Attachments lists file paths uploaded with the report as
	// "attachment_<basename>" multipart parts. Seeded from
	// Config.AttachmentPaths; BeforeSend may add or remove entries.
	Attachments []string
}

// toWire converts the report to the Backtrace JSON submission format.
func (r *ReportData) toWire() map[string]interface{} {
	return map[string]interface{}{
		"uuid":         r.UUID,
		"timestamp":    r.Timestamp,
		"lang":         "go",
		"langVersion":  runtime.Version(),
		"agent":        "backtrace-go",
		"agentVersion": Version,
		"classifiers":  r.Classifiers,
		"attributes":   r.Attributes,
		"annotations":  r.Annotations,
		"threads":      r.Threads,
		"mainThread":   r.MainThread,
		"sourceCode":   r.SourceCode,
	}
}

// uuid4 returns an RFC 4122 version 4 UUID from crypto/rand.
func uuid4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand is documented never to fail on supported
		// platforms; if it somehow does, a constant-free fallback is
		// still preferable to panicking inside a crash reporter.
		for i := range b {
			b[i] = byte(i * 17)
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// errorChainLink describes one error in an unwrapped chain.
type errorChainLink struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// unwrapErrorChain walks err's Unwrap chain (up to maxDepth links) and
// returns the chain description plus the Go type names encountered, for use
// as classifiers. A maxDepth < 0 disables chain capture.
func unwrapErrorChain(err error, maxDepth int) []errorChainLink {
	if err == nil || maxDepth < 0 {
		return nil
	}
	var chain []errorChainLink
	for e := err; e != nil && len(chain) < maxDepth; e = errors.Unwrap(e) {
		chain = append(chain, errorChainLink{
			Type:    fmt.Sprintf("%T", e),
			Message: e.Error(),
		})
	}
	return chain
}
