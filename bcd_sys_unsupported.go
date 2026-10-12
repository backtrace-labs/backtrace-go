//go:build !linux

package bt

import (
	"fmt"
)

func gettid() (int, error) {
	return 0, fmt.Errorf("%w: gettid", ErrUnsupportedPlatform)
}

// Call this function to allow other (non-parent) processes to trace this one.
//
// This is a Linux-specific utility function; on other operating systems it
// returns an error wrapping ErrUnsupportedPlatform so callers can detect
// that tracing permissions were NOT changed.
func EnableTracing() error {
	return fmt.Errorf("%w: process tracing", ErrUnsupportedPlatform)
}
