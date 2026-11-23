//go:build linux || freebsd

// Command bcd demonstrates the out-of-process tracer integration: signal
// handling, panic recovery, and manual trace requests with snapshot upload.
//
// Snapshots go to BACKTRACE_ENDPOINT with BACKTRACE_TOKEN when both are set
// (https://{universe}.sp.backtrace.io:6098 and a project token). Otherwise
// they go to a local server that prints each upload it receives.
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"syscall"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
)

var (
	tracer *bt.BTTracer
	wg     sync.WaitGroup
)

// panicAndRecover triggers a panic that is traced and then recovered.
func panicAndRecover() {
	defer bt.Recover(tracer, false, &bt.TraceOptions{
		Faulted:           true,
		CallerOnly:        true,
		ErrClassification: true,
		SpawnedGs:         &wg,
	})

	panic("panic error")
}

// raiseSignal sends SIGSEGV to this process; the registered tracer handles it.
func raiseSignal() bool {
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		fmt.Printf("error: failed to find process object: %v\n", err)
		return false
	}

	if err := p.Signal(syscall.SIGSEGV); err != nil {
		fmt.Printf("error: failed to send signal: %v\n", err)
		return false
	}
	return true
}

// requestTrace asks for a snapshot explicitly, without any fault.
func requestTrace() {
	wg.Add(1)
	go func() {
		defer wg.Done()

		fmt.Println("Requesting trace...")

		err := errors.New("trace-request")
		if traceErr := bt.Trace(tracer, err, &bt.TraceOptions{
			// Faulted and CallerOnly don't make sense for
			// asynchronous trace requests.
			Faulted:           false,
			CallerOnly:        false,
			ErrClassification: true,
			Classifications:   []string{"example", "manual-trace"},
			SpawnedGs:         &wg,
		}); traceErr != nil {
			fmt.Printf("Failed to trace: %v\n", traceErr)
		}
	}()
}

func main() {
	// On kernels with restrictive ptrace_scope settings this call allows
	// a non-parent tracer to attach to this process.
	if err := bt.EnableTracing(); err != nil {
		fmt.Printf("Warning: failed to enable tracing permission: %v\n", err)
	}

	bt.UpdateConfig(bt.GlobalConfig{
		PanicOnKillFailure: true,
		ResendSignal:       true,
		RateLimit:          time.Second * 5,
		SynchronousPut:     false,
	})

	tracer = bt.New(bt.NewOptions{IncludeSystemGs: false})
	tracer.AddOptions(nil, "-L", "WARNING")
	tracer.AddKV(nil, "version", "1.2.3")
	tracer.SetLogLevel(bt.LogMax)

	if err := tracer.SetOutputPath("./tracedir", 0755); err != nil {
		fmt.Printf("Warning: failed to set output path: %v\n"+
			"Generated snapshots will be stored in cwd.\n", err)
	}

	// Tracer I/O is directed to os.DevNull by default.
	logFile, err := os.Create("./tracelog")
	if err != nil {
		fmt.Printf("Warning: failed to create trace log: %v\n", err)
	} else {
		defer logFile.Close()
		tracer.SetPipes(nil, logFile)
	}

	endpoint, token := os.Getenv("BACKTRACE_ENDPOINT"), os.Getenv("BACKTRACE_TOKEN")
	if endpoint == "" || token == "" {
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body)
			fmt.Printf("snapshot upload received: %d bytes\n", n)
		}))
		defer local.Close()
		endpoint, token = local.URL, "example-token"
	}
	if err := tracer.ConfigurePut(
		endpoint,
		token,
		bt.PutOptions{Unlink: true, OnTrace: true},
	); err != nil {
		fmt.Printf("Failed to enable put: %v\n", err)
	}

	// Upload any snapshots left over from previous runs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := tracer.PutDir("./tracedir"); err != nil {
			fmt.Printf("Failed to Put from directory: %v\n", err)
		}
	}()

	// Handle crash signals with the tracer's default signal set.
	bt.Register(tracer)

	fmt.Println("Panicking...")
	panicAndRecover()
	fmt.Println("Panic recovered")

	requestTrace()

	wg.Wait()

	// The re-sent signal ends the process after the trace upload.
	fmt.Println("Sending signal...")
	if raiseSignal() {
		select {}
	}
}
