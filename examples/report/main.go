// Command report demonstrates the error reporting API: the modern Client,
// panic capture, breadcrumbs, runtime attributes, and the net/http
// middleware.
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
	"github.com/backtrace-labs/backtrace-go/bthttp"
)

func main() {
	client, err := bt.NewClient(bt.Config{
		// Or set BACKTRACE_ENDPOINT / BACKTRACE_TOKEN in the environment.
		Endpoint: os.Getenv("BACKTRACE_ENDPOINT"), // e.g. https://submit.backtrace.io/{universe}/{token}/json
		Attributes: map[string]interface{}{
			"application.environment": "development",
		},
		// Scrub or drop reports before submission:
		BeforeSend: func(r *bt.ReportData) *bt.ReportData {
			delete(r.Attributes, "internal.hostname.alias")
			return r
		},
		Debug: true,
	})
	if err != nil {
		log.Fatalf("backtrace: %v", err)
	}
	defer client.Close()

	// Attributes and breadcrumbs may be added at any time, from any goroutine.
	client.SetAttribute("app.version", "1.2.3")
	client.AddBreadcrumb(bt.Breadcrumb{Message: "service starting", Level: bt.BreadcrumbInfo})

	// Report an error with per-report attributes.
	if _, err := os.Open("/does/not/exist"); err != nil {
		client.Report(fmt.Errorf("startup check failed: %w", err), map[string]interface{}{
			"check": "filesystem",
		})
	}

	// Capture panics in HTTP handlers with request attributes attached.
	handler := bthttp.New(bthttp.Options{
		Client:          client,
		Repanic:         false,
		WaitForDelivery: true,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		panic(errors.New("handler exploded"))
	})
	_ = handler.Handle(mux) // pass to http.ListenAndServe in a real service

	// Wait for queued reports before exiting.
	if !client.Flush(5 * time.Second) {
		log.Print("backtrace: flush timed out; some reports may be dropped")
	}
}
