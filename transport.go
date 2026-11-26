package bt

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptrace"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// errRateLimited is returned by httpTransport.send while submissions are
// paused due to a server 429 response.
var errRateLimited = errors.New("bt: rate limited by server, report dropped")

var errCircuitOpen = errors.New("bt: submissions paused after consecutive network failures, report dropped")

// rateLimitFallback is the pause applied after a 429 without a parsable
// Retry-After header.
const rateLimitFallback = time.Minute

// rateLimitMaxPause caps server-supplied Retry-After values so one
// malformed or hostile response cannot disable reporting for the process
// lifetime.
const rateLimitMaxPause = 5 * time.Minute

const (
	retryBackoffBase = time.Second
	retryBackoffCap  = 30 * time.Second
	maxRetryWait     = 60 * time.Second

	circuitThreshold = 5
	circuitPause     = 30 * time.Second

	expectContinueBytes = 256 << 10
)

// httpTransport delivers serialized reports over HTTP. It is safe for
// concurrent use, applies the configured deadline through an SDK-owned
// request context (independent of any custom http.Client), verifies
// response status, honors 429 Retry-After, drains response bodies so
// connections are reused, and never lets a credential-bearing URL escape
// into an error or log line. It also retries transient failures and pauses
// submissions after repeated network failures.
type httpTransport struct {
	client  *http.Client
	timeout time.Duration
	backoff func(retry int) time.Duration
	// ownsClient is true when the SDK created the http.Client and the
	// connection pool behind it, so it may close idle connections on
	// shutdown. A caller-supplied client, like a transport shared with the
	// rest of the process, is left untouched.
	ownsClient bool

	mu          sync.Mutex
	pauseUntil  time.Time
	pauseReason dropReason
	pauseErr    error
	failures    int

	warnedPermanent atomic.Bool
}

// noRedirect refuses to follow redirects: a report or multipart body must
// never be replayed to a host other than the configured endpoint. A 3xx is
// returned to the caller and handled by the non-2xx rejection branch.
func noRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func newHTTPTransport(client *http.Client, timeout time.Duration) *httpTransport {
	t := &httpTransport{timeout: timeout, backoff: fullJitter}
	if client != nil {
		// Copy, so the no-redirect policy is enforced without mutating the
		// caller's shared client.
		cloned := *client
		cloned.CheckRedirect = noRedirect
		t.client = &cloned
		return t
	}

	// The SDK's own client gets its own connection pool, configured like
	// the process default (proxy, TLS). A client with a nil
	// Transport would share http.DefaultTransport with the application,
	// and closing idle connections at shutdown would close the
	// application's as well.
	t.client = &http.Client{CheckRedirect: noRedirect}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		t.client.Transport = sdkTransport(base)
		t.ownsClient = true
	}
	// A replaced http.DefaultTransport (instrumentation, a custom
	// RoundTripper) cannot be copied: it is used as it is and, being
	// shared, left alone at shutdown.
	return t
}

func sdkTransport(base *http.Transport) *http.Transport {
	tr := base.Clone()
	dial := tr.DialContext
	if dial == nil {
		dial = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return dial(ctx, network, addr)
	}
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ResponseHeaderTimeout = 20 * time.Second
	return tr
}

func (t *httpTransport) closeIdleConnections() {
	// Only close connections the SDK owns: a shared pool's idle
	// connections may belong to unrelated application traffic.
	if t != nil && t.client != nil && t.ownsClient {
		t.client.CloseIdleConnections()
	}
}

func (t *httpTransport) pauseState() (dropReason, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Now().Before(t.pauseUntil) {
		return t.pauseReason, t.pauseErr
	}
	return 0, nil
}

func (t *httpTransport) pause(d time.Duration, reason dropReason, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pauseUntil = time.Now().Add(d)
	t.pauseReason = reason
	t.pauseErr = err
}

func (t *httpTransport) recordNetworkFailure() {
	t.mu.Lock()
	t.failures++
	open := t.failures >= circuitThreshold
	if open {
		t.failures = 0
	}
	t.mu.Unlock()
	if open {
		t.pause(circuitPause, dropNetwork, errCircuitOpen)
	}
}

func (t *httpTransport) recordResponse() {
	t.mu.Lock()
	t.failures = 0
	t.mu.Unlock()
}

func fullJitter(retry int) time.Duration {
	ceiling := retryBackoffBase
	for i := 0; i < retry && ceiling < retryBackoffCap; i++ {
		ceiling *= 2
	}
	return rand.N(min(ceiling, retryBackoffCap))
}

// inlinePart is an in-memory attachment (e.g. the bt-breadcrumbs-0 file).
type inlinePart struct {
	name string
	data []byte
}

type payload struct {
	body        []byte
	contentType string
	gzipped     bool
}

func buildPayload(body []byte, attachments []string, inline []inlinePart, limits multipartLimits, compress bool, warnedInline *atomic.Bool, d diag) (payload, error) {
	p := payload{body: body, contentType: "application/json"}
	if len(attachments) > 0 || len(inline) > 0 {
		data, contentType, err := buildMultipart(body, attachments, inline, limits, warnedInline, d)
		if err != nil {
			return payload{}, fmt.Errorf("bt: building multipart request: %w", err)
		}
		p.body, p.contentType = data, contentType
	}
	if compress {
		if zipped, err := gzipBytes(p.body); err == nil {
			p.body, p.gzipped = zipped, true
		}
	}
	return p, nil
}

func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type sendResult struct {
	reason  dropReason
	err     error
	retries int
}

type attemptResult struct {
	reason    dropReason
	err       error
	retry     bool
	permanent bool
}

// send POSTs p.body to url under an SDK-owned deadline derived from parent.
func (t *httpTransport) send(parent context.Context, url string, p payload, maxRetries int, wait func(time.Duration) bool, d diag) sendResult {
	if parent == nil {
		parent = context.Background()
	}
	var (
		retries int
		waited  time.Duration
	)
	if r, stop := t.blocked(parent, retries); stop {
		return r
	}
	for {
		res := t.attempt(parent, url, p)
		if res.err == nil {
			return sendResult{retries: retries}
		}
		if res.permanent && t.warnedPermanent.CompareAndSwap(false, true) {
			d.warnf("%v (destination %s); not retried. Later rejections are logged only with Debug.",
				res.err, urlOrigin(url))
		}
		if !res.retry || retries >= maxRetries || waited >= maxRetryWait {
			return sendResult{reason: res.reason, err: res.err, retries: retries}
		}
		if r, stop := t.blocked(parent, retries); stop {
			return r
		}
		delay := min(t.backoff(retries), maxRetryWait-waited)
		d.logf("submission attempt %d failed: %v; retrying in %s", retries+1, res.err, delay)
		if !wait(delay) {
			return sendResult{reason: dropClosed, err: errors.New("bt: client closed while waiting to retry"), retries: retries}
		}
		waited += delay
		if r, stop := t.blocked(parent, retries); stop {
			return r
		}
		retries++
	}
}

func (t *httpTransport) blocked(parent context.Context, retries int) (sendResult, bool) {
	if reason, err := t.pauseState(); err != nil {
		return sendResult{reason: reason, err: err, retries: retries}, true
	}
	if parent.Err() != nil {
		return sendResult{reason: dropClosed, err: errors.New("bt: client closed before the report was sent"), retries: retries}, true
	}
	return sendResult{}, false
}

func (t *httpTransport) attempt(parent context.Context, url string, p payload) attemptResult {
	ctx, cancel := context.WithTimeout(parent, t.timeout)
	defer cancel()
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(true)
			}
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(p.body))
	if err != nil {
		return attemptResult{reason: dropSerialization, err: fmt.Errorf("bt: building request for %s: %s",
			urlOrigin(url), sanitizeHTTPError(err, url))}
	}
	req.Header.Set("Content-Type", p.contentType)
	req.Header.Set("User-Agent", "backtrace-go/"+Version)
	if p.gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if len(p.body) >= expectContinueBytes {
		req.Header.Set("Expect", "100-continue")
	}

	resp, err := t.client.Do(req)
	if err != nil {
		// Never return an error that retains a credential-bearing URL.
		err = fmt.Errorf("bt: sending report to %s: %s", urlOrigin(url), sanitizeHTTPError(err, url))
		if parent.Err() != nil {
			return attemptResult{reason: dropClosed, err: err}
		}
		t.recordNetworkFailure()
		return attemptResult{reason: dropNetwork, err: err, retry: !wrote.Load()}
	}
	t.recordResponse()
	defer func() {
		// Drain (bounded) so the keep-alive connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	res := attemptResult{reason: dropServerReject}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return attemptResult{}
	case resp.StatusCode == http.StatusTooManyRequests:
		pause := retryAfter(resp)
		t.pause(pause, dropRateLimit, errRateLimited)
		res.reason = dropRateLimit
		res.err = fmt.Errorf("bt: server rate limit (429), pausing submissions for %s", pause)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Say why: a redirecting gateway is a configuration to fix, and
		// "rejected" alone does not point at it. The target is not named;
		// it is whatever the answering server chose to send.
		res.err = fmt.Errorf(
			"bt: server answered %s; redirects are never followed, set the final URL as the endpoint",
			resp.Status)
		res.permanent = true
	default:
		res.err = fmt.Errorf("bt: server rejected report: %s", resp.Status)
		res.retry = retryableStatus[resp.StatusCode]
		res.permanent = permanentStatus[resp.StatusCode]
	}
	return res
}

var retryableStatus = map[int]bool{
	http.StatusRequestTimeout:      true,
	http.StatusInternalServerError: true,
	http.StatusBadGateway:          true,
	http.StatusServiceUnavailable:  true,
	http.StatusGatewayTimeout:      true,
}

var permanentStatus = map[int]bool{
	http.StatusBadRequest:            true,
	http.StatusUnauthorized:          true,
	http.StatusForbidden:             true,
	http.StatusNotFound:              true,
	http.StatusRequestEntityTooLarge: true,
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
	if rawURL == "" {
		return text
	}
	origin := urlOrigin(rawURL)
	text = strings.ReplaceAll(text, rawURL, origin)
	if u, parseErr := neturl.Parse(rawURL); parseErr == nil {
		text = strings.ReplaceAll(text, u.String(), origin)
	}
	for _, secret := range submissionSecrets(rawURL, "") {
		text = strings.ReplaceAll(text, secret, "REDACTED")
	}
	return text
}

// maxMultipartNameLength bounds sanitized attachment part names.
const maxMultipartNameLength = 220

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
		cut := len(name) - maxMultipartNameLength
		for cut < len(name) && !utf8.RuneStart(name[cut]) {
			cut++
		}
		name = name[cut:]
	}
	return name
}

func uniquePartName(name string, used map[string]bool) string {
	if !used[name] {
		return name
	}
	ext := filepath.Ext(name)
	if len(ext) > 16 {
		ext = ""
	}
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		suffix := "_" + strconv.Itoa(i) + ext
		candidate := truncateUTF8(stem, maxMultipartNameLength-len(suffix)) + suffix
		if !used[candidate] {
			return candidate
		}
	}
}

// buildMultipart assembles the multipart body documented for Backtrace
// submissions: the report JSON in an "upload_file" part, one
// "attachment_<basename>" part per admitted file, then the SDK's own inline
// parts while the budgets allow. Unreadable, non-regular, oversized, or
// over-budget files — and files that fail while being read — are skipped,
// never failing the report itself. Size caps are enforced at read time (a
// file may grow between stat and copy).
func buildMultipart(body []byte, attachments []string, inline []inlinePart, limits multipartLimits, warnedInline *atomic.Bool, d diag) ([]byte, string, error) {
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

		file, info, err := openRegularFile(hostFiles{}, path)
		if err != nil {
			// The *PathError text repeats the full path: log its cause.
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				err = pathErr.Err
			}
			d.logf("attachment %q skipped: %v", label, err)
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
		name := uniquePartName(label, used)
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
			if warnedInline.CompareAndSwap(false, true) {
				d.warnf("inline attachment %q skipped: attachment budget exhausted. Later skips are logged only with Debug.", p.name)
			} else {
				d.logf("inline attachment %q skipped: attachment budget exhausted", p.name)
			}
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
	return buf.Bytes(), w.FormDataContentType(), nil
}

// Paths, queries and userinfo can carry a submission token.
func urlOrigin(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "[invalid URL]"
	}
	return u.Scheme + "://" + u.Host
}

// Token segments count at any depth: an endpoint can sit behind a path prefix.
func submissionSecrets(rawURL, token string) []string {
	secrets := []string{token}
	if u, err := neturl.Parse(rawURL); err == nil {
		for _, pair := range strings.Split(u.RawQuery, "&") {
			key, value, _ := strings.Cut(pair, "=")
			if name, err := neturl.QueryUnescape(key); err == nil && strings.EqualFold(name, "token") {
				secrets = append(secrets, value)
				if unescaped, err := neturl.QueryUnescape(value); err == nil {
					secrets = append(secrets, unescaped)
				}
			}
		}
		segments := strings.Split(strings.Trim(u.Path, "/"), "/")
		n := len(segments)
		for i, segment := range segments {
			if i == 1 && strings.EqualFold(u.Hostname(), "submit.backtrace.io") ||
				n >= 3 && i == n-2 && submissionFormats[strings.ToLower(segments[n-1])] ||
				hexTokenPattern.MatchString(segment) {
				secrets = append(secrets, segment)
			}
		}
	}
	slices.SortFunc(secrets, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	return slices.DeleteFunc(slices.Compact(secrets), func(s string) bool { return s == "" })
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
