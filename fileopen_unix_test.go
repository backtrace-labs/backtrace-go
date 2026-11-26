//go:build unix

package bt

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func makeFIFO(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	t.Cleanup(func() {
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
	})
	return path
}

func withinDeadline(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked on a FIFO", what)
	}
}

type regularLookingFiles struct{ info os.FileInfo }

func (r regularLookingFiles) Stat(string) (os.FileInfo, error) { return r.info, nil }

func (regularLookingFiles) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}

func TestOpenRegularFileNeverWaitsOnFIFO(t *testing.T) {
	fifo := makeFIFO(t)
	regular := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(regular, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(regular)
	if err != nil {
		t.Fatal(err)
	}
	withinDeadline(t, "swapped path", func() {
		if _, _, err := openRegularFile(regularLookingFiles{info}, fifo); !errors.Is(err, errNotRegularFile) {
			t.Errorf("FIFO behind a regular stat: err = %v, want errNotRegularFile", err)
		}
	})
}

func TestFIFOAttachmentIsSkipped(t *testing.T) {
	fifo := makeFIFO(t)
	log := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(log, []byte("log line"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.AttachmentPaths = []string{fifo, log}
	})
	c.ReportMessage("fifo attachment", nil)
	flushOne(t, c, rs)
	attachments := rs.lastAttachments()
	if attachments["attachment_app.log"] != "log line" {
		t.Errorf("regular attachment missing: %v", attachments)
	}
	if _, ok := attachments["attachment_fifo"]; ok {
		t.Error("FIFO was attached")
	}
	if s := c.Stats(); s.Delivered != 1 {
		t.Errorf("stats = %+v", s)
	}
}

func TestFIFOSourceFileIsSkipped(t *testing.T) {
	fifo := makeFIFO(t)
	stack := "goroutine 1 [running]:\nmain.a()\n\t" + fifo + ":1 +0x1\n"
	for _, roots := range [][]string{nil, {filepath.Dir(fifo)}} {
		withinDeadline(t, "source read", func() {
			_, sources, _ := buildThreads([]byte(stack), sourceOptions{mode: SourceCodeFile, tabWidth: 8, roots: roots})
			for _, sc := range sources {
				if sc.Text != "" || sc.Path != fifo {
					t.Errorf("roots %v: source entry = %+v", roots, sc)
				}
			}
		})
	}
}
