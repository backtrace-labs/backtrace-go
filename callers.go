package bt

import (
	"runtime"
	"slices"
	"strconv"
	"strings"
)

const (
	maxCallerFrames     = 512
	maxCallerStackBytes = 64 << 10
)

func callerPCs() []uintptr {
	var pcs [maxCallerFrames]uintptr
	return slices.Clone(pcs[:runtime.Callers(2, pcs[:])])
}

func callerGoroutine() (name string, creator []byte) {
	text := captureStack(false, maxCallerStackBytes)
	name, creator = splitGoroutineText(string(text))
	if len(text) == maxCallerStackBytes {
		creator = nil
	}
	return name, creator
}

func splitGoroutineText(text string) (name string, creator []byte) {
	text, _, _ = strings.Cut(text, "\n[originating from goroutine ")
	header, _, _ := strings.Cut(text, "\n")
	if i := strings.LastIndex(text, "\ncreated by "); i >= 0 {
		creator = []byte(header + text[i:])
	}
	return strings.TrimSuffix(header, ":"), creator
}

func framesFromPCs(pcs []uintptr, sources *sourceBuilder) []StackFrame {
	var all []runtime.Frame
	frames := runtime.CallersFrames(pcs)
	for {
		f, more := frames.Next()
		if f.Function != "" {
			all = append(all, f)
		}
		if !more {
			break
		}
	}
	shown := make([]runtime.Frame, 0, len(all))
	for _, f := range all {
		if showFrame(f.Function, len(shown) == 0) {
			shown = append(shown, f)
		}
	}
	if len(shown) == 0 {
		shown = all
	}

	out := make([]StackFrame, 0, len(shown))
	for _, f := range shown {
		if isSDKFrame(f.Function) {
			continue
		}
		library, function := splitQualifiedFunction(f.Function)
		if f.Function == "runtime.gopanic" {
			library, function = "runtime", "panic"
		}
		line := strconv.Itoa(f.Line)
		out = append(out, StackFrame{
			FuncName:     function,
			Library:      library,
			Line:         line,
			SourceCodeID: sources.reference(f.File, line),
		})
	}
	return out
}

// Mirrors the runtime.Stack frame filter.
func showFrame(name string, first bool) bool {
	switch name {
	case "runtime.gopanic":
		return !first
	case "runtime.runFinalizers", "runtime.runCleanups":
		return true
	}
	return strings.Contains(name, ".") && (!strings.HasPrefix(name, "runtime.") || isExportedRuntime(name))
}

func isExportedRuntime(name string) bool {
	name, found := strings.CutPrefix(name, "runtime.")
	if !found {
		return false
	}
	receiver := ""
	if i := strings.LastIndex(name, "."); i >= 0 {
		receiver, name = name[:i], name[i+1:]
		if len(receiver) >= 3 && strings.HasPrefix(receiver, "(*") && strings.HasSuffix(receiver, ")") {
			receiver = receiver[2 : len(receiver)-1]
		}
	}
	return isUpperASCII(name) && (receiver == "" || isUpperASCII(receiver))
}

func isUpperASCII(s string) bool {
	return s != "" && 'A' <= s[0] && s[0] <= 'Z'
}

func markPanicFrame(frames []StackFrame) {
	for i, f := range frames {
		if f.FuncName != "panic" || f.Library != "runtime" {
			continue
		}
		for j := i + 1; j < len(frames); j++ {
			if lib := frames[j].Library; lib != "runtime" && !strings.HasPrefix(lib, "runtime.") {
				frames[j].Faulted = true
				return
			}
		}
		return
	}
}
