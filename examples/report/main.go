// Command report demonstrates the error reporting API: the modern Client,
// request scopes, panic capture, breadcrumbs, runtime attributes, the
// net/http middleware, and the log/slog handler.
//
// It sends reports to BACKTRACE_ENDPOINT (and BACKTRACE_TOKEN) when set.
// Otherwise it starts a local server that prints each report it receives.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
	"github.com/backtrace-labs/backtrace-go/bthttp"
	"github.com/backtrace-labs/backtrace-go/btslog"
	"github.com/backtrace-labs/backtrace-go/internal/fakebacktrace"
)

func main() {
	endpoint := os.Getenv("BACKTRACE_ENDPOINT")
	if endpoint == "" {
		local := httptest.NewServer(http.HandlerFunc(printReport))
		defer local.Close()
		endpoint = local.URL + "/example-universe/example-token/json"
	}

	client, err := bt.NewClient(bt.Config{
		// Or set BACKTRACE_ENDPOINT / BACKTRACE_TOKEN in the environment.
		Endpoint: endpoint, // e.g. https://submit.backtrace.io/{universe}/{token}/json
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

	ctx := bt.WithScope(context.Background())
	scope := bt.ScopeFromContext(ctx)
	scope.SetAttribute("request.id", "req-1")
	scope.AddBreadcrumb(bt.Breadcrumb{Message: "loading cart", Type: bt.BreadcrumbTypeUser})
	client.ReportContext(ctx, errors.New("cart lookup failed"), nil)

	// Report an error with per-report attributes.
	if _, err := os.Open("/does/not/exist"); err != nil {
		client.Report(fmt.Errorf("startup check failed: %w", err), map[string]interface{}{
			"check": "filesystem",
		})
	}

	logger := slog.New(btslog.NewHandler(slog.NewTextHandler(os.Stdout, nil), btslog.Options{Client: client}))
	logger.InfoContext(ctx, "charging card", "order", 1001)
	logger.ErrorContext(ctx, "charge failed", "order", 1001, "err", errors.New("card declined"))

	// Capture panics in HTTP handlers with request attributes attached.
	handler := bthttp.New(bthttp.Options{
		Client:          client,
		Repanic:         false,
		WaitForDelivery: true,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		bt.ScopeFromContext(r.Context()).SetAttribute("user.id", "u-42")
		panic(errors.New("handler exploded"))
	})
	wrapped := handler.Handle(mux) // pass to http.ListenAndServe in a real service
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	fmt.Println("middleware answered", rec.Code)

	// Wait for queued reports before exiting.
	if !client.Flush(5 * time.Second) {
		log.Print("backtrace: flush timed out; some reports may be dropped")
	}
	stats := client.Stats()
	fmt.Printf("delivered %d of %d reports\n", stats.Delivered, stats.Accepted)
}

func printReport(w http.ResponseWriter, r *http.Request) {
	report, err := fakebacktrace.Decode(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fmt.Printf("report: %v (error.type %v, classifiers %v)\n",
		report.Attributes["error.message"], report.Attributes["error.type"], report.Classifiers)
	_, _ = io.WriteString(w, `{"response":"ok"}`)
}
