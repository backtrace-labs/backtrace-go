//go:build !linux && !freebsd

package bt

import (
	"errors"
	"os"
	"slices"
	"testing"
	"time"
)

func TestBTTracerStub(t *testing.T) {
	UpdateConfig(GlobalConfig{RateLimit: 2 * time.Second, SynchronousPut: true})
	defer UpdateConfig(GlobalConfig{
		PanicOnKillFailure: true,
		ResendSignal:       true,
		RateLimit:          3 * time.Second,
		SynchronousPut:     true,
	})

	var tr TracerSig = New(NewOptions{})
	stub := tr.(*BTTracer)

	if err := Trace(tr, errors.New("boom"), nil); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Trace = %v, want ErrUnsupportedPlatform", err)
	}
	start := time.Now()
	if err := Trace(tr, errors.New("boom"), nil); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("second Trace = %v, want ErrUnsupportedPlatform", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("second stub trace took %v; the rate limit applied although no tracer ran", elapsed)
	}

	func() {
		defer Recover(tr, false, nil)
		panic("recovered by the stub")
	}()

	for name, err := range map[string]error{
		"ConfigurePut":  stub.ConfigurePut("https://host", "tok", PutOptions{}),
		"Put":           stub.Put([]byte("snapshot")),
		"PutDir":        stub.PutDir(t.TempDir()),
		"SetOutputPath": stub.SetOutputPath(t.TempDir(), 0),
	} {
		if !errors.Is(err, ErrUnsupportedPlatform) {
			t.Errorf("%s = %v, want ErrUnsupportedPlatform", name, err)
		}
	}

	got := stub.AppendOptionWithPrefix([]string{"--module=go:enable,true"}, "--module=go:", "goid,7")
	if !slices.Equal(got, []string{"--module=go:enable,true,goid,7"}) {
		t.Errorf("AppendOptionWithPrefix = %q", got)
	}
	if got := stub.AppendOptionWithPrefix(nil, "--module=go:", "goid,7"); got != nil {
		t.Errorf("AppendOptionWithPrefix(nil) = %q, want nil", got)
	}
	if got := AppendOptionWithPrefix([]string{"-x"}, "--kv=", "a"); !slices.Equal(got, []string{"-x", "--kv=a"}) {
		t.Errorf("package AppendOptionWithPrefix = %q", got)
	}

	if d := stub.DefaultTraceOptions(); !d.Faulted || !d.ErrClassification || d.Timeout != 120*time.Second {
		t.Errorf("DefaultTraceOptions = %+v", d)
	}

	stub.SetSigset(os.Interrupt)
	if got := stub.Sigset(); len(got) != 0 {
		t.Errorf("Sigset after SetSigset = %v, want empty", got)
	}
	Register(tr)
}
