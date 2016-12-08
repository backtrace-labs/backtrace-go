package bt

import "testing"
import "errors"

func TestEverything(t *testing.T) {
	Options.Endpoint = "http://127.0.0.1:1234"
	Options.Token = "fake token"
	Options.CaptureAllGoroutines = true
	go doSomething(make(chan int))
	SendReport(errors.New("it broke"), nil)
}

func doSomething(ch chan int) {
	_ = <-ch
}
