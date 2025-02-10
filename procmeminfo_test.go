package bt

import (
	"reflect"
	"strings"
	"testing"
)

func Test_readKeyValueLinesIntoAttrs(t *testing.T) {
	t.Run("ok_mem_info", func(t *testing.T) {
		r := strings.NewReader(meminfoFile)
		attrs := make(map[string]interface{})
		readKeyValueLinesIntoAttrs(r, attrs)
		requireSubmap(t, map[string]interface{}{
			"system.memory.total": "1033457664",
			"system.memory.free":  "149852160",
			"system.memory.dirty": "20480",
		}, attrs)
	})

	t.Run("ok_proc_status", func(t *testing.T) {
		r := strings.NewReader(procStatusFile)
		attrs := make(map[string]interface{})
		readKeyValueLinesIntoAttrs(r, attrs)
		requireSubmap(t, map[string]interface{}{
			"vm.vma.peak":      "9048064",
			"descriptor.count": "256",
		}, attrs)
	})
}

func requireSubmap[K comparable, V any](t *testing.T, submap map[K]V, actual map[K]V) {
	actualKeys := make([]K, 0, len(actual))
	for k := range actual {
		actualKeys = append(actualKeys, k)
	}
	for smKey, smVal := range submap {
		val, ok := actual[smKey]
		if !ok {
			t.Errorf("key is missing: %v, keys %v", smKey, actualKeys)
			t.FailNow()
		}
		requireEqual(t, smVal, val)
	}
}

func requireEqual(t *testing.T, expected interface{}, actual interface{}) {
	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("values not equal, expected: %v actual: %v", expected, actual)
		t.FailNow()
	}
}

var meminfoFile = `
MemTotal:        1009236 kB
MemFree:          146340 kB
MemAvailable:     662052 kB
Buffers:          201576 kB
Cached:           413916 kB
SwapCached:         2548 kB
Active:           429724 kB
Inactive:         258732 kB
Active(anon):      52128 kB
Inactive(anon):    23880 kB
Active(file):     377596 kB
Inactive(file):   234852 kB
Unevictable:           0 kB
Mlocked:               0 kB
SwapTotal:        974844 kB
SwapFree:         909564 kB
Dirty:                20 kB
Writeback:             0 kB
AnonPages:         71580 kB
Mapped:            45072 kB
Shmem:              3044 kB
Slab:             127736 kB
SReclaimable:      60948 kB
SUnreclaim:        66788 kB
KernelStack:        2472 kB
PageTables:         8084 kB
NFS_Unstable:          0 kB
Bounce:                0 kB
WritebackTmp:          0 kB
CommitLimit:     1479460 kB
Committed_AS:     695168 kB
VmallocTotal:   34359738367 kB
VmallocUsed:           0 kB
VmallocChunk:          0 kB
HardwareCorrupted:     0 kB
AnonHugePages:         0 kB
ShmemHugePages:        0 kB
ShmemPmdMapped:        0 kB
CmaTotal:              0 kB
CmaFree:               0 kB
HugePages_Total:       0
HugePages_Free:        0
HugePages_Rsvd:        0
HugePages_Surp:        0
Hugepagesize:       2048 kB
DirectMap4k:     1003456 kB
DirectMap2M:       45056 kB
`

var procStatusFile = `
Name:	cat
Umask:	0002
State:	R (running)
Tgid:	13697
Ngid:	0
Pid:	13697
PPid:	13625
TracerPid:	0
Uid:	1001	1001	1001	1001
Gid:	1001	1001	1001	1001
FDSize:	256
Groups:	4 27 110 1001
NStgid:	13697
NSpid:	13697
NSpgid:	13697
NSsid:	13625
VmPeak:	    8836 kB
VmSize:	    8836 kB
VmLck:	       0 kB
VmPin:	       0 kB
VmHWM:	     784 kB
VmRSS:	     784 kB
RssAnon:	      60 kB
RssFile:	     724 kB
RssShmem:	       0 kB
VmData:	     312 kB
VmStk:	     132 kB
VmExe:	      32 kB
VmLib:	    2120 kB
VmPTE:	      60 kB
VmSwap:	       0 kB
HugetlbPages:	       0 kB
CoreDumping:	0
Threads:	1
SigQ:	0/3676
SigPnd:	0000000000000000
ShdPnd:	0000000000000000
SigBlk:	0000000000000000
SigIgn:	0000000000000000
SigCgt:	0000000000000000
CapInh:	0000000000000000
CapPrm:	0000000000000000
CapEff:	0000000000000000
CapBnd:	0000003fffffffff
CapAmb:	0000000000000000
NoNewPrivs:	0
Seccomp:	0
Speculation_Store_Bypass:	vulnerable
Cpus_allowed:	1
Cpus_allowed_list:	0
Mems_allowed:	00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000000,00000001
Mems_allowed_list:	0
voluntary_ctxt_switches:	0
nonvoluntary_ctxt_switches:	0
`
