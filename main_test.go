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

func TestPanic(t *testing.T) {
	var err error
	addr := net.TCPAddr{
		IP: []byte{127, 0, 0, 1},
	}
	listener, err := net.ListenTCP("tcp4", &addr)
	if err != nil {
		panic(err)
	}
	go func() {
		handler := myHandler{
			listener: listener,
		}
		err = http.Serve(listener, handler)
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	Options.Endpoint = fmt.Sprintf("http://127.0.0.1:%d", port)
	Options.Token = "fake token"
	Options.CaptureAllGoroutines = true
	//Options.DebugBacktrace = true
	Options.ContextLineCount = 2
	count := 0
	for i := 0; i < 5; i++ {
		defer func() {
			recover()
			count++
		}()
		defer ReportPanic(nil)
		// fire off a panic. this should happen 5 times
		panic("boom")
	}
	if count != 5 {
		// really this doesn't do much, since it won't be hit if the code above deadlocks
		t.Fatal("Expected 5 panics")
	}
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
	Report(errors.New("it broke"), nil)
	FinishSendingReports()
}
