package bt_test

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
)

func crashInGoroutine() { panic("crash-test goroutine panic") }

func unlockUnlocked() {
	var mu sync.Mutex
	mu.Unlock()
}

func reportAndRepanic(c *bt.Client) {
	defer c.ReportPanic(nil)
	panic("reported\nbefore the crash")
}

func reportAndPanicInCleanup(c *bt.Client) {
	defer func() { panic("cleanup failed") }()
	defer c.ReportPanic(nil)
	panic("original")
}

func TestCrashMonitorHelper(t *testing.T) {
	mode := os.Getenv("BACKTRACE_GO_TEST_CRASH")
	if mode == "" {
		t.Skip("started by the MonitorCrashes tests")
	}
	if os.Getenv("CRASH_TEST_ENDPOINT") == "" {
		fmt.Fprintf(os.Stderr, "monitor-pid=%d\n", os.Getpid())
		if mode == "monitorexit" {
			os.Exit(7)
		}
	}
	cfg := bt.Config{
		Endpoint:   os.Getenv("CRASH_TEST_ENDPOINT"),
		Token:      "crash-test-token",
		MaxRetries: -1,
		Timeout:    5 * time.Second,
		SourceCode: bt.SourceCodeNone,
		Attributes: map[string]any{
			"crash.test":    mode,
			"crash.app.env": os.Getenv("CRASH_TEST_ENDPOINT") != "",
		},
	}
	if err := bt.MonitorCrashes(cfg); err != nil {
		fmt.Println("monitor-error:", err)
		os.Exit(3)
	}
	fmt.Printf("crasher-pid=%d session=%s\n", os.Getpid(), bt.ProcessSession())

	switch mode {
	case "goroutine":
		go crashInGoroutine()
	case "fatal":
		unlockUnlocked()
	case "reported", "chained":
		c, err := bt.NewClient(cfg)
		if err != nil {
			fmt.Println("client-error:", err)
			os.Exit(3)
		}
		if mode == "chained" {
			go reportAndPanicInCleanup(c)
		} else {
			go reportAndRepanic(c)
		}
	case "clean":
		if bt.MonitorCrashes(cfg) == nil {
			fmt.Println("second MonitorCrashes call succeeded")
			os.Exit(4)
		}
		return
	}
	time.Sleep(30 * time.Second)
	os.Exit(5)
}
