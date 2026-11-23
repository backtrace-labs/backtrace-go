package logbridge

import (
	"context"
	"testing"
)

func TestInDiagnostic(t *testing.T) {
	if InDiagnostic() {
		t.Error("InDiagnostic outside WriteDiagnostic = true")
	}
	var inside, nested, otherGoroutine bool
	WriteDiagnostic(func() {
		inside = InDiagnostic()
		func() { nested = InDiagnostic() }()
		done := make(chan struct{})
		go func() {
			defer close(done)
			otherGoroutine = InDiagnostic()
		}()
		<-done
	})
	if !inside || !nested || otherGoroutine {
		t.Errorf("inside=%v nested=%v other goroutine=%v, want true true false", inside, nested, otherGoroutine)
	}
	if InDiagnostic() || writers.Load() != 0 {
		t.Errorf("after WriteDiagnostic: InDiagnostic=%v writers=%d", InDiagnostic(), writers.Load())
	}

	func() {
		defer func() { _ = recover() }()
		WriteDiagnostic(func() { panic("logger") })
	}()
	if writers.Load() != 0 {
		t.Errorf("writers = %d after a panicking write, want 0", writers.Load())
	}
}

func TestInRecord(t *testing.T) {
	if InRecord() {
		t.Error("InRecord outside HandleRecord = true")
	}
	var inside, nested, otherGoroutine, diagnostic bool
	HandleRecord(func() {
		inside = InRecord()
		func() { nested = InRecord() }()
		diagnostic = InDiagnostic()
		done := make(chan struct{})
		go func() {
			defer close(done)
			otherGoroutine = InRecord()
		}()
		<-done
	})
	if !inside || !nested || otherGoroutine || diagnostic {
		t.Errorf("inside=%v nested=%v other goroutine=%v InDiagnostic=%v, want true true false false",
			inside, nested, otherGoroutine, diagnostic)
	}
	WriteDiagnostic(func() {
		if InRecord() {
			t.Error("InRecord inside WriteDiagnostic = true")
		}
	})

	func() {
		defer func() { _ = recover() }()
		HandleRecord(func() { panic("handler") })
	}()
	if InRecord() || handlers.Load() != 0 {
		t.Errorf("after a panicking handle: InRecord=%v handlers=%d", InRecord(), handlers.Load())
	}
}

func TestCallSite(t *testing.T) {
	var nilCtx context.Context
	if CallSite(nilCtx) != 0 || CallSite(context.Background()) != 0 {
		t.Error("CallSite without a stored PC must be 0")
	}
	if got := CallSite(WithCallSite(nilCtx, 42)); got != 42 {
		t.Errorf("CallSite = %d, want 42", got)
	}
	if got := CallSite(WithCallSite(context.Background(), 7)); got != 7 {
		t.Errorf("CallSite = %d, want 7", got)
	}
}
