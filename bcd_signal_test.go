//go:build unix

package bt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestSignalTraceOptions(t *testing.T) {
	defer traceTestConfig()()
	UpdateConfig(GlobalConfig{RateLimit: time.Millisecond, SynchronousPut: false})
	waitTraceLockFree(t)

	puts := make(chan struct{}, 1)
	tr := &recordingTracer{
		sigs: []os.Signal{syscall.SIGUSR1},
		dto: &TraceOptions{
			Faulted:           true,
			CallerOnly:        true,
			ErrClassification: true,
			Classifications:   []string{"service"},
			Timeout:           5 * time.Second,
		},
		put: func([]byte) error {
			puts <- struct{}{}
			return nil
		},
	}
	Register(tr)
	defer Unregister(tr)

	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-puts:
	case <-time.After(10 * time.Second):
		t.Fatal("signal trace did not upload")
	}

	argv := tr.lastArgv()
	for _, flag := range []string{"--fault-thread", "--thread", "--caller-go"} {
		if got := optionValues(argv, flag); len(got) != 0 {
			t.Errorf("signal trace has %s %q", flag, got)
		}
	}
	if got := optionValues(argv, "--classifier"); !slices.Equal(got, []string{"SIGUSR1", "service"}) {
		t.Errorf("classifiers = %q, want [SIGUSR1 service]", got)
	}
	if got := optionValues(argv, "--kv"); !slices.Equal(got, []string{"error:" + syscall.SIGUSR1.String()}) {
		t.Errorf("kv = %q", got)
	}
}

func TestRegisterIgnoresEmptySigset(t *testing.T) {
	tr := &recordingTracer{sigs: []os.Signal{}}
	Register(tr)
	if tr.Sigchan() != nil {
		Unregister(tr)
		t.Fatal("Register with an empty sigset installed a handler for every signal")
	}
}

func TestSignalResendHelper(t *testing.T) {
	if os.Getenv("BACKTRACE_GO_TEST_SIGNAL_RESEND") == "" {
		t.Skip("started by TestSignalTraceUploadsBeforeResend")
	}
	UpdateConfig(GlobalConfig{ResendSignal: true, RateLimit: time.Millisecond, SynchronousPut: false})
	tr := &recordingTracer{
		sigs: []os.Signal{syscall.SIGABRT},
		dto:  &TraceOptions{Timeout: 10 * time.Second},
		put: func([]byte) error {
			time.Sleep(300 * time.Millisecond)
			fmt.Println("snapshot uploaded")
			panic("put exploded")
		},
	}
	Register(tr)
	if err := syscall.Kill(os.Getpid(), syscall.SIGABRT); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

func TestSignalTraceUploadsBeforeResend(t *testing.T) {
	skipWithoutSubprocess(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalResendHelper$")
	cmd.Env = append(os.Environ(), "BACKTRACE_GO_TEST_SIGNAL_RESEND=1", "GOTRACEBACK=single")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("helper did not end within 30s; output:\n%s", out)
	}
	if err == nil {
		t.Fatalf("helper exited cleanly; want the re-sent SIGABRT to end it. Output:\n%s", out)
	}
	uploaded := strings.Index(string(out), "snapshot uploaded")
	aborted := strings.Index(string(out), "SIGABRT")
	if uploaded < 0 || aborted < uploaded || strings.Contains(string(out), "put exploded") {
		t.Fatalf("upload at %d, SIGABRT at %d; want the upload before the re-sent signal and the Put panic contained. Output:\n%s",
			uploaded, aborted, out)
	}
}

func TestRegisterReadsResendSignalWhenSignalArrives(t *testing.T) {
	defer traceTestConfig()()
	waitTraceLockFree(t)

	puts := make(chan struct{}, 1)
	var resent atomic.Bool
	exited := make(chan struct{})
	tr := &recordingTracer{
		sigs: []os.Signal{syscall.SIGUSR1},
		dto:  &TraceOptions{Timeout: 5 * time.Second},
		put: func([]byte) error {
			puts <- struct{}{}
			return nil
		},
		logf: func(msg string) {
			if strings.HasPrefix(msg, "Resending") {
				resent.Store(true)
			}
			if strings.HasPrefix(msg, "Signal channel closed") {
				close(exited)
			}
		},
	}
	Register(tr)
	UpdateConfig(GlobalConfig{RateLimit: time.Millisecond})

	for i := 1; i <= 2; i++ {
		if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
			t.Fatal(err)
		}
		select {
		case <-puts:
		case <-time.After(10 * time.Second):
			t.Fatalf("signal %d was not traced; want the handler kept after UpdateConfig turned ResendSignal off", i)
		}
	}
	if resent.Load() {
		t.Error("signal was re-sent; want ResendSignal read when the signal arrives, not at Register")
	}
	// A config restore that lands before the handler exits re-sends the signal.
	Unregister(tr)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("signal handler goroutine did not exit")
	}
}
