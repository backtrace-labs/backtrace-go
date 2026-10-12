//go:build linux || freebsd

package bt

import (
	"os"
	"strings"
)

// firstExistingFileLine returns the first non-empty line of the first
// readable file in paths, capped at 4 KiB.
func firstExistingFileLine(paths ...string) string {
	for _, p := range paths {
		data, err := readSmallFile(p, 4096)
		if err != nil {
			continue
		}
		line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
		if line != "" {
			return line
		}
	}
	return ""
}

// readSmallFile reads a regular file bounded by maxBytes.
func readSmallFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, os.ErrInvalid
	}
	buf := make([]byte, maxBytes)
	n, _ := f.Read(buf)
	return buf[:n], nil
}
