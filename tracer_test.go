//go:build linux || freebsd

package bt

import (
	"io"
	"log"
	"strings"
	"sync"
	"testing"
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
