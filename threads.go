package bt

import (
	"os"
	"strconv"
	"strings"
)

// sdkFramePrefix identifies the SDK's own frames, which are filtered from
// reported stacks.
const sdkFramePrefix = "github.com/backtrace-labs/backtrace-go"

// elidedFramesMarker appears in runtime.Stack output when frames are omitted.
const elidedFramesMarker = "...additional frames elided..."

// Thread is one goroutine's captured stack in the Backtrace wire format.
type Thread struct {
	Name   string       `json:"name"`
	Fault  bool         `json:"fault"`
	Stacks []StackFrame `json:"stack"`
}

// StackFrame is a single frame in the Backtrace wire format.
type StackFrame struct {
	FuncName     string `json:"funcName"`
	Library      string `json:"library"`
	SourceCodeID string `json:"sourceCode,omitempty"`
	Line         string `json:"line"`
}

// SourceCode is a source snippet referenced by stack frames.
type SourceCode struct {
	Text        string `json:"text"`
	Path        string `json:"path"`
	StartLine   int    `json:"startLine"`
	StartColumn int    `json:"startColumn"`
	StartPos    int    `json:"startPos"`
	TabWidth    int    `json:"tabWidth"`
}

// sourceOptions controls source snippet embedding; see SourceCodeMode.
type sourceOptions struct {
	mode         SourceCodeMode
	contextLines int
	tabWidth     int
}

// ParseThreadsFromStack parses runtime.Stack output into the Backtrace
// threads and sourceCode payload sections, honoring the global Options for
// source capture (SourceCode mode, ContextLineCount, TabWidth).
func ParseThreadsFromStack(stackTrace []byte) (map[string]Thread, map[string]SourceCode) {
	cfg := optionsToConfig()
	threads, sourceCodes, _ := buildThreads(stackTrace, sourceOptions{
		mode:         cfg.SourceCode,
		contextLines: cfg.ContextLineCount,
		tabWidth:     cfg.TabWidth,
	})
	return threads, sourceCodes
}

// buildThreads structurally parses runtime.Stack output. Unlike positional
// (odd/even line) parsing it survives elided-frame markers, "created by"
// lines, frames without locations, and Windows paths.
func buildThreads(stackTrace []byte, opts sourceOptions) (map[string]Thread, map[string]SourceCode, string) {
	threads := map[string]Thread{}
	sources := newSourceBuilder(opts)

	var (
		current   *Thread
		threadID  = -1
		pending   *StackFrame // function seen, waiting for its location line
		skipLoc   bool        // next location line belongs to a skipped frame
		sdkOnly   bool        // every frame so far was SDK-internal
		mainKey   string
		threadKey string
	)
	flushCur := func() {
		if current == nil {
			return
		}
		// Goroutines whose only frames were SDK-internal (e.g. the
		// SDK's own send worker) are pure noise — drop them. Threads
		// that are legitimately frameless ("stack unavailable") stay.
		if len(current.Stacks) == 0 && sdkOnly {
			return
		}
		threads[threadKey] = *current
	}

	for _, rawLine := range strings.Split(string(stackTrace), "\n") {
		if strings.TrimSpace(rawLine) == "" {
			continue
		}

		indented := rawLine[0] == '\t' || rawLine[0] == ' '
		line := strings.TrimSpace(rawLine)

		switch {
		case !indented && strings.HasPrefix(line, "goroutine ") && strings.HasSuffix(line, ":"):
			// New goroutine header.
			flushCur()
			threadID++
			threadKey = strconv.Itoa(threadID)
			if threadID == 0 {
				mainKey = threadKey
			}
			current = &Thread{
				Name:   strings.TrimSuffix(line, ":"),
				Fault:  threadID == 0,
				Stacks: []StackFrame{},
			}
			pending, skipLoc, sdkOnly = nil, false, false

		case !indented && strings.HasPrefix(line, "[originating from goroutine "):
			// GODEBUG=tracebackancestors section: these frames
			// describe the creating goroutine's history, not a
			// live thread. Ignore until the next goroutine header.
			flushCur()
			current = nil
			pending, skipLoc = nil, false

		case !indented:
			// Function line (or a special marker).
			pending, skipLoc = nil, false
			if current == nil || line == elidedFramesMarker {
				continue
			}
			qualified := trimCreatedBy(line)
			if strings.HasPrefix(qualified, sdkFramePrefix) {
				if len(current.Stacks) == 0 {
					sdkOnly = true
				}
				skipLoc = true
				continue
			}
			library, function := splitQualifiedFunction(qualified)
			if function == "panic" && library == "" {
				library = "runtime"
			}
			pending = &StackFrame{FuncName: function, Library: library}

		default:
			// Location line ("\t/path/file.go:42 +0x1f").
			if skipLoc {
				skipLoc = false
				continue
			}
			if pending == nil || current == nil {
				continue
			}
			path, lineNo := splitLocation(line)
			pending.Line = lineNo
			pending.SourceCodeID = sources.reference(path, lineNo)
			current.Stacks = append(current.Stacks, *pending)
			pending = nil
		}
	}
	flushCur()

	return threads, sources.result(), mainKey
}

// splitLocation parses "\t/path/file.go:42 +0x1f" into path and line. The
// split uses the last colon so Windows drive letters ("C:/app/main.go:42")
// survive.
func splitLocation(line string) (path, lineNo string) {
	line, _, _ = strings.Cut(line, " +")
	line = strings.TrimSpace(line)
	idx := strings.LastIndex(line, ":")
	if idx < 0 {
		return line, ""
	}
	return line[:idx], line[idx+1:]
}

// splitQualifiedFunction splits a qualified name from a runtime.Stack
// function line into library (package path, possibly with receiver) and
// function name. Handles argument suffixes, method receivers, generic
// instantiations, and names without any dot.
//
//	"main.main()"                     -> "main", "main"
//	"testing.(*T).Run(0x14, {...})"   -> "testing.(*T)", "Run"
//	"pkg.F[go.shape.int](0x1)"        -> "pkg", "F[go.shape.int]"
//	"panic({0x1?, 0x2?})"             -> "", "panic"
func splitQualifiedFunction(line string) (library, function string) {
	// Strip the argument list.
	if strings.HasSuffix(line, ")") {
		if open := strings.LastIndex(line, "("); open != -1 {
			// Don't strip a method receiver like "pkg.(*T).Run".
			if !strings.HasPrefix(line[open:], "(*") || strings.HasSuffix(line, "})") {
				line = line[:open]
			}
		}
	}

	// Split at the last dot that sits at bracket depth zero: dots inside
	// generic type arguments ("pkg.F[go.shape.int]",
	// "pkg.(*Cache[go.shape.string]).Get") must never be the split point.
	depth, lastDot := 0, -1
	for i, r := range line {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
		case '.':
			if depth == 0 {
				lastDot = i
			}
		}
	}
	if lastDot < 0 {
		return "", line
	}
	return line[:lastDot], line[lastDot+1:]
}

// trimCreatedBy reduces "created by pkg.fn in goroutine 7" to "pkg.fn".
func trimCreatedBy(line string) string {
	if strings.HasPrefix(line, "created by") {
		_, line, _ = strings.Cut(line, " by ")
		line, _, _ = strings.Cut(line, " in ")
	}
	return line
}

// sourceBuilder deduplicates and extracts source snippets for stack frames.
type sourceBuilder struct {
	opts    sourceOptions
	ids     map[string]string // dedup key -> snippet ID
	entries map[string]SourceCode
	files   map[string][]string // per-report file line cache
	failed  map[string]bool
	nextID  int
}

func newSourceBuilder(opts sourceOptions) *sourceBuilder {
	return &sourceBuilder{
		opts:    opts,
		ids:     map[string]string{},
		entries: map[string]SourceCode{},
		files:   map[string][]string{},
		failed:  map[string]bool{},
	}
}

// reference registers a (path, line) pair and returns the snippet ID a frame
// should carry, or "" when source capture is disabled.
func (b *sourceBuilder) reference(path, lineNo string) string {
	if b.opts.mode == SourceCodeNone || path == "" {
		return ""
	}

	key := path
	if b.opts.mode == SourceCodeContext {
		key = path + ":" + lineNo
	}
	if id, ok := b.ids[key]; ok {
		return id
	}

	id := strconv.Itoa(b.nextID)
	b.nextID++
	b.ids[key] = id
	b.entries[id] = b.extract(path, lineNo)
	return id
}

func (b *sourceBuilder) result() map[string]SourceCode {
	return b.entries
}

// extract builds the snippet for path around lineNo according to the mode.
// Unreadable files degrade to a path-only entry.
func (b *sourceBuilder) extract(path, lineNo string) SourceCode {
	sc := SourceCode{Path: path}

	lines := b.readLines(path)
	if lines == nil {
		return sc
	}

	switch b.opts.mode {
	case SourceCodeFile:
		sc.Text = strings.Join(lines, "\n")
		sc.StartLine = 1
	default: // SourceCodeContext
		center, err := strconv.Atoi(lineNo)
		if err != nil || center < 1 {
			return sc
		}
		start := center - b.opts.contextLines
		if start < 1 {
			start = 1
		}
		end := center + b.opts.contextLines
		if end > len(lines) {
			end = len(lines)
		}
		if start > len(lines) {
			return sc
		}
		sc.Text = strings.Join(lines[start-1:end], "\n")
		sc.StartLine = start
	}

	sc.StartColumn = 1
	sc.StartPos = 0
	sc.TabWidth = b.opts.tabWidth
	return sc
}

// readLines reads and caches a source file for the duration of one report.
func (b *sourceBuilder) readLines(path string) []string {
	if lines, ok := b.files[path]; ok {
		return lines
	}
	if b.failed[path] {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		b.failed[path] = true
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	b.files[path] = lines
	return lines
}
