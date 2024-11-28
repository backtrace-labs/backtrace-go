package bt

import (
	"bufio"
	"io"
	"log"
	"os"
	"strings"
)

const (
	memPath  = "/proc/meminfo"
	procPath = "/proc/self/status"
)

var (
	paths  = []string{memPath, procPath}
	memMap = map[string]string{
		"MemTotal":     "system.memory.total",
		"MemFree":      "system.memory.free",
		"MemAvailable": "system.memory.available",
		"Buffers":      "system.memory.buffers",
		"Cached":       "system.memory.cached",
		"SwapCached":   "system.memory.swap.cached",
		"Active":       "system.memory.active",
		"Inactive":     "system.memory.inactive",
		"SwapTotal":    "system.memory.swap.total",
		"SwapFree":     "system.memory.swap.free",
		"Dirty":        "system.memory.dirty",
		"Writeback":    "system.memory.writeback",
		"Slab":         "system.memory.slab",
		"VmallocTotal": "system.memory.vmalloc.total",
		"VmallocUsed":  "system.memory.vmalloc.used",
		"VmallocChunk": "system.memory.vmalloc.chunk",
	}

	procMap = map[string]string{
		"nonvoluntary_ctxt_switches": " sched.cs.involuntary",
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

func readMemProcInfo() {
	for _, path := range paths {
		readFile(path)
	}
}

func readFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	for {
		l, _, err := reader.ReadLine()
		if err != nil {
			if err == io.EOF {
				break
			} else {
				if Options.DebugBacktrace {
					log.Printf("readFile err: %v", err)
				}
				break
			}
		}

		values := strings.Split(string(l), ":")
		if len(values) == 2 {
			trimmedValue := strings.TrimSpace(values[1])
			if path == memPath {
				mapValues(values[0], trimmedValue, memMap)
			} else {
				mapValues(values[0], trimmedValue, procMap)
			}
		}
	}
}

func mapValues(key string, value string, mapper map[string]string) {
	if attr, exists := mapper[key]; exists {
		Options.Attributes[attr] = value
	}
}
