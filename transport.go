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
	// ownsClient is true when the SDK created the http.Client and the
	// connection pool behind it, so it may close idle connections on
	// shutdown. A caller-supplied client, like a transport shared with the
	// rest of the process, is left untouched.
	ownsClient bool

	mu         sync.Mutex
	pauseUntil time.Time
}

// noRedirect refuses to follow redirects: a report or multipart body must
// never be replayed to a host other than the configured endpoint. A 3xx is
// returned to the caller and handled by the non-2xx rejection branch.
func noRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func newHTTPTransport(client *http.Client, timeout time.Duration) *httpTransport {
	t := &httpTransport{timeout: timeout}
	if client != nil {
		// Copy, so the no-redirect policy is enforced without mutating the
		// caller's shared client.
		cloned := *client
		cloned.CheckRedirect = noRedirect
		t.client = &cloned
		return t
	}

	// The SDK's own client gets its own connection pool, configured like
	// the process default (proxy, TLS, timeouts). A client with a nil
	// Transport would share http.DefaultTransport with the application,
	// and closing idle connections at shutdown would close the
	// application's as well.
	t.client = &http.Client{CheckRedirect: noRedirect}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		t.client.Transport = base.Clone()
		t.ownsClient = true
	}
	// A replaced http.DefaultTransport (instrumentation, a custom
	// RoundTripper) cannot be copied: it is used as it is and, being
	// shared, left alone at shutdown.
	return t
}

func (t *httpTransport) closeIdleConnections() {
	// Only close connections the SDK owns: a shared pool's idle
	// connections may belong to unrelated application traffic.
	if t != nil && t.client != nil && t.ownsClient {
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
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Say why: a redirecting gateway is a configuration to fix, and
		// "rejected" alone does not point at it. The target is not named;
		// it is whatever the answering server chose to send.
		return dropServerReject, fmt.Errorf(
			"bt: server answered %s; redirects are never followed, set the final URL as the endpoint",
			resp.Status)
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
	// The error may come from an application-supplied http.Client, so its
	// Error method is called behind panic containment.
	text := safeErrorString(err)
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
	// Degenerate basenames first: on Windows, Base("/") is `\`, which the
	// character mapping below would otherwise turn into "_".
	if name == "" || name == "." || name == ".." || name == "/" || name == `\` {
		return "attachment"
	}
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
	if name == "" {
		return "attachment"
	}
	if len(name) > maxMultipartNameLength {
		name = name[len(name)-maxMultipartNameLength:]
	}
	return name
}

// buildMultipart assembles the multipart body documented for Backtrace
// submissions: the report JSON in an "upload_file" part, one
// "attachment_<basename>" part per admitted file, then the SDK's own inline
// parts while the budgets allow. Unreadable, non-regular, oversized, or
// over-budget files — and files that fail while being read — are skipped,
// never failing the report itself. Size caps are enforced at read time (a
// file may grow between stat and copy).
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

	var (
		count      int
		totalBytes int64
	)

	// The names of the SDK's own parts (bt-breadcrumbs-0) are reserved
	// first: a file that happens to share one is renamed, not the part the
	// server looks up by name.
	used := map[string]bool{}
	for _, p := range inline {
		used[p.name] = true
	}

	for _, path := range attachments {
		// Diagnostics name the file, not its location: a path can carry
		// a user or tenant name.
		label := safeMultipartName(path)
		if limits.maxAttachments < 0 {
			d.logf("attachment %q skipped: attachments disabled (MaxAttachments < 0)", label)
			continue
		}
		if count >= limits.maxAttachments {
			d.logf("attachment %q skipped: attachment count limit (%d) reached",
				label, limits.maxAttachments)
			continue
		}
		remaining := limits.maxTotalBytes - totalBytes
		if remaining <= 0 {
			d.logf("attachment %q skipped: aggregate attachment budget (%d bytes) exhausted",
				label, limits.maxTotalBytes)
			continue
		}
		perFile := limits.maxAttachmentBytes
		if perFile > remaining {
			perFile = remaining
		}

		file, err := os.Open(path)
		if err != nil {
			// The *PathError text repeats the full path: log its cause.
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				err = pathErr.Err
			}
			d.logf("attachment %q skipped: %v", label, err)
			continue
		}
		// Stat the opened handle (not the path) so a file swapped
		// between stat and open cannot bypass the checks.
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			file.Close()
			d.logf("attachment %q skipped: not a readable regular file", label)
			continue
		}
		if info.Size() > perFile {
			file.Close()
			d.logf("attachment %q skipped: %d bytes exceeds the %d-byte budget",
				label, info.Size(), perFile)
			continue
		}

		// Attachments from different directories may share a basename;
		// uniquify so no part overwrites another.
		name := label
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
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				err = pathErr.Err
			}
			d.logf("attachment %q skipped: read failed: %v", label, err)
			continue
		}
		if copied > perFile {
			buf.Truncate(mark)
			delete(used, name)
			d.logf("attachment %q skipped: grew beyond the %d-byte budget while reading",
				label, perFile)
			continue
		}
		count++
		totalBytes += copied
	}

	// The SDK's own parts share the attachment budgets and come after the
	// files the application asked for: when a budget is tight, the
	// explicit attachment wins. A negative MaxAttachments disables them
	// along with file attachments.
	for _, p := range inline {
		if limits.maxAttachments < 0 {
			d.logf("inline attachment %q skipped: attachments disabled (MaxAttachments < 0)", p.name)
			continue
		}
		size := int64(len(p.data))
		if count >= limits.maxAttachments || totalBytes+size > limits.maxTotalBytes ||
			size > limits.maxAttachmentBytes {
			d.logf("inline attachment %q skipped: attachment budget exhausted", p.name)
			continue
		}
		part, err := w.CreateFormFile("attachment_"+p.name, p.name)
		if err == nil {
			_, err = part.Write(p.data)
		}
		if err != nil {
			return nil, "", err
		}
		count++
		totalBytes += size
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
