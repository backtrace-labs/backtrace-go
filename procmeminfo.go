package bt

import (
	"bufio"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
)

const (
	memPath  = "/proc/meminfo"
	procPath = "/proc/self/status"
)

var (
	procPaths  = []string{memPath, procPath}
	procMapper = map[string]string{
		"MemTotal":                   "system.memory.total",
		"MemFree":                    "system.memory.free",
		"MemAvailable":               "system.memory.available",
		"Buffers":                    "system.memory.buffers",
		"Cached":                     "system.memory.cached",
		"SwapCached":                 "system.memory.swap.cached",
		"Active":                     "system.memory.active",
		"Inactive":                   "system.memory.inactive",
		"SwapTotal":                  "system.memory.swap.total",
		"SwapFree":                   "system.memory.swap.free",
		"Dirty":                      "system.memory.dirty",
		"Writeback":                  "system.memory.writeback",
		"Slab":                       "system.memory.slab",
		"VmallocTotal":               "system.memory.vmalloc.total",
		"VmallocUsed":                "system.memory.vmalloc.used",
		"VmallocChunk":               "system.memory.vmalloc.chunk",
		"nonvoluntary_ctxt_switches": "sched.cs.involuntary",
		"voluntary_ctxt_switches":    "sched.cs.voluntary",
		"FDSize":                     "descriptor.count",
		"VmData":                     "vm.data.size",
		"VmLck":                      "vm.locked.size",
		"VmPTE":                      "vm.pte.size",
		"VmHWM":                      "vm.rss.peak",
		"VmRSS":                      "vm.rss.size",
		"VmLib":                      "vm.shared.size",
		"VmStk":                      "vm.stack.size",
		"VmSwap":                     "vm.swap.size",
		"VmPeak":                     "vm.vma.peak",
		"VmSize":                     "vm.vma.size",
	}
)

// updateAttrsWithProcMemInfo adds memory and scheduler attributes from
// /proc on Linux; it is a no-op elsewhere.
func updateAttrsWithProcMemInfo(attributes map[string]interface{}, d diag) {
	if runtime.GOOS != "linux" {
		return
	}
	for _, path := range procPaths {
		readFileIntoAttrs(path, attributes, d)
	}
}

func readFileIntoAttrs(path string, attributes map[string]interface{}, d diag) {
	file, err := os.Open(path)
	if err != nil {
		d.logf("readFileIntoAttrs: %v", err)
		return
	}
	defer file.Close()
	readKeyValueLinesIntoAttrs(file, attributes)
}

// readKeyValueLinesIntoAttrs parses "Key:   value [kB]" lines, mapping known
// keys to Backtrace attribute names. Values are emitted as numbers where
// possible (kB values converted to bytes) so the Backtrace query engine can
// aggregate them.
func readKeyValueLinesIntoAttrs(r io.Reader, attributes map[string]interface{}) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		btAttr, known := procMapper[key]
		if !known {
			continue
		}
		attributes[btAttr] = normalizeProcValue(value)
	}
}

// normalizeProcValue converts proc values to int64 where possible; " kB"
// suffixed values become bytes.
func normalizeProcValue(value string) interface{} {
	value = strings.TrimSpace(value)
	if kb, found := strings.CutSuffix(value, " kB"); found {
		if n, err := strconv.ParseInt(strings.TrimSpace(kb), 10, 64); err == nil {
			return n * 1024
		}
		return value
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return n
	}
	return value
}
