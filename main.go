package bt

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	mathrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	VersionMajor = 1
	VersionMinor = 0
	VersionPatch = 0
)

var (
	Version = fmt.Sprintf("%d.%d.%d", VersionMajor, VersionMinor, VersionPatch)

	windowsGUIDCommand = []string{"reg", "query", "\"HKEY_LOCAL_MACHINE\\Software\\Microsoft\\Cryptography\"", "/v", "MachineGuid"}
	linuxGUIDCommand   = []string{"sh", "-c", "( cat /var/lib/dbus/machine-id /etc/machine-id 2> /dev/null || hostname ) | head -n 1 || :"}
	freebsdGUIDCommand = []string{"sh", "-c", "kenv -q smbios.system.uuid || sysctl -n kern.hostuuid"}
	darwinGUIDCommand  = []string{"sh", "-c", "ioreg -rd1 -c IOPlatformExpertDevice | grep IOPlatformUUID | awk -F'= \"' '{print $2}' | tr -d '\"' | tr -d '\n'"}

	windowsCPUCommand = []string{"wmic", "CPU", "get", "NAME"}
	linuxCPUCommand   = []string{"sh", "-c", "lscpu | grep \"Model name\" | awk -F':' '{print $2}' | sed 's/^[[:space:]]*//'"}
	darwinCPUCommand  = []string{"sh", "-c", "sysctl -n machdep.cpu.brand_string | tr -d '\n'"}
	freebsdCPUCommand = []string{"sh", "-c", "sysctl -n hw.model"}

	linuxOSVersionCommand   = []string{"sh", "-c", "cat /etc/os-release | grep VERSION= | awk -F'=\"' '{print $2}' | tr -d '\"'"}
	darwinOSVersionCommand  = []string{"sh", "-c", "sw_vers | grep ProductVersion | awk -F':' '{print $2}' | tr -d '\t' | tr -d '\n'"}
	freebsdOSVersionCommand = []string{"sh", "-c", "cat /etc/os-release | grep VERSION= | awk -F'=\"' '{print $2}' | tr -d '\"'"}
)

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
var doneChan = make(chan struct{})
var blockChan = make(chan struct{})

func init() {
	var err error

	var seedBytes [8]byte
	_, err = cryptorand.Read(seedBytes[:])
	if err != nil {
		panic(err)
	}

	seed :=
		(int64(seedBytes[0]) << 0) |
			(int64(seedBytes[1]) << 1) |
			(int64(seedBytes[2]) << 2) |
			(int64(seedBytes[3]) << 3) |
			(int64(seedBytes[4]) << 4) |
			(int64(seedBytes[5]) << 5) |
			(int64(seedBytes[6]) << 6) |
			(int64(seedBytes[7]) << 7)

	randSource := mathrand.NewSource(seed)
	rng = mathrand.New(randSource)

	setDefaultAttributes()

	go sendWorkerMain()
}

func setDefaultAttributes() {
	if Options.Attributes == nil {
		Options.Attributes = make(map[string]interface{})
	}

	hostName, _ := os.Hostname()
	Options.Attributes["backtrace.version"] = Version
	Options.Attributes["backtrace.agent"] = "backtrace-go"
	Options.Attributes["hostname"] = hostName
	Options.Attributes["uname.sysname"] = runtime.GOOS
	Options.Attributes["cpu.arch"] = runtime.GOARCH
	Options.Attributes["process.id"] = os.Getpid()
	Options.Attributes["application.session"] = uuid.New()
	Options.Attributes["application"] = filepath.Base(os.Args[0])

	guiCommand := []string{}
	cpuCommand := []string{}
	osCommand := []string{}
	switch runtime.GOOS {
	case "windows":
		guiCommand = windowsGUIDCommand
		cpuCommand = windowsCPUCommand
	case "linux":
		guiCommand = linuxGUIDCommand
		cpuCommand = linuxCPUCommand
		osCommand = linuxOSVersionCommand
	case "darwin":
		guiCommand = darwinGUIDCommand
		cpuCommand = darwinCPUCommand
		osCommand = darwinOSVersionCommand
	case "freebsd":
		guiCommand = freebsdGUIDCommand
		cpuCommand = freebsdCPUCommand
		osCommand = freebsdOSVersionCommand
	}

	if len(guiCommand) > 0 {
		if output := execCommand(guiCommand); output != "" {
			if runtime.GOOS == "windows" {
				// windows gives:
				// HKEY_LOCAL_MACHINE\Software\Microsoft\Cryptography
				//    MachineGuid    REG_SZ    {XXXX-XXXX-XXXX-XXXX-XXXX}
				if splitOutput := strings.Split(output, "{"); len(splitOutput) > 1 {
					output = strings.TrimSuffix(splitOutput[1], "}")
				}
			}

			Options.Attributes["guid"] = output
		}
	}

	if len(cpuCommand) > 0 {
		if output := execCommand(cpuCommand); output != "" {
			if runtime.GOOS == "windows" {
				// windows gives:
				//NAME
				//Intel(R) Core(TM) i7-9700K CPU @ 3.60GHz
				if splitOutput := strings.Split(output, "\n"); len(splitOutput) > 1 {
					output = splitOutput[1]
				}
			}

			Options.Attributes["cpu.brand"] = output
		}
	}

	if len(osCommand) > 0 {
		if output := execCommand(osCommand); output != "" {
			Options.Attributes["uname.version"] = output
		}
	}
}

// first value in array is command to exec, rest are arguments.
// e.g. []string{"sh", "-c", "sysctl -n foo_bar | grep foo_bar | tr -d "foo_var" "}
func execCommand(commands []string) string {
	out, err := exec.Command(commands[0], commands[1:]...).Output()
	if err != nil {
		if Options.DebugBacktrace {
			log.Println(err)
		}
	}

	return string(out)
}

func Report(object interface{}, extraAttributes map[string]interface{}) {
	if extraAttributes == nil {
		extraAttributes = map[string]interface{}{}
	}
	if extraAttributes["report_type"] == nil {
		extraAttributes["report_type"] = "error"
	}
	switch value := object.(type) {
	case nil:
		return
	case error:
		sendReportString(value.Error(), "error", extraAttributes)
	default:
		sendReportString(fmt.Sprint(value), "message", extraAttributes)
	}
}

func sendReportString(msg string, classifier string, extraAttributes map[string]interface{}) {
	if !checkOptions() {
		return
	}

	timestamp := time.Now().Unix()

	attributes := map[string]interface{}{}

	for k, v := range Options.Attributes {
		attributes[k] = v
	}

	attributes["error.message"] = msg

	for k, v := range extraAttributes {
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

func ReportPanic(extraAttributes map[string]interface{}) {
	if !checkOptions() {
		return
	}

	err := recover()
	if err == nil {
		return
	}

	if extraAttributes == nil {
		extraAttributes = map[string]interface{}{}
	}
	extraAttributes["report_type"] = "panic"

	Report(err, extraAttributes)
	finishSendingReports(false)
	panic(err)
}

func ReportAndRecoverPanic(extraAttributes map[string]interface{}) {
	if !checkOptions() {
		return
	}

	if extraAttributes == nil {
		extraAttributes = map[string]interface{}{}
	}
	extraAttributes["report_type"] = "panic"

	Report(recover(), extraAttributes)
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

	if !strings.HasPrefix(Options.Endpoint, "https://submit.backtrace.io") {
		if len(Options.Token) == 0 {
			if !Options.DebugBacktrace {
				return false
			}
			panic("must set bt.Options.Token")
		}
	}
	return true
}

func createUuid() string {
	var uuidBytes [16]byte
	_, _ = rng.Read(uuidBytes[:]) // This function is documented to never fail.
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		uuidBytes[0], uuidBytes[1], uuidBytes[2], uuidBytes[3],
		uuidBytes[4], uuidBytes[5],
		uuidBytes[6], uuidBytes[7],
		uuidBytes[8], uuidBytes[9],
		uuidBytes[10], uuidBytes[11], uuidBytes[12], uuidBytes[13], uuidBytes[14], uuidBytes[15])
}

func sendWorkerMain() {
	for {
		select {
		case queueItem := <-queue:
			switch value := queueItem.(type) {
			case nil:
				doneChan <- struct{}{}
				return
			case *reportPayload:
				processAndSend(value)
			default:
				panic("invalid queue item")
			}
		case <-blockChan:
			doneChan <- struct{}{}
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
		blockChan <- struct{}{}
	}
	<-doneChan
}

func processAndSend(payload *reportPayload) {
	threads, sourceCode := ParseThreadsFromStack(payload.stack)

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
	report["mainThread"] = "0"
	report["sourceCode"] = sourceCode
	report["classifiers"] = []string{payload.classifier}

	if runtime.GOOS == "linux" {
		readMemProcInfo()
	}

	fullUrl := Options.Endpoint

	if len(Options.Token) != 0 { // if token is set that means its old URL.
		fullUrl = fmt.Sprintf("%s/post?format=json&token=%s", Options.Endpoint, url.QueryEscape(Options.Token))
	}

	if Options.DebugBacktrace {
		fmt.Fprintf(os.Stderr, "POST %s\n", fullUrl)
		var err error
		jsonBytes, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(os.Stderr, "%s\n", string(jsonBytes))
	}

	jsonBytes, err := json.Marshal(report)
	if err != nil {
		if Options.DebugBacktrace {
			panic(err)
		}
		return
	}
	resp, err := http.Post(fullUrl, "application/json", bytes.NewReader(jsonBytes))
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
