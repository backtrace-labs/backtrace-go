package bt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
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

// rateLimitMaxPause caps server-supplied Retry-After values so one
// malformed or hostile response cannot disable reporting for the process
// lifetime.
const rateLimitMaxPause = 5 * time.Minute

// httpTransport delivers serialized reports over HTTP. It is safe for
// concurrent use, applies the configured deadline through an SDK-owned
// request context (independent of any custom http.Client), verifies
// response status, honors 429 Retry-After, drains response bodies so
// connections are reused, and never lets a credential-bearing URL escape
// into an error or log line.
type httpTransport struct {
	client  *http.Client
	timeout time.Duration

	mu         sync.Mutex
	pauseUntil time.Time
}

func newHTTPTransport(client *http.Client, timeout time.Duration) *httpTransport {
	if client == nil {
		client = &http.Client{}
	}
	return &httpTransport{client: client, timeout: timeout}
}

func (t *httpTransport) closeIdleConnections() {
	if t != nil && t.client != nil {
		t.client.CloseIdleConnections()
	}
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

// inlinePart is an in-memory attachment (e.g. the bt-breadcrumbs-0 file).
type inlinePart struct {
	name string
	data []byte
}

// send POSTs body to url under an SDK-owned deadline derived from parent.
// Attachments, when present, switch the request to the documented multipart
// form ("upload_file" part for the report JSON, "attachment_<name>" parts
// for files). It returns the drop reason alongside any error.
func (t *httpTransport) send(
	parent context.Context,
	url string,
	body []byte,
	attachments []string,
	inline []inlinePart,
	limits multipartLimits,
	d diag,
) (dropReason, error) {
	if t.rateLimited() {
		return dropRateLimit, errRateLimited
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, t.timeout)
	defer cancel()

	var (
		reqBody     io.Reader = bytes.NewReader(body)
		contentType           = "application/json"
	)
	if len(attachments) > 0 || len(inline) > 0 {
		multipartBody, multipartType, err := buildMultipart(body, attachments, inline, limits, d)
		if err != nil {
			return dropSerialization, fmt.Errorf("bt: building multipart request: %w", err)
		}
		reqBody, contentType = multipartBody, multipartType
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, reqBody)
	if err != nil {
		return dropSerialization, fmt.Errorf("bt: building request for %s: %s",
			redactURL(url), sanitizeHTTPError(err, url))
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "backtrace-go/"+Version)

	resp, err := t.client.Do(req)
	if err != nil {
		// Never return an error that retains a credential-bearing URL.
		return dropNetwork, fmt.Errorf("bt: sending report to %s: %s",
			redactURL(url), sanitizeHTTPError(err, url))
	}
	defer func() {
		// Drain (bounded) so the keep-alive connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return 0, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		pause := retryAfter(resp)
		t.pause(pause)
		return dropRateLimit, fmt.Errorf("bt: server rate limit (429), pausing submissions for %s", pause)
	default:
		return dropServerReject, fmt.Errorf("bt: server rejected report: %s", resp.Status)
	}
}

// sanitizeHTTPError renders err with every occurrence of the raw URL or its
// embedded credentials replaced. Returns a string (not an error) so callers
// cannot accidentally re-wrap the original.
func sanitizeHTTPError(err error, rawURL string) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if rawURL != "" {
		text = strings.ReplaceAll(text, rawURL, redactURL(rawURL))
	}
	if u, parseErr := neturl.Parse(rawURL); parseErr == nil {
		if token := u.Query().Get("token"); token != "" {
			text = strings.ReplaceAll(text, token, "REDACTED")
		}
		segments := strings.Split(strings.Trim(u.Path, "/"), "/")
		if pathEmbedsToken(u.Hostname(), segments) {
			text = strings.ReplaceAll(text, segments[1], "REDACTED")
		}
	}
	return text
}

// maxMultipartNameLength bounds sanitized attachment part names.
const maxMultipartNameLength = 255

// safeMultipartName reduces an attachment path to a conservative printable
// basename for use in multipart part names and filenames.
func safeMultipartName(path string) string {
	name := filepath.Base(path)
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '\r' || r == '\n' || r == 0 || r == '"' || r == '\\':
			return '_'
		case r < 0x20 || r == 0x7f:
			return '_'
		default:
			return r
		}
	}, name)
	if name == "" || name == "." || name == ".." || name == "/" || name == `\` {
		return "attachment"
	}
	if len(name) > maxMultipartNameLength {
		name = name[len(name)-maxMultipartNameLength:]
	}
	return name
}

// buildMultipart assembles the multipart body documented for Backtrace
// submissions: the report JSON in an "upload_file" part plus one
// "attachment_<basename>" part per admitted attachment. Unreadable,
// non-regular, oversized, or over-budget files — and files that fail while
// being read — are skipped, never failing the report itself. Size caps are
// enforced at read time (a file may grow between stat and copy).
func buildMultipart(body []byte, attachments []string, inline []inlinePart, limits multipartLimits, d diag) (io.Reader, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	reportPart, err := w.CreateFormFile("upload_file", "report.json")
	if err != nil {
		return nil, "", err
	}
	if _, err := reportPart.Write(body); err != nil {
		return nil, "", err
	}

	used := map[string]bool{}
	for _, p := range inline {
		used[p.name] = true
		part, err := w.CreateFormFile("attachment_"+p.name, p.name)
		if err == nil {
			_, err = part.Write(p.data)
		}
		if err != nil {
			return nil, "", err
		}
	}

	var (
		count      int
		totalBytes int64
	)
	for _, path := range attachments {
		if limits.maxAttachments < 0 {
			d.logf("attachment %q skipped: attachments disabled (MaxAttachments < 0)", path)
			continue
		}
		if count >= limits.maxAttachments {
			d.logf("attachment %q skipped: attachment count limit (%d) reached",
				path, limits.maxAttachments)
			continue
		}
		remaining := limits.maxTotalBytes - totalBytes
		if remaining <= 0 {
			d.logf("attachment %q skipped: aggregate attachment budget (%d bytes) exhausted",
				path, limits.maxTotalBytes)
			continue
		}
		perFile := limits.maxAttachmentBytes
		if perFile > remaining {
			perFile = remaining
		}

		file, err := os.Open(path)
		if err != nil {
			d.logf("attachment %q skipped: %v", path, err)
			continue
		}
		// Stat the opened handle (not the path) so a file swapped
		// between stat and open cannot bypass the checks.
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			file.Close()
			d.logf("attachment %q skipped: not a readable regular file", path)
			continue
		}
		if info.Size() > perFile {
			file.Close()
			d.logf("attachment %q skipped: %d bytes exceeds the %d-byte budget",
				path, info.Size(), perFile)
			continue
		}

		// Attachments from different directories may share a basename;
		// uniquify so no part overwrites another.
		name := safeMultipartName(path)
		if used[name] {
			ext := filepath.Ext(name)
			stem := strings.TrimSuffix(name, ext)
			for i := 1; ; i++ {
				candidate := fmt.Sprintf("%s_%d%s", stem, i, ext)
				if !used[candidate] {
					name = candidate
					break
				}
			}
		}
		used[name] = true

		// Remember the buffer position so a failed or over-limit read
		// can be rolled back cleanly (part boundaries are only written
		// by CreateFormFile/Close, so truncating removes the part).
		mark := buf.Len()
		part, err := w.CreateFormFile("attachment_"+name, name)
		var copied int64
		if err == nil {
			copied, err = io.Copy(part, io.LimitReader(file, perFile+1))
		}
		file.Close()
		if err != nil {
			buf.Truncate(mark)
			delete(used, name)
			d.logf("attachment %q skipped: read failed: %v", path, err)
			continue
		}
		if copied > perFile {
			buf.Truncate(mark)
			delete(used, name)
			d.logf("attachment %q skipped: grew beyond the %d-byte budget while reading",
				path, perFile)
			continue
		}
		count++
		totalBytes += copied
	}

	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &buf, w.FormDataContentType(), nil
}

// redactURL hides credentials in diagnostics output: URL userinfo, the
// ?token= query form, and the submit-style {universe}/{token}/{format}
// path form.
func redactURL(u string) string {
	parsed, err := neturl.Parse(u)
	if err != nil {
		return "[REDACTED URL]"
	}
	if parsed.User != nil {
		parsed.User = neturl.User("REDACTED")
	}
	q := parsed.Query()
	if q.Get("token") != "" {
		q.Set("token", "REDACTED")
		parsed.RawQuery = q.Encode()
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if pathEmbedsToken(parsed.Hostname(), segments) {
		segments[1] = "REDACTED"
		parsed.Path = "/" + strings.Join(segments, "/")
	}
	return parsed.String()
}

// submissionFormats are the final path segments of submit-style URLs
// ({universe}/{token}/{format}).
var submissionFormats = map[string]bool{"json": true, "minidump": true, "plcrash": true, "dmp": true}

var hexTokenPattern = regexp.MustCompile(`^[0-9a-fA-F]{32,}$`)

// pathEmbedsToken reports whether segments[1] is a submission token: always
// for submit.backtrace.io, and for self-hosted/aliased gateways when the
// path has the {universe}/{token}[/{format}] shape (known format suffix or
// a hex token). Plain API paths like /api/post never match.
func pathEmbedsToken(host string, segments []string) bool {
	if len(segments) < 2 || len(segments) > 3 || segments[1] == "" {
		return false
	}
	if strings.EqualFold(host, "submit.backtrace.io") {
		return true
	}
	return submissionFormats[strings.ToLower(segments[len(segments)-1])] ||
		hexTokenPattern.MatchString(segments[1])
}

// retryAfter parses a Retry-After header given either as delay seconds or as
// an HTTP date, falling back to rateLimitFallback and capped at
// rateLimitMaxPause.
func retryAfter(resp *http.Response) time.Duration {
	header := resp.Header.Get("Retry-After")
	if header == "" {
		return rateLimitFallback
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
		if seconds > int(rateLimitMaxPause/time.Second) {
			return rateLimitMaxPause
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		d := time.Until(at)
		switch {
		case d <= 0:
			return 0
		case d > rateLimitMaxPause:
			return rateLimitMaxPause
		default:
			return d
		}
	}
	return rateLimitFallback
}
