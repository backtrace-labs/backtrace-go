package bt

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errRateLimited is returned by httpTransport.send while submissions are
// paused due to a server 429 response.
var errRateLimited = errors.New("bt: rate limited by server, report dropped")

// rateLimitFallback is the pause applied after a 429 without a parsable
// Retry-After header.
const rateLimitFallback = time.Minute

// httpTransport delivers serialized reports over HTTP. It is safe for
// concurrent use, applies the configured timeout, verifies response status,
// honors 429 Retry-After, and drains response bodies so connections are
// reused.
type httpTransport struct {
	client *http.Client

	mu         sync.Mutex
	pauseUntil time.Time
}

func newHTTPTransport(client *http.Client, timeout time.Duration) *httpTransport {
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &httpTransport{client: client}
}

// rateLimited reports whether submissions are currently paused.
func (t *httpTransport) rateLimited() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return time.Now().Before(t.pauseUntil)
}

func (t *httpTransport) pause(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pauseUntil = time.Now().Add(d)
}

// maxAttachmentSize caps individual attachment uploads; larger files are
// skipped with a diagnostic.
const maxAttachmentSize = 10 << 20 // 10 MiB

// send POSTs body to url. Attachments, when present, switch the request to
// the documented multipart form ("upload_file" part for the report JSON,
// "attachment_<name>" parts for files). A non-2xx response is an error; a
// 429 additionally pauses future sends until the server's Retry-After
// deadline.
func (t *httpTransport) send(url string, body []byte, attachments []string, d diag) error {
	if t.rateLimited() {
		return errRateLimited
	}

	var (
		reqBody     io.Reader = bytes.NewReader(body)
		contentType           = "application/json"
	)
	if len(attachments) > 0 {
		multipartBody, multipartType, err := buildMultipart(body, attachments, d)
		if err != nil {
			return fmt.Errorf("bt: building multipart request: %w", err)
		}
		reqBody, contentType = multipartBody, multipartType
	}

	req, err := http.NewRequest(http.MethodPost, url, reqBody)
	if err != nil {
		return fmt.Errorf("bt: building request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "backtrace-go/"+Version)

	resp, err := t.client.Do(req)
	if err != nil {
		// *url.Error embeds the full URL (token included); scrub it
		// before the error reaches any log.
		var ue *neturl.Error
		if errors.As(err, &ue) {
			ue.URL = redactURL(ue.URL)
		}
		return fmt.Errorf("bt: sending report: %w", err)
	}
	defer func() {
		// Drain so the keep-alive connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		d := retryAfter(resp)
		t.pause(d)
		return fmt.Errorf("bt: server rate limit (429), pausing submissions for %s", d)
	default:
		return fmt.Errorf("bt: server rejected report: %s", resp.Status)
	}
}

// buildMultipart assembles the multipart body documented for Backtrace
// submissions: the report JSON in an "upload_file" part plus one
// "attachment_<basename>" part per readable attachment. Unreadable or
// oversized files are skipped, never failing the report itself.
func buildMultipart(body []byte, attachments []string, d diag) (io.Reader, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	reportPart, err := w.CreateFormFile("upload_file", "report.json")
	if err != nil {
		return nil, "", err
	}
	if _, err := reportPart.Write(body); err != nil {
		return nil, "", err
	}

	seen := map[string]int{}
	for _, path := range attachments {
		info, err := os.Stat(path)
		if err != nil {
			d.logf("attachment %q skipped: %v", path, err)
			continue
		}
		if info.Size() > maxAttachmentSize {
			d.logf("attachment %q skipped: %d bytes exceeds the %d byte limit",
				path, info.Size(), maxAttachmentSize)
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			d.logf("attachment %q skipped: %v", path, err)
			continue
		}
		// Attachments from different directories may share a
		// basename; uniquify so no part overwrites another.
		name := filepath.Base(path)
		if n := seen[name]; n > 0 {
			ext := filepath.Ext(name)
			name = fmt.Sprintf("%s_%d%s", strings.TrimSuffix(name, ext), n, ext)
		}
		seen[filepath.Base(path)]++
		part, err := w.CreateFormFile("attachment_"+name, name)
		if err == nil {
			_, err = io.Copy(part, file)
		}
		file.Close()
		if err != nil {
			return nil, "", err
		}
	}

	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &buf, w.FormDataContentType(), nil
}

// redactURL hides the submission token in diagnostics output, both in the
// ?token= query form and in the submit.backtrace.io/{universe}/{token}/{fmt}
// path form.
func redactURL(u string) string {
	parsed, err := neturl.Parse(u)
	if err != nil {
		return u
	}
	q := parsed.Query()
	if q.Get("token") != "" {
		q.Set("token", "REDACTED")
		parsed.RawQuery = q.Encode()
	}
	if strings.EqualFold(parsed.Hostname(), "submit.backtrace.io") {
		segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(segments) >= 3 {
			segments[1] = "REDACTED"
			parsed.Path = "/" + strings.Join(segments, "/")
		}
	}
	return parsed.String()
}

// retryAfter parses a Retry-After header given either as delay seconds or as
// an HTTP date, falling back to rateLimitFallback.
func retryAfter(resp *http.Response) time.Duration {
	header := resp.Header.Get("Retry-After")
	if header == "" {
		return rateLimitFallback
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
		return 0
	}
	return rateLimitFallback
}
