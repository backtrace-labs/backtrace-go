package bt

import "fmt"
import "testing"
import "errors"
import "net"
import "net/http"
import "io/ioutil"
import "encoding/json"

func TestEverything(t *testing.T) {
	var err error
	addr := net.TCPAddr{
		IP: []byte{127, 0, 0, 1},
	}
	listener, err := net.ListenTCP("tcp4", &addr)
	if err != nil {
		panic(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	Options.Endpoint = fmt.Sprintf("http://127.0.0.1:%d", port)
	Options.Token = "fake token"
	Options.CaptureAllGoroutines = true
	//Options.DebugBacktrace = true
	Options.ContextLineCount = 2

	go causeErrorReport()

	handler := myHandler{
		listener: listener,
	}
	err = http.Serve(listener, handler)
}

type myHandler struct {
	listener *net.TCPListener
}

func (h myHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer h.listener.Close()

	var err error
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		panic(err)
	}
	report := map[string]interface{}{}
	err = json.Unmarshal(body, &report)
	if err != nil {
		panic(err)
	}
	if report["lang"] != "go" {
		panic("bad lang")
	}
	attributes := report["attributes"].(map[string]interface{})
	if attributes["error.message"] != "it broke" {
		panic("bad error message")
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "OK\n")
}

func doSomething(ch chan int) {
	_ = <-ch
}

func causeErrorReport() {
	go doSomething(make(chan int))
	SendReport(errors.New("it broke"), nil)
	FinishSendingReports()
}
