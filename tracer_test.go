//go:build linux || freebsd

package bt

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfigurePutURLForms(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		token    string
		want     string // "" means an error is expected
	}{
		{"host only", "yourcompany.sp.backtrace.io", "tok",
			"https://yourcompany.sp.backtrace.io:6098/post?token=tok"},
		{"scheme and port kept", "http://host.example.com:1234", "tok",
			"http://host.example.com:1234/post?token=tok"},
		{"ipv6 gets default port", "https://[::1]", "tok",
			"https://[::1]:6098/post?token=tok"},
		{"token escaped", "https://host.example.com:6098", "a&b #c",
			"https://host.example.com:6098/post?token=a%26b+%23c"},
		{"empty endpoint", "", "tok", ""},
		{"empty token", "https://host.example.com", "", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := New(NewOptions{})
			err := tr.ConfigurePut(c.endpoint, c.token, PutOptions{})
			if c.want == "" {
				if err == nil {
					t.Fatalf("expected error, got endpoint %q", tr.put.endpoint)
				}
				if c.endpoint != "" && !strings.Contains(err.Error(), "token") {
					t.Errorf("empty-token error misleading: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConfigurePut: %v", err)
			}
			if tr.put.endpoint != c.want {
				t.Errorf("endpoint = %q, want %q", tr.put.endpoint, c.want)
			}
		})
	}
}

func TestBuildPutURLRejections(t *testing.T) {
	rejected := []struct{ name, endpoint string }{
		{"userinfo", "https://user:pw@host"},
		{"fragment", "https://host/x#frag"},
		{"non-http scheme", "ftp://host"},
		{"opaque", "mailto:x@y"},
		{"out-of-range port", "https://host:99999"},
		{"non-numeric port", "https://host:abc"},
	}
	for _, c := range rejected {
		t.Run(c.name, func(t *testing.T) {
			if _, err := buildPutURL(c.endpoint, "tok"); err == nil {
				t.Errorf("buildPutURL(%q) accepted", c.endpoint)
			}
		})
	}

	// The parse-failure error must not leak the raw endpoint verbatim.
	if _, err := buildPutURL("https://host/x\x7f?token=SECRETLEAK", "tok"); err == nil {
		t.Error("control-character endpoint accepted")
	} else if strings.Contains(err.Error(), "SECRETLEAK") {
		t.Errorf("raw endpoint leaked through parse error: %v", err)
	}
}

func TestConfigurePutOptionValidation(t *testing.T) {
	tr := New(NewOptions{})
	if err := tr.ConfigurePut("https://host", "tok", PutOptions{Timeout: -time.Second}); err == nil {
		t.Error("negative upload timeout accepted")
	}
	if err := tr.ConfigurePut("https://host", "tok", PutOptions{MaxSnapshotBytes: -1}); err == nil {
		t.Error("negative snapshot size limit accepted")
	}
}

// TestDefaultTraceOptionsCopySemantics pins the race fix: the returned
// pointer is a copy, and SetDefaultTraceOptions is the mutation path.
func TestDefaultTraceOptionsCopySemantics(t *testing.T) {
	tr := New(NewOptions{})
	opts := tr.DefaultTraceOptions()
	opts.Timeout = time.Nanosecond // must NOT affect the tracer's defaults

	if got := tr.DefaultTraceOptions().Timeout; got != 120*time.Second {
		t.Errorf("defaults mutated through returned pointer: Timeout = %v", got)
	}

	tr.SetDefaultTraceOptions(TraceOptions{Timeout: 7 * time.Second})
	if got := tr.DefaultTraceOptions().Timeout; got != 7*time.Second {
		t.Errorf("SetDefaultTraceOptions not applied: Timeout = %v", got)
	}
}

func TestPutSnapshotFileHardening(t *testing.T) {
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "snap.btt")
	if err := os.WriteFile(snapshot, []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("unconfigured", func(t *testing.T) {
		tr := New(NewOptions{})
		if err := tr.putSnapshotFile(snapshot); err == nil ||
			!strings.Contains(err.Error(), "not configured") {
			t.Errorf("err = %v, want not-configured error", err)
		}
	})

	t.Run("non-regular file rejected", func(t *testing.T) {
		// A device file opens instantly (unlike a FIFO, whose blocking
		// read-end open would hang the test) and is not regular.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("non-regular file reached the server")
		}))
		defer srv.Close()
		tr := New(NewOptions{})
		if err := tr.ConfigurePut(srv.URL, "tok", PutOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := tr.putSnapshotFile(os.DevNull); err == nil ||
			!strings.Contains(err.Error(), "regular file") {
			t.Errorf("err = %v, want regular-file rejection", err)
		}
	})

	t.Run("size cap enforced", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("oversized snapshot reached the server")
		}))
		defer srv.Close()
		tr := New(NewOptions{})
		if err := tr.ConfigurePut(srv.URL, "tok", PutOptions{MaxSnapshotBytes: 4}); err != nil {
			t.Fatal(err)
		}
		if err := tr.putSnapshotFile(snapshot); err == nil ||
			!strings.Contains(err.Error(), "limit") {
			t.Errorf("err = %v, want size-limit rejection", err)
		}
	})

	t.Run("success with bounded body and token-safe endpoint", func(t *testing.T) {
		var gotLen int64
		var gotToken string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			gotLen = int64(len(body))
			gotToken = r.URL.Query().Get("token")
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		tr := New(NewOptions{})
		if err := tr.ConfigurePut(srv.URL, "tok", PutOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := tr.putSnapshotFile(snapshot); err != nil {
			t.Fatalf("putSnapshotFile: %v", err)
		}
		if gotLen != int64(len("snapshot-bytes")) || gotToken != "tok" {
			t.Errorf("upload = %d bytes, token %q", gotLen, gotToken)
		}
	})
}

// TestTracerLoggerConcurrency locks in the fix for the recursive-RLock
// deadlock: Finalize/Logf/String must be callable concurrently with
// SetLogger/SetLogLevel. Run with -race; a regression deadlocks or races.
func TestTracerLoggerConcurrency(t *testing.T) {
	tr := New(NewOptions{})

	quiet := &defaultLogger{logger: log.New(io.Discard, "", 0), level: LogError}

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				tr.SetLogLevel(LogMax)
				tr.SetLogger(quiet)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				_ = tr.Finalize([]string{"--noop"})
				tr.Logf(LogDebug, "tracer: %s\n", tr)
				_ = tr.String()
				tr.SetTracerPath("/opt/backtrace/bin/ptrace")
			}
		}()
	}
	wg.Wait()
}
