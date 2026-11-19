//go:build darwin

package bt

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestLimitedWriterTruncatesWithoutFailing pins that output over the cap is
// cut off, not turned into a failed copy: io.Copy reports io.ErrShortWrite
// when a writer returns a short count with a nil error, which is how
// exec.Cmd copies a probe's stdout.
func TestLimitedWriterTruncatesWithoutFailing(t *testing.T) {
	var kept bytes.Buffer
	w := &limitedWriter{w: &kept, remaining: 10}

	// The struct wrapper hides strings.Reader's WriteTo, so io.Copy goes
	// through Write in chunks the way exec's copy does.
	source := struct{ io.Reader }{strings.NewReader(strings.Repeat("abcdefgh", 8))}
	n, err := io.Copy(w, source)
	if err != nil {
		t.Fatalf("copy through the capped writer failed: %v", err)
	}
	if n != 64 {
		t.Errorf("copy reported %d bytes, want all 64 consumed", n)
	}
	if kept.String() != "abcdefghab" {
		t.Errorf("kept %q, want the first 10 bytes", kept.String())
	}

	// Writes after the cap are discarded, still without an error.
	if n, err := w.Write([]byte("more")); n != 4 || err != nil {
		t.Errorf("write past the cap = (%d, %v), want (4, nil)", n, err)
	}
	if kept.Len() != 10 {
		t.Errorf("kept %d bytes after the cap, want 10", kept.Len())
	}
}

func TestParseIOPlatformUUID(t *testing.T) {
	output := `+-o Root  <class IORegistryEntry>
    {
      "IOPlatformSerialNumber" = "C02XXXXXXXXX"
      "IOPlatformUUID" = "01234567-89AB-CDEF-0123-456789ABCDEF"
    }
`
	if got := parseIOPlatformUUID(output); got != "01234567-89AB-CDEF-0123-456789ABCDEF" {
		t.Errorf("parseIOPlatformUUID = %q", got)
	}
	if got := parseIOPlatformUUID("no identifier here"); got != "" {
		t.Errorf("parseIOPlatformUUID without the key = %q, want empty", got)
	}
}
