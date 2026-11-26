package bt

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	markWindow       = 30 * time.Second
	maxMarks         = 1024
	controlDrainWait = 2 * time.Second
)

var runtimePanicTexts = map[string]bool{
	"assignment to entry in nil map":         true,
	"send on closed channel":                 true,
	"close of closed channel":                true,
	"close of nil channel":                   true,
	"makechan: size out of range":            true,
	"runtime: allocation size out of range":  true,
	"arg size to reflect.call more than 1GB": true,
}

func runCrashMonitor(cfg Config, files string) {
	// Group signals aimed at the application must not end the monitor first.
	ignoreGroupSignals()
	controlID, readyID, _ := strings.Cut(files, ",")
	ready := inheritedFile(readyID)
	_, _ = ready.Write([]byte{'\n'})
	ready.Close()
	code := 1
	func() {
		defer func() { _ = recover() }()
		code = superviseCrashes(cfg, os.Stdin, inheritedFile(controlID))
	}()
	os.Exit(code)
}

func inheritedFile(id string) *os.File {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return nil
	}
	return os.NewFile(uintptr(n), "backtrace-crash-monitor")
}

func superviseCrashes(cfg Config, crash, control io.Reader) int {
	cfg = cloneConfig(cfg).normalize()
	ctl := &controlLog{hello: monitorHello{PID: os.Getppid()}}
	controlDone := make(chan struct{})
	go func() { defer close(controlDone); ctl.read(control) }()

	text := readCrash(crash, cfg.MaxStackBytes)
	if len(text) == 0 {
		return 0
	}
	at := time.Now()
	wait := time.NewTimer(controlDrainWait)
	select {
	case <-controlDone:
	case <-wait.C:
	}
	wait.Stop()

	d := diag{logger: cfg.Logger, debug: cfg.Debug}
	info := parseCrash(text)
	if n := len(info.panics); n > 0 && info.repanicked && ctl.marked(info.goroutine, info.panics[n-1], at) {
		d.logf("crash not sent: ReportPanic already delivered this panic")
		return 0
	}
	parent := ctl.parent()
	if parent.Endpoint != "" {
		cfg.Endpoint, cfg.Token = parent.Endpoint, parent.Token
	}
	c, err := NewClient(cfg)
	if err != nil {
		d.warnf("crash not sent: %v", err)
		return 1
	}
	c.enqueueCrash(info, parent, at)
	ctx, cancel := context.WithTimeout(context.Background(), crashDeliveryTimeout(c.cfgFn()))
	defer cancel()
	if !c.CloseContext(ctx) || c.Stats().Delivered == 0 {
		return 1
	}
	return 0
}

func readCrash(r io.Reader, limit int) []byte {
	text, _ := io.ReadAll(io.LimitReader(r, int64(limit)))
	// Drain past the limit. The crashing runtime blocks on a full pipe.
	_, _ = io.Copy(io.Discard, r)
	return text
}

func crashDeliveryTimeout(cfg Config) time.Duration {
	total := maxRetryWait
	add := func(d time.Duration) {
		if d > math.MaxInt64-total {
			total = math.MaxInt64
			return
		}
		total += d
	}
	for range min(max(cfg.MaxRetries, 0), int(maxRetryWait/retryBackoffBase)) + 1 {
		add(cfg.Timeout)
	}
	add(cfg.ShutdownTimeout)
	return total
}

type controlMark struct {
	mark string
	at   time.Time
}

type controlLog struct {
	mu    sync.Mutex
	hello monitorHello
	marks []controlMark
}

func (l *controlLog) read(r io.Reader) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		l.add(s.Text(), time.Now())
	}
}

func (l *controlLog) add(line string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if strings.HasPrefix(line, "{") {
		var hello monitorHello
		if json.Unmarshal([]byte(line), &hello) == nil {
			l.hello = hello
		}
		return
	}
	l.marks = slices.DeleteFunc(l.marks, func(m controlMark) bool { return at.Sub(m.at) > markWindow })
	if len(l.marks) >= maxMarks {
		l.marks = slices.Delete(l.marks, 0, 1)
	}
	l.marks = append(l.marks, controlMark{mark: line, at: at})
}

func (l *controlLog) parent() monitorHello {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hello
}

func (l *controlLog) marked(goroutine, value string, at time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	mark := panicMark(goroutine, value)
	return slices.ContainsFunc(l.marks, func(m controlMark) bool {
		return m.mark == mark && at.Sub(m.at) <= markWindow
	})
}

type crashInfo struct {
	text       []byte
	message    string
	classifier string
	reportType string
	panics     []string
	repanicked bool
	goroutine  string
}

func parseCrash(text []byte) crashInfo {
	info := crashInfo{text: text, message: "fatal error", classifier: "fatal error", reportType: "crash"}
	lines := strings.Split(string(text), "\n")
	var preamble []string
	for i, line := range lines {
		if strings.HasPrefix(line, "goroutine ") || line == "runtime stack:" {
			break
		}
		switch {
		case strings.HasPrefix(line, "panic: "):
			block := headerBlock(lines[i:])
			info.message = strings.Join(block, "\n")
			info.panics, info.repanicked = panicValues(block)
			info.classifier = panicClassifier(info.panics[len(info.panics)-1])
			info.reportType = "panic"
			info.goroutine = firstGoroutineID(lines[i+len(block):])
			return info
		case signalName(line) != "":
			info.message = line
			info.classifier = signalName(line)
			return info
		case strings.TrimSpace(line) != "":
			preamble = append(preamble, line)
		}
	}
	if len(preamble) > 0 {
		info.message += "\n" + strings.Join(preamble, "\n")
	}
	return info
}

func headerBlock(lines []string) []string {
	n := 1
	for n < len(lines) && strings.HasPrefix(lines[n], "\t") {
		n++
	}
	return lines[:n]
}

func panicValues(block []string) (values []string, repanicked bool) {
	values = []string{strings.TrimPrefix(block[0], "panic: ")}
	for _, line := range block[1:] {
		line = strings.TrimPrefix(line, "\t")
		if v, ok := strings.CutPrefix(line, "panic: "); ok {
			values = append(values, v)
			continue
		}
		values[len(values)-1] += "\n" + line
	}
	for i, v := range values {
		trimmed, ok := strings.CutSuffix(v, " [recovered, repanicked]")
		if ok {
			values[i] = trimmed
		} else {
			values[i] = strings.TrimSuffix(v, " [recovered]")
		}
		repanicked = ok
	}
	return values, repanicked
}

func firstGoroutineID(lines []string) string {
	for _, line := range lines {
		if strings.HasPrefix(line, "goroutine ") {
			return goroutineID(line)
		}
	}
	return ""
}

func panicClassifier(value string) string {
	if strings.HasPrefix(value, "runtime error: ") ||
		strings.HasPrefix(value, "interface conversion: ") ||
		runtimePanicTexts[value] ||
		(strings.HasPrefix(value, "value method ") && strings.Contains(value, " called using nil *")) {
		return "runtime.Error"
	}
	return "panic"
}

func signalName(line string) string {
	name, _, ok := strings.Cut(line, ": ")
	if !ok || len(name) < 4 || !strings.HasPrefix(name, "SIG") ||
		strings.Trim(name[3:], "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		return ""
	}
	return name
}

func (c *Client) enqueueCrash(info crashInfo, parent monitorHello, at time.Time) {
	cfg := c.cfgFn()
	// Runtime and memory attributes would describe the monitor process.
	attributes := map[string]any{}
	for k, v := range staticAttributes() {
		attributes[k] = v
	}
	for k, v := range cfg.Attributes {
		attributes[k] = v
	}
	if parent.PID > 0 {
		attributes["process.id"] = parent.PID
	}
	if parent.Session != "" {
		attributes["application.session"] = parent.Session
	}
	if parent.Start > 0 {
		attributes["process.age"] = int64(at.Sub(time.UnixMilli(parent.Start)).Seconds())
	}
	attributes["error.message"] = info.message
	attributes["report_type"] = info.reportType
	if _, set := attributes["error.type"]; !set {
		attributes["error.type"] = defaultErrorTypes[kindCrash]
	}
	qr := &queuedReport{
		cfg:         cfg,
		stack:       info.text,
		attributes:  attributes,
		annotations: map[string]any{},
		classifiers: []string{info.classifier},
		kind:        kindCrash,
		timestamp:   at.Unix(),
	}
	c.enqueueContext(nonBlocking, lanePriority, clientJob{data: qr})
}
