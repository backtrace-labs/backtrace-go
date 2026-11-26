package bt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	monitorEnvVar       = "BACKTRACE_GO_CRASH_MONITOR"
	monitorStartTimeout = 5 * time.Second
)

var controlWriteTimeout = time.Second

var monitorEnvNames = []string{
	"PATH", "HOME", "TMPDIR", "SYSTEMROOT",
	"USERPROFILE", "TMP", "TEMP", "LOCALAPPDATA", "APPDATA",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR",
	// net/http refuses to use HTTP_PROXY while REQUEST_METHOD is set (CGI).
	"REQUEST_METHOD",
}

var (
	crashMonitorMu sync.Mutex
	activeMonitor  atomic.Pointer[crashMonitor]
)

type crashMonitor struct {
	control *os.File
}

type monitorHello struct {
	PID      int    `json:"pid"`
	Session  string `json:"session"`
	Start    int64  `json:"start"`
	Endpoint string `json:"endpoint"`
	Token    string `json:"token,omitempty"`
}

// MonitorCrashes reports the crashes that recover cannot catch: unrecovered
// panics in any goroutine, runtime fatal errors and fatal signals. Call it
// first thing in main, with the application's Config.
//
// MonitorCrashes re-runs the executable, with the same arguments, as a
// monitor process that receives the runtime's crash output. Package
// initialization and the code in main before the call run in the monitor
// too, and there the call never returns. The monitor gets only the
// BACKTRACE_* environment variables and a few system and proxy variables,
// such as PATH and HTTPS_PROXY. A later debug.SetCrashOutput call stops the
// monitor.
//
// After a crash, the monitor sends one report with error.type "Crash" and
// the crash header, such as "panic: ..." or a signal line, as
// error.message. A runtime fatal error carries "fatal error" without its
// reason. The classifier is "panic", "runtime.Error" for runtime error panics,
// "fatal error" or the signal name. The report_type is "panic" for panics
// and "crash" otherwise. The report has no attributes, breadcrumbs or
// scopes added at run time, and no runtime, /proc or environment data.
// BeforeSend runs in the monitor. The monitor keeps the application's
// standard error open until the report is sent or its delivery times out.
//
// A re-panic of a panic that ReportPanic or Client.ReportPanic delivered in
// the previous 30 seconds is not reported again, when the panic value is an
// error, a fmt.Stringer, a string, a bool or an integer. On Windows, an
// exception outside Go, such as an access violation in C code, is not
// reported.
//
// MonitorCrashes returns an error, and the application continues without a
// monitor, when cfg is invalid, when a monitor is already running, when the
// application is PID 1 on Linux, or when the monitor process cannot be
// started, exits, or does not reach MonitorCrashes within 5 seconds.
func MonitorCrashes(cfg Config) error {
	if files := os.Getenv(monitorEnvVar); files != "" {
		runCrashMonitor(cfg, files)
	}
	crashMonitorMu.Lock()
	defer crashMonitorMu.Unlock()
	if activeMonitor.Load() != nil {
		return errors.New("bt: MonitorCrashes was already called")
	}
	n := cfg.normalize()
	if err := n.validate(); err != nil {
		return err
	}
	if err := checkMonitorHost(runtime.GOOS, os.Getpid()); err != nil {
		return err
	}
	m, err := startCrashMonitor(n)
	if err != nil {
		return fmt.Errorf("bt: crash monitor not started: %w", err)
	}
	activeMonitor.Store(m)
	return nil
}

func checkMonitorHost(goos string, pid int) error {
	if goos == "linux" && pid == 1 {
		return errors.New("bt: crash monitor not started: the application is PID 1, and the kernel kills the monitor when PID 1 exits")
	}
	return nil
}

func startCrashMonitor(cfg Config) (*crashMonitor, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	crashR, crashW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer crashR.Close()
	defer crashW.Close()
	controlR, controlW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer controlR.Close()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		controlW.Close()
		return nil, err
	}
	defer readyR.Close()

	args := os.Args
	if len(args) == 0 {
		args = []string{exe}
	}
	cmd := exec.Command(exe, args[1:]...)
	cmd.Args[0] = args[0]
	cmd.Stdin = crashR
	cmd.Stderr = os.Stderr
	files, err := passFiles(cmd, controlR, readyW)
	if err == nil {
		cmd.Env = monitorEnv(os.Environ(), files)
		err = cmd.Start()
	}
	readyW.Close()
	if err != nil {
		controlW.Close()
		return nil, err
	}
	d := diag{logger: cfg.Logger, debug: cfg.Debug}
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		exited <- err
		d.logf("crash monitor exited: %v", err)
	}()
	err = awaitMonitor(readyR, exited, monitorStartTimeout)
	if err == nil {
		err = debug.SetCrashOutput(crashW, debug.CrashOptions{})
	}
	if err != nil {
		_ = cmd.Process.Kill()
		controlW.Close()
		return nil, err
	}
	m := &crashMonitor{control: controlW}
	m.send(helloLine(cfg))
	return m, nil
}

func awaitMonitor(ready io.Reader, exited <-chan error, timeout time.Duration) error {
	readyc := make(chan struct{})
	go func() {
		var b [1]byte
		if n, _ := ready.Read(b[:]); n == 1 {
			close(readyc)
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-readyc:
		return nil
	case err := <-exited:
		if err == nil {
			err = errors.New("exit status 0")
		}
		return fmt.Errorf("monitor process exited before reaching MonitorCrashes: %w", err)
	case <-timer.C:
		return fmt.Errorf("monitor process did not reach MonitorCrashes within %v", timeout)
	}
}

func monitorEnv(environ []string, files string) []string {
	env := []string{monitorEnvVar + "=" + files}
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		name = strings.ToUpper(name)
		if name != monitorEnvVar && (strings.HasPrefix(name, "BACKTRACE_") || slices.Contains(monitorEnvNames, name)) {
			env = append(env, kv)
		}
	}
	return env
}

func helloLine(cfg Config) []byte {
	data, _ := json.Marshal(monitorHello{
		PID:      os.Getpid(),
		Session:  processSessionID,
		Start:    processStart.UnixMilli(),
		Endpoint: cfg.Endpoint,
		Token:    cfg.Token,
	})
	return append(data, '\n')
}

// A stalled monitor must not hold up a panicking goroutine.
func (m *crashMonitor) send(line []byte) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.control.Write(line)
	}()
	timer := time.NewTimer(controlWriteTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func markDeliveredPanic(v any) {
	m := activeMonitor.Load()
	if m == nil {
		return
	}
	if text, ok := panicText(v); ok {
		m.send([]byte(panicMark(currentGoroutineID(), text) + "\n"))
	}
}

func currentGoroutineID() string {
	var buf [64]byte
	header, _, _ := strings.Cut(string(buf[:runtime.Stack(buf[:], false)]), "\n")
	return goroutineID(header)
}

func goroutineID(header string) string {
	id, _, _ := strings.Cut(strings.TrimPrefix(header, "goroutine "), " ")
	return id
}

// Must match runtime.printpanicval. It prints other kinds as an address.
func panicText(v any) (text string, ok bool) {
	defer func() {
		if recover() != nil {
			text, ok = "", false
		}
	}()
	switch x := v.(type) {
	case error:
		return x.Error(), true
	case fmt.Stringer:
		return x.String(), true
	}
	rv := reflect.ValueOf(v)
	t := rv.Type()
	named := t.PkgPath() != ""
	switch rv.Kind() {
	case reflect.String:
		if named {
			return t.String() + `("` + rv.String() + `")`, true
		}
		return rv.String(), true
	case reflect.Complex64, reflect.Complex128:
		text = strconv.FormatComplex(rv.Complex(), 'g', -1, t.Bits())
		if named {
			return t.String() + text, true
		}
		return text, true
	case reflect.Bool:
		text = strconv.FormatBool(rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		text = strconv.FormatInt(rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		text = strconv.FormatUint(rv.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		text = strconv.FormatFloat(rv.Float(), 'g', -1, t.Bits())
	default:
		return "", false
	}
	if named {
		return t.String() + "(" + text + ")", true
	}
	return text, true
}

func panicMark(goroutine, text string) string {
	sum := sha256.Sum256([]byte(goroutine + " " + text))
	return hex.EncodeToString(sum[:8])
}
