// Package logbridge passes state between package bt and its logging integrations.
package logbridge

import (
	"context"
	"reflect"
	"runtime"
	"sync/atomic"
)

type callSiteKey struct{}

func WithCallSite(ctx context.Context, pc uintptr) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, callSiteKey{}, pc)
}

func CallSite(ctx context.Context) uintptr {
	if ctx == nil {
		return 0
	}
	pc, _ := ctx.Value(callSiteKey{}).(uintptr)
	return pc
}

const maxStackDepth = 256

var (
	writers        atomic.Int64
	handlers       atomic.Int64
	writeFuncName  = funcName(WriteDiagnostic)
	handleFuncName = funcName(HandleRecord)
)

func funcName(f func(func())) string {
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}

func WriteDiagnostic(write func()) {
	writers.Add(1)
	defer writers.Add(-1)
	write()
}

func HandleRecord(handle func()) {
	handlers.Add(1)
	defer handlers.Add(-1)
	handle()
}

func InDiagnostic() bool {
	return onStack(&writers, writeFuncName)
}

func InRecord() bool {
	return onStack(&handlers, handleFuncName)
}

func onStack(active *atomic.Int64, name string) bool {
	if active.Load() == 0 {
		return false
	}
	var pcs [maxStackDepth]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs[:])])
	for {
		f, more := frames.Next()
		if f.Function == name {
			return true
		}
		if !more {
			return false
		}
	}
}
