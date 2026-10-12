package bt

import "fmt"

// SDK version. Follows semantic versioning.
const (
	VersionMajor = 1
	VersionMinor = 1
	VersionPatch = 0
)

// Version is the canonical SDK version string reported with every payload.
var Version = fmt.Sprintf("%d.%d.%d", VersionMajor, VersionMinor, VersionPatch)
