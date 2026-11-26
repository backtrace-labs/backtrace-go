package bt

import "errors"

// ErrUnsupportedPlatform is returned (wrapped) by operations that have no
// implementation on the current platform, such as tracer snapshot upload on
// macOS or process-tracing setup outside Linux. Test with errors.Is.
var ErrUnsupportedPlatform = errors.New("bt: operation unsupported on this platform")
