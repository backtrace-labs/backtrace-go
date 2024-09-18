package bt

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	mathrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

const VersionMajor = 0
const VersionMinor = 0
const VersionPatch = 0

var Version = fmt.Sprintf("%d.%d.%d", VersionMajor, VersionMinor, VersionPatch)

type OptionsStruct struct {
	Endpoint string
	Token    string
	// SendEnvVars gathers and sends all environment variables with every report if true. Default false.
	SendEnvVars bool

	CaptureAllGoroutines bool
	TabWidth             int
	ContextLineCount     int
	Attributes           map[string]interface{}
	DebugBacktrace       bool
}

var Options OptionsStruct

var rng *mathrand.Rand

type reportPayload struct {
	stack       []byte
	attributes  map[string]interface{}
	annotations map[string]interface{}
	timestamp   int64
	classifier  string
}

var queue = make(chan interface{}, 50)
var done_chan = make(chan struct{})
var block_chan = make(chan struct{})

func init() {
	var err error

	var seed_bytes [8]byte
	_, err = cryptorand.Read(seed_bytes[:])
	if err != nil {
		panic(err)
	}

	seed :=
		(int64(seed_bytes[0]) << 0) |
			(int64(seed_bytes[1]) << 1) |
			(int64(seed_bytes[2]) << 2) |
			(int64(seed_bytes[3]) << 3) |
			(int64(seed_bytes[4]) << 4) |
			(int64(seed_bytes[5]) << 5) |
			(int64(seed_bytes[6]) << 6) |
			(int64(seed_bytes[7]) << 7)

	rand_source := mathrand.NewSource(seed)
	rng = mathrand.New(rand_source)

	go sendWorkerMain()
}

func Report(object interface{}, extra_attributes map[string]interface{}) {
	if extra_attributes == nil {
		extra_attributes = map[string]interface{}{}
	}
	if extra_attributes["report_type"] == nil {
		extra_attributes["report_type"] = "error"
	}
	switch value := object.(type) {
	case nil:
		return
	case error:
		sendReportString(value.Error(), "error", extra_attributes)
	default:
		sendReportString(fmt.Sprint(value), "message", extra_attributes)
	}
}

func sendReportString(msg string, classifier string, extra_attributes map[string]interface{}) {
	if !checkOptions() {
		return
	}

	timestamp := time.Now().Unix()

	attributes := map[string]interface{}{}

	for k, v := range Options.Attributes {
		attributes[k] = v
	}

	attributes["error.message"] = msg

	for k, v := range extra_attributes {
		attributes[k] = v
	}

	annotations := map[string]interface{}{}
	if Options.SendEnvVars {
		annotations["Environment Variables"] = getEnvVars()
	}

	payload := &reportPayload{
		stack:       stack(Options.CaptureAllGoroutines),
		attributes:  attributes,
		annotations: annotations,
		timestamp:   timestamp,
		classifier:  classifier,
	}
	queue <- payload
}

func ReportPanic(extra_attributes map[string]interface{}) {
	if !checkOptions() {
		return
	}

	err := recover()
	if err == nil {
		return
	}

	if extra_attributes == nil {
		extra_attributes = map[string]interface{}{}
	}
	extra_attributes["report_type"] = "panic"

	Report(err, extra_attributes)
	finishSendingReports(false)
	panic(err)
}

func ReportAndRecoverPanic(extra_attributes map[string]interface{}) {
	if !checkOptions() {
		return
	}

	if extra_attributes == nil {
		extra_attributes = map[string]interface{}{}
	}
	extra_attributes["report_type"] = "panic"

	Report(recover(), extra_attributes)
}

func stack(all bool) []byte {
	buf := make([]byte, 1024)
	for {
		n := runtime.Stack(buf, all)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

func getEnvVars() map[string]string {
	lines := os.Environ()
	result := map[string]string{}
	for _, line := range lines {
		kv := strings.Split(line, "=")
		result[kv[0]] = kv[1]
	}
	return result
}

func checkOptions() bool {
	if len(Options.Endpoint) == 0 {
		if !Options.DebugBacktrace {
			return false
		}
		panic("must set bt.Options.Endpoint")
	}
	if len(Options.Token) == 0 {
		if !Options.DebugBacktrace {
			return false
		}
		panic("must set bt.Options.Token")
	}
	return true
}

func createUuid() string {
	var uuid_bytes [16]byte
	_, _ = rng.Read(uuid_bytes[:]) // This function is documented to never fail.
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		uuid_bytes[0], uuid_bytes[1], uuid_bytes[2], uuid_bytes[3],
		uuid_bytes[4], uuid_bytes[5],
		uuid_bytes[6], uuid_bytes[7],
		uuid_bytes[8], uuid_bytes[9],
		uuid_bytes[10], uuid_bytes[11], uuid_bytes[12], uuid_bytes[13], uuid_bytes[14], uuid_bytes[15])
}

func sendWorkerMain() {
	for {
		select {
		case queue_item := <-queue:
			switch value := queue_item.(type) {
			case nil:
				done_chan <- struct{}{}
				return
			case *reportPayload:
				processAndSend(value)
			default:
				panic("invalid queue item")
			}
		case <-block_chan:
			done_chan <- struct{}{}
		}

	}
}

func FinishSendingReports() {
	finishSendingReports(true)
}
func finishSendingReports(kill bool) {
	if kill {
		queue <- nil
	} else {
		block_chan <- struct{}{}
	}
	<-done_chan
}

func processAndSend(payload *reportPayload) {
	threads, mainThread, sourceCode := ParseThreadsFromStack(payload.stack)

	report := map[string]interface{}{}
	report["uuid"] = createUuid()
	report["timestamp"] = payload.timestamp
	report["lang"] = "go"
	report["langVersion"] = runtime.Version()
	report["agent"] = "backtrace-go"
	report["agentVersion"] = Version
	report["attributes"] = payload.attributes
	report["annotations"] = payload.annotations
	report["threads"] = threads
	report["mainThread"] = mainThread
	report["sourceCode"] = sourceCode
	report["classifiers"] = []string{payload.classifier}

	full_url := fmt.Sprintf("%s/post?format=json&token=%s", Options.Endpoint, url.QueryEscape(Options.Token))
	if Options.DebugBacktrace {
		fmt.Fprintf(os.Stderr, "POST %s\n", full_url)
		var err error
		json_bytes, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(os.Stderr, "%s\n", string(json_bytes))
	}

	json_bytes, err := json.Marshal(report)
	if err != nil {
		if Options.DebugBacktrace {
			panic(err)
		}
		return
	}
	resp, err := http.Post(full_url, "application/json", bytes.NewReader(json_bytes))
	if err != nil {
		if Options.DebugBacktrace {
			panic(err)
		}
		return
	}
	defer resp.Body.Close()

	if _, err = io.ReadAll(resp.Body); err != nil {
		if Options.DebugBacktrace {
			panic(err)
		}
		return
	}
}
