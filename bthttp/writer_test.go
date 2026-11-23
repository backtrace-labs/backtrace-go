package bthttp

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func implements[T any](value any) bool {
	_, ok := value.(T)
	return ok
}

// plainWriter implements only http.ResponseWriter and records what reaches
// it, so a test can assert on what the middleware wrote.
type plainWriter struct {
	header   http.Header
	statuses []int
	body     strings.Builder
}

func (w *plainWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *plainWriter) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *plainWriter) WriteHeader(code int)        { w.statuses = append(w.statuses, code) }

// untouched reports whether nothing was written to the writer.
func (w *plainWriter) untouched() bool { return len(w.statuses) == 0 && w.body.Len() == 0 }

// flusherWriter adds only http.Flusher.
type flusherWriter struct {
	plainWriter
	flushes int
}

func (w *flusherWriter) Flush() { w.flushes++ }

// hijackWriter adds only http.Hijacker.
type hijackWriter struct {
	plainWriter
	hijacked bool
}

func (w *hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	client, server := net.Pipe()
	go func() { _ = server.Close() }()
	return client, bufio.NewReadWriter(bufio.NewReader(client), bufio.NewWriter(client)), nil
}

// fullWriter implements every optional interface.
type fullWriter struct {
	hijackWriter
	flushes int
}

func (w *fullWriter) Flush()                               { w.flushes++ }
func (w *fullWriter) Push(string, *http.PushOptions) error { return nil }
func (w *fullWriter) ReadFrom(r io.Reader) (int64, error)  { return io.Copy(io.Discard, r) }

// unwrapOnly is the shape of a middleware writer written for
// http.ResponseController: it implements no optional interface itself and
// exposes the writer it wraps through Unwrap.
type unwrapOnly struct{ inner http.ResponseWriter }

func (w *unwrapOnly) Header() http.Header         { return w.inner.Header() }
func (w *unwrapOnly) Write(p []byte) (int, error) { return w.inner.Write(p) }
func (w *unwrapOnly) WriteHeader(code int)        { w.inner.WriteHeader(code) }
func (w *unwrapOnly) Unwrap() http.ResponseWriter { return w.inner }

// Capability mix-ins: embedded next to plainWriter they give a writer type
// for each of the sixteen combinations of optional interfaces.
type (
	canFlush    struct{}
	canHijack   struct{}
	canPush     struct{}
	canReadFrom struct{}
)

func (canFlush) Flush() {}
func (canHijack) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("not a real connection")
}
func (canPush) Push(string, *http.PushOptions) error    { return nil }
func (canReadFrom) ReadFrom(r io.Reader) (int64, error) { return io.Copy(io.Discard, r) }

// everyWriterShape returns one writer per combination of Flusher, Hijacker,
// Pusher and ReaderFrom.
func everyWriterShape() []http.ResponseWriter {
	return []http.ResponseWriter{
		&struct{ plainWriter }{},
		&struct {
			plainWriter
			canFlush
		}{},
		&struct {
			plainWriter
			canHijack
		}{},
		&struct {
			plainWriter
			canPush
		}{},
		&struct {
			plainWriter
			canReadFrom
		}{},
		&struct {
			plainWriter
			canFlush
			canHijack
		}{},
		&struct {
			plainWriter
			canFlush
			canPush
		}{},
		&struct {
			plainWriter
			canFlush
			canReadFrom
		}{},
		&struct {
			plainWriter
			canHijack
			canPush
		}{},
		&struct {
			plainWriter
			canHijack
			canReadFrom
		}{},
		&struct {
			plainWriter
			canPush
			canReadFrom
		}{},
		&struct {
			plainWriter
			canFlush
			canHijack
			canPush
		}{},
		&struct {
			plainWriter
			canFlush
			canHijack
			canReadFrom
		}{},
		&struct {
			plainWriter
			canFlush
			canPush
			canReadFrom
		}{},
		&struct {
			plainWriter
			canHijack
			canPush
			canReadFrom
		}{},
		&struct {
			plainWriter
			canFlush
			canHijack
			canPush
			canReadFrom
		}{},
	}
}

// TestWriterParityForEveryShape pins the mapping in wrapWriter for all
// sixteen combinations: handed a writer directly, the wrapper offers the
// same optional interfaces as that writer, no more and no fewer.
func TestWriterParityForEveryShape(t *testing.T) {
	seen := map[[4]bool]bool{}
	for _, w := range everyWriterShape() {
		shape := [4]bool{
			implements[http.Flusher](w), implements[http.Hijacker](w),
			implements[http.Pusher](w), implements[io.ReaderFrom](w),
		}
		seen[shape] = true
		wrapped := wrapWriter(&trackingWriter{ResponseWriter: w})
		got := [4]bool{
			implements[http.Flusher](wrapped), implements[http.Hijacker](wrapped),
			implements[http.Pusher](wrapped), implements[io.ReaderFrom](wrapped),
		}
		if got != shape {
			t.Errorf("writer offering [flush hijack push readFrom] = %v is wrapped as %v", shape, got)
		}
		if implements[interface{ FlushError() error }](wrapped) != shape[0] {
			t.Errorf("FlushError and Flusher disagree for shape %v", shape)
		}
	}
	if len(seen) != 16 {
		t.Fatalf("the test covers %d of the 16 combinations", len(seen))
	}
}

// selfUnwrap is a broken writer whose Unwrap never ends.
type selfUnwrap struct{ plainWriter }

func (w *selfUnwrap) Unwrap() http.ResponseWriter { return w }

// TestWriterInterfaceParity pins the compatibility contract. Flusher and
// Hijacker are offered when the wrapped writer, or a writer behind its
// Unwrap, supports them, which is what http.ResponseController can reach.
// Pusher and ReaderFrom are offered only when the wrapped writer has them
// itself: going around an intermediate writer for those would skip its
// Write.
func TestWriterInterfaceParity(t *testing.T) {
	cases := []struct {
		name                          string
		w                             func() http.ResponseWriter
		flush, hijack, push, readFrom bool
	}{
		{"plain", func() http.ResponseWriter { return &plainWriter{} }, false, false, false, false},
		{"flusher", func() http.ResponseWriter { return &flusherWriter{} }, true, false, false, false},
		{"hijacker", func() http.ResponseWriter { return &hijackWriter{} }, false, true, false, false},
		{"full", func() http.ResponseWriter { return &fullWriter{} }, true, true, true, true},
		{"httptest.Recorder", func() http.ResponseWriter { return httptest.NewRecorder() }, true, false, false, false},
		{"unwrap-only over plain", func() http.ResponseWriter { return &unwrapOnly{&plainWriter{}} }, false, false, false, false},
		{"unwrap-only over full", func() http.ResponseWriter { return &unwrapOnly{&fullWriter{}} }, true, true, false, false},
		{"unwrap-only twice over hijacker", func() http.ResponseWriter { return &unwrapOnly{&unwrapOnly{&hijackWriter{}}} }, false, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wrapped := wrapWriter(&trackingWriter{ResponseWriter: c.w()})

			checks := []struct {
				name      string
				want, got bool
			}{
				{"Flusher", c.flush, implements[http.Flusher](wrapped)},
				{"FlushError", c.flush, implements[interface{ FlushError() error }](wrapped)},
				{"Hijacker", c.hijack, implements[http.Hijacker](wrapped)},
				{"Pusher", c.push, implements[http.Pusher](wrapped)},
				{"ReaderFrom", c.readFrom, implements[io.ReaderFrom](wrapped)},
				{"Unwrap", true, implements[interface{ Unwrap() http.ResponseWriter }](wrapped)},
			}
			for _, check := range checks {
				if check.got != check.want {
					t.Errorf("%s offered = %v, want %v", check.name, check.got, check.want)
				}
			}
			// The deprecated CloseNotifier must never be advertised: a
			// nil channel from a false positive would block handlers
			// forever.
			//nolint:staticcheck // asserting the deprecated interface is absent
			if implements[http.CloseNotifier](wrapped) {
				t.Error("wrapper advertises http.CloseNotifier")
			}

			// The table must agree with what http.ResponseController can
			// do on the bare writer; that is the lookup being mirrored.
			flushErr := http.NewResponseController(c.w()).Flush()
			if got := !errors.Is(flushErr, http.ErrNotSupported); got != c.flush {
				t.Errorf("ResponseController.Flush supported = %v, table says %v", got, c.flush)
			}
			conn, _, hijackErr := http.NewResponseController(c.w()).Hijack()
			if conn != nil {
				_ = conn.Close()
			}
			if got := !errors.Is(hijackErr, http.ErrNotSupported); got != c.hijack {
				t.Errorf("ResponseController.Hijack supported = %v, table says %v", got, c.hijack)
			}
		})
	}
}

// TestWrapWriterSurvivesEndlessUnwrap pins that the capability lookup is
// bounded: a writer whose Unwrap returns itself must not stall a request
// whose handler never flushes or hijacks.
func TestWrapWriterSurvivesEndlessUnwrap(t *testing.T) {
	done := make(chan http.ResponseWriter, 1)
	go func() { done <- wrapWriter(&trackingWriter{ResponseWriter: &selfUnwrap{}}) }()
	select {
	case wrapped := <-done:
		if implements[http.Flusher](wrapped) || implements[http.Hijacker](wrapped) {
			t.Error("capabilities invented for a writer that has none")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wrapWriter never returned")
	}
}

// writerCase is an underlying writer handed to the middleware, either
// directly or behind an Unwrap-only writer.
type writerCase[T any] struct {
	name string
	make func() (http.ResponseWriter, T)
}

// TestFlushStartsResponse pins that a flush counts as a started response
// whichever way the handler reaches it. Untracked, a swallowed panic
// appends a 500 to a response that is already on the wire.
func TestFlushStartsResponse(t *testing.T) {
	writers := []writerCase[*flusherWriter]{
		{"direct", func() (http.ResponseWriter, *flusherWriter) {
			under := &flusherWriter{}
			return under, under
		}},
		{"behind Unwrap", func() (http.ResponseWriter, *flusherWriter) {
			under := &flusherWriter{}
			return &unwrapOnly{under}, under
		}},
	}
	flushers := []struct {
		name  string
		flush func(t *testing.T, w http.ResponseWriter)
	}{
		{"http.Flusher", func(t *testing.T, w http.ResponseWriter) {
			f, ok := w.(http.Flusher)
			if !ok {
				t.Error("http.Flusher not offered")
				return
			}
			f.Flush()
		}},
		{"ResponseController", func(t *testing.T, w http.ResponseWriter) {
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("ResponseController.Flush: %v", err)
			}
		}},
	}
	for _, wc := range writers {
		for _, fc := range flushers {
			t.Run(wc.name+"/"+fc.name, func(t *testing.T) {
				client, reports := newCaptureClient(t)
				outer, under := wc.make()
				h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 2 * time.Second})
				serveExpectingAbort(t, h.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fc.flush(t, w)
					panic("panic after flush")
				})), outer, httptest.NewRequest(http.MethodGet, "/", nil))

				if under.flushes != 1 {
					t.Errorf("the flush reached the writer %d times, want 1", under.flushes)
				}
				if !under.untouched() {
					t.Errorf("written after the response was flushed: statuses %v, body %q",
						under.statuses, under.body.String())
				}
				if reports.count() != 1 {
					t.Errorf("panic reported %d times, want 1", reports.count())
				}
			})
		}
	}
}

// TestUnsupportedFlushLeavesResponseUnstarted is the control for the test
// above: when nothing can flush, asking does not count as a response, and
// the swallowed panic still becomes a 500.
func TestUnsupportedFlushLeavesResponseUnstarted(t *testing.T) {
	client, _ := newCaptureClient(t)
	under := &plainWriter{}
	h := New(Options{Client: client})
	h.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); !errors.Is(err, http.ErrNotSupported) {
			t.Errorf("Flush on a writer that cannot flush: %v", err)
		}
		panic("panic after a refused flush")
	})).ServeHTTP(&unwrapOnly{under}, httptest.NewRequest(http.MethodGet, "/", nil))

	if len(under.statuses) != 1 || under.statuses[0] != http.StatusInternalServerError {
		t.Errorf("statuses = %v, want [500]", under.statuses)
	}
}

// TestNoResponseAfterHijack pins that a swallowed panic writes nothing once
// the handler has taken over the connection, whichever way it did so. A
// 500 written into a hijacked connection corrupts the protocol spoken on
// it.
func TestNoResponseAfterHijack(t *testing.T) {
	writers := []writerCase[*hijackWriter]{
		{"direct", func() (http.ResponseWriter, *hijackWriter) {
			under := &hijackWriter{}
			return under, under
		}},
		{"behind Unwrap", func() (http.ResponseWriter, *hijackWriter) {
			under := &hijackWriter{}
			return &unwrapOnly{under}, under
		}},
	}
	hijackers := []struct {
		name   string
		hijack func(w http.ResponseWriter) (net.Conn, error)
	}{
		{"http.Hijacker", func(w http.ResponseWriter) (net.Conn, error) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				return nil, errors.New("http.Hijacker not offered")
			}
			conn, _, err := hj.Hijack()
			return conn, err
		}},
		{"ResponseController", func(w http.ResponseWriter) (net.Conn, error) {
			conn, _, err := http.NewResponseController(w).Hijack()
			return conn, err
		}},
	}
	for _, wc := range writers {
		for _, hc := range hijackers {
			t.Run(wc.name+"/"+hc.name, func(t *testing.T) {
				client, reports := newCaptureClient(t)
				outer, under := wc.make()
				h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 2 * time.Second})
				h.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := hc.hijack(w)
					if err != nil {
						t.Errorf("hijack: %v", err)
						return
					}
					defer conn.Close()
					panic("panic after hijack")
				})).ServeHTTP(outer, httptest.NewRequest(http.MethodGet, "/", nil))

				if !under.hijacked {
					t.Fatal("handler did not hijack")
				}
				if !under.untouched() {
					t.Errorf("written to a hijacked connection: statuses %v, body %q",
						under.statuses, under.body.String())
				}
				if reports.count() != 1 {
					t.Errorf("panic reported %d times, want 1", reports.count())
				}
			})
		}
	}
}

// TestWriteStringStartsResponse pins that io.WriteString goes through the
// wrapper and counts as a response.
func TestWriteStringStartsResponse(t *testing.T) {
	client, _ := newCaptureClient(t)
	rec := httptest.NewRecorder()
	h := New(Options{Client: client})
	serveExpectingAbort(t, h.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !implements[io.StringWriter](w) {
			t.Error("io.StringWriter not offered")
		}
		_, _ = io.WriteString(w, "partial")
		panic("panic after WriteString")
	})), rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != "partial" {
		t.Errorf("response = %d %q, want the handler's 200 \"partial\" untouched", rec.Code, rec.Body.String())
	}
}
