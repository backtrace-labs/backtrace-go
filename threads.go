package bt

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
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

	// Faulted marks the frame that called panic in a panic report.
	Faulted bool `json:"faulted,omitempty"`
}

// SourceCode is a source snippet referenced by stack frames.
type SourceCode struct {
	Text        string `json:"text,omitempty"`
	Path        string `json:"path"`
	StartLine   int    `json:"startLine,omitempty"`
	StartColumn int    `json:"startColumn,omitempty"`
	StartPos    int    `json:"startPos,omitempty"`
	TabWidth    int    `json:"tabWidth,omitempty"`
}

// sourceOptions controls source snippet embedding; see SourceCodeMode.
type sourceOptions struct {
	mode         SourceCodeMode
	contextLines int
	tabWidth     int
	// roots, when non-empty, restricts which files may be read.
	roots []string
	// maxFileBytes caps a single file read; maxTotal caps the aggregate
	// source text embedded in one report.
	maxFileBytes int64
	maxTotal     int64
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
		roots:        cfg.SourceRoots,
		maxFileBytes: cfg.MaxSourceFileBytes,
		maxTotal:     cfg.MaxSourceBytes,
	})
	return threads, sourceCodes
}

// buildThreads structurally parses runtime.Stack output. Unlike positional
// (odd/even line) parsing it survives elided-frame markers, "created by"
// lines, frames without locations, and Windows paths.
func buildThreads(stackTrace []byte, opts sourceOptions) (map[string]Thread, map[string]SourceCode, string) {
	sources := newSourceBuilder(opts)
	defer sources.close()
	threads, mainKey := parseStackText(stackTrace, sources)
	return threads, sources.result(), mainKey
}

func parseStackText(stackTrace []byte, sources *sourceBuilder) (map[string]Thread, string) {
	threads := map[string]Thread{}

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

		case !indented && (line == "runtime stack:" || strings.HasPrefix(line, "[originating from goroutine ")):
			// GODEBUG=tracebackancestors section: these frames
			// describe the creating goroutine's history, not a
			// live thread. Ignore until the next goroutine header.
			// The "runtime stack:" section of crash output is skipped the same way.
			flushCur()
			current = nil
			pending, skipLoc = nil, false

		case !indented:
			// Function line (or a special marker).
			pending, skipLoc = nil, false
			if current == nil || isElidedMarker(line) {
				continue
			}
			qualified := trimCreatedBy(line)
			if isSDKFrame(qualified) {
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

	return threads, mainKey
}

func isElidedMarker(line string) bool {
	if line == elidedFramesMarker {
		return true
	}
	count, ok := strings.CutPrefix(line, "...")
	if !ok {
		return false
	}
	count, ok = strings.CutSuffix(count, " frames elided...")
	return ok && allDigits(count)
}

// splitLocation parses "\t/path/file.go:42 +0x1f" into path and line. The
// split uses the last colon so Windows drive letters ("C:/app/main.go:42")
// survive.
func splitLocation(line string) (path, lineNo string) {
	line = strings.TrimSpace(line)
	for _, marker := range []string{" pc=", " sp=", " fp=", " +"} {
		line = cutHexSuffix(line, marker)
	}
	idx := strings.LastIndex(line, ":")
	if idx < 0 || !allDigits(line[idx+1:]) {
		return line, ""
	}
	return line[:idx], line[idx+1:]
}

func cutHexSuffix(s, marker string) string {
	i := strings.LastIndex(s, marker)
	if i < 0 {
		return s
	}
	digits, ok := strings.CutPrefix(s[i+len(marker):], "0x")
	if !ok || digits == "" || strings.Trim(digits, "0123456789abcdefABCDEF") != "" {
		return s
	}
	return s[:i]
}

func allDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
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

// isSDKFrame reports whether a qualified function name belongs to this SDK,
// requiring an import-path boundary so sibling module paths (e.g.
// ".../backtrace-go-fork") are never filtered.
func isSDKFrame(name string) bool {
	return name == sdkFramePrefix ||
		strings.HasPrefix(name, sdkFramePrefix+"/") ||
		strings.HasPrefix(name, sdkFramePrefix+".")
}

// trimCreatedBy reduces "created by pkg.fn in goroutine 7" to "pkg.fn".
func trimCreatedBy(line string) string {
	if strings.HasPrefix(line, "created by") {
		_, line, _ = strings.Cut(line, " by ")
		line, _, _ = strings.Cut(line, " in ")
	}
	return line
}

// openedRoot pairs an os.Root handle with the absolute paths that name the
// directory: as configured and, when that differs, with symbolic links
// resolved (/tmp on macOS, a symlinked checkout). A frame carries whichever
// form the build saw.
type openedRoot struct {
	names []string
	root  *os.Root
}

// sourceBuilder deduplicates and extracts source snippets for stack frames,
// under per-file and per-report byte budgets and an optional root allowlist.
type sourceBuilder struct {
	opts      sourceOptions
	ids       map[string]string // dedup key -> snippet ID
	entries   map[string]SourceCode
	files     map[string][]string // per-report file line cache
	failed    map[string]bool
	nextID    int
	usedBytes int64 // embedded source text so far (budget accounting)

	// roots are traversal-resistant handles opened once per report when
	// SourceRoots is configured. rootsRequired records that roots WERE
	// configured, so a failure to open them fails closed (never falls
	// back to unrestricted reads).
	roots         []openedRoot
	rootsRequired bool
	// resolvedDirs caches symlink-resolved frame directories for the
	// report ("" when resolving failed or changed nothing).
	resolvedDirs map[string]string
}

func newSourceBuilder(opts sourceOptions) *sourceBuilder {
	b := &sourceBuilder{
		opts:    opts,
		ids:     map[string]string{},
		entries: map[string]SourceCode{},
		files:   map[string][]string{},
		failed:  map[string]bool{},
	}
	if len(opts.roots) > 0 && opts.mode != SourceCodeNone && opts.mode != SourceCodeMetadata {
		b.rootsRequired = true
		for _, root := range opts.roots {
			abs, err := filepath.Abs(root)
			if err != nil {
				continue
			}
			// os.OpenRoot confines subsequent opens: symlinks and ".."
			// that escape the root are refused at open time, closing the
			// check/use race that a separate EvalSymlinks+Open had.
			handle, err := os.OpenRoot(abs)
			if err != nil {
				continue
			}
			names := []string{filepath.Clean(abs)}
			if resolved, err := filepath.EvalSymlinks(abs); err == nil {
				if resolved = filepath.Clean(resolved); resolved != names[0] {
					names = append(names, resolved)
				}
			}
			b.roots = append(b.roots, openedRoot{names: names, root: handle})
		}
	}
	return b
}

// close releases any opened root handles.
func (b *sourceBuilder) close() {
	for _, r := range b.roots {
		_ = r.root.Close()
	}
	b.roots = nil
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
	if b.opts.mode == SourceCodeMetadata {
		// Path/line metadata only; no source text leaves the host.
		b.entries[id] = SourceCode{Path: path}
	} else {
		b.entries[id] = b.extract(path, lineNo)
	}
	return id
}

func (b *sourceBuilder) result() map[string]SourceCode {
	return b.entries
}

// extract builds the snippet for path around lineNo according to the mode.
// Unreadable, disallowed, or over-budget files degrade to path-only entries.
func (b *sourceBuilder) extract(path, lineNo string) SourceCode {
	sc := SourceCode{Path: path}

	// Short-circuit before reading: once the per-report budget is
	// exhausted no further file I/O is useful.
	if b.opts.maxTotal > 0 && b.usedBytes >= b.opts.maxTotal {
		return sc
	}

	lines := b.readLines(path)
	if lines == nil {
		return sc
	}

	var text string
	startLine := 1
	switch b.opts.mode {
	case SourceCodeFile:
		text = strings.Join(lines, "\n")
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
		text = strings.Join(lines[start-1:end], "\n")
		startLine = start
	}

	// Enforce the per-report source budget.
	if b.opts.maxTotal > 0 && b.usedBytes+int64(len(text)) > b.opts.maxTotal {
		return sc
	}
	b.usedBytes += int64(len(text))

	sc.Text = text
	sc.StartLine = startLine
	sc.StartColumn = 1
	sc.StartPos = 0
	sc.TabWidth = b.opts.tabWidth
	return sc
}

// openSource opens a source file, confined to the configured roots when
// SourceRoots is set. It never falls back to an unrestricted open once
// roots were required.
func (b *sourceBuilder) openSource(path string) (*os.File, error) {
	if !b.rootsRequired {
		file, _, err := openRegularFile(hostFiles{}, path)
		return file, err
	}
	if len(b.roots) == 0 {
		// Roots were configured but none could be opened: fail closed.
		return nil, fs.ErrPermission
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	if file, matched, err := b.openInRoots(abs); matched {
		return file, err
	}
	// The frame may reach the file through a symbolic link above the root
	// (built in /tmp/app, root given as /private/tmp/app). Resolving the
	// directory only decides which root the file belongs to: the open is
	// still made through os.Root, and the file name itself is left for
	// os.Root to judge.
	if dir := b.resolveDir(filepath.Dir(abs)); dir != "" {
		if file, matched, err := b.openInRoots(filepath.Join(dir, filepath.Base(abs))); matched {
			return file, err
		}
	}
	return nil, fs.ErrPermission
}

// openInRoots opens abs through the first root that contains it. matched
// reports that a root claimed the path, whatever the open returned: a
// refused open is final and never retried under another name.
func (b *sourceBuilder) openInRoots(abs string) (file *os.File, matched bool, err error) {
	for _, r := range b.roots {
		for _, name := range r.names {
			rel, relErr := filepath.Rel(name, abs)
			if relErr != nil || rel == ".." ||
				strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			// root.Open resolves rel traversal-resistantly (symlinks and
			// ".." that would escape the root are refused).
			file, _, err = openRegularFile(r.root, rel)
			return file, true, err
		}
	}
	return nil, false, nil
}

// resolveDir returns dir with symbolic links resolved, or "" when that
// fails or changes nothing. Cached per report: frames of one package share
// a directory.
func (b *sourceBuilder) resolveDir(dir string) string {
	if resolved, ok := b.resolvedDirs[dir]; ok {
		return resolved
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		resolved = ""
	} else if resolved = filepath.Clean(resolved); resolved == dir {
		resolved = ""
	}
	if b.resolvedDirs == nil {
		b.resolvedDirs = map[string]string{}
	}
	b.resolvedDirs[dir] = resolved
	return resolved
}

// readLines reads and caches a source file for the duration of one report,
// bounded by the per-file byte budget and restricted to a regular file
// inside the configured roots.
func (b *sourceBuilder) readLines(path string) []string {
	if lines, ok := b.files[path]; ok {
		return lines
	}
	if b.failed[path] {
		return nil
	}
	fail := func() []string {
		b.failed[path] = true
		return nil
	}

	file, err := b.openSource(path)
	if err != nil {
		return fail()
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fail()
	}
	maxBytes := b.opts.maxFileBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxSourceFileBytes
	}
	if info.Size() > maxBytes {
		return fail()
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(data)) > maxBytes {
		return fail()
	}

	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	b.files[path] = lines
	return lines
}
