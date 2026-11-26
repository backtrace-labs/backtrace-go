package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestMainOffline(t *testing.T) {
	t.Setenv("BACKTRACE_ENDPOINT", "")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	output := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		output <- string(data)
	}()

	stdout := os.Stdout
	os.Stdout = w
	func() {
		defer func() { os.Stdout = stdout }()
		main()
	}()
	w.Close()
	got := <-output

	for _, want := range []string{
		"middleware answered 500",
		"report: handler exploded (error.type Unhandled exception",
		"delivered 4 of 4 reports",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "report: "); n != 4 {
		t.Errorf("local server printed %d reports, want 4:\n%s", n, got)
	}
}
