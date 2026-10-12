//go:build linux || freebsd || darwin

package bt

import (
	"net/http"
	"time"
)

// PutOptions modifies the behavior of snapshot uploads (Tracer.Put and
// friends). The zero value uses the documented defaults.
type PutOptions struct {
	// If set to true, tracer results (i.e. generated snapshot files)
	// will be unlinked from the filesystem after successful puts.
	Unlink bool

	// Deprecated: use HTTPClient. Retained for source compatibility;
	// ignored when HTTPClient is set.
	Client http.Client

	// HTTPClient, when set, is used for snapshot uploads. Requests carry
	// an SDK-owned deadline (Timeout) either way.
	HTTPClient *http.Client

	// Timeout bounds each snapshot upload. Default: 30s.
	Timeout time.Duration

	// MaxSnapshotBytes caps the size of an uploaded snapshot file.
	// Default: 1 GiB.
	MaxSnapshotBytes int64

	// If set to true, tracer results will be uploaded after each
	// successful Trace request.
	OnTrace bool
}
