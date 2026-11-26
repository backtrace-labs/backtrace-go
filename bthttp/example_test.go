package bthttp_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
	"github.com/backtrace-labs/backtrace-go/bthttp"
	"github.com/backtrace-labs/backtrace-go/internal/fakebacktrace"
)

func ExampleHandler_Handle() {
	backtrace := fakebacktrace.Start()
	defer backtrace.Close()
	client, err := bt.NewClient(bt.Config{Endpoint: backtrace.Endpoint})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer client.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		bt.ScopeFromContext(r.Context()).SetAttribute("order.id", r.PathValue("id"))
		panic("order store unavailable")
	})
	middleware := bthttp.New(bthttp.Options{Client: client})
	server := httptest.NewServer(middleware.Handle(mux))
	defer server.Close()

	resp, err := http.Get(server.URL + "/orders/1001")
	if err != nil {
		fmt.Println(err)
		return
	}
	resp.Body.Close()
	fmt.Println(resp.Status)
	client.Flush(5 * time.Second)

	for _, r := range backtrace.Reports() {
		fmt.Println(r.Attributes["error.message"])
		fmt.Println(r.Attributes["request.route"], r.Attributes["order.id"])
	}
	// Output:
	// 500 Internal Server Error
	// order store unavailable
	// GET /orders/{id} 1001
}

func ExampleOptions_reportStatus() {
	backtrace := fakebacktrace.Start()
	defer backtrace.Close()
	client, err := bt.NewClient(bt.Config{Endpoint: backtrace.Endpoint})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer client.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /rates", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate provider timed out", http.StatusGatewayTimeout)
	})
	middleware := bthttp.New(bthttp.Options{
		Client: client,
		ReportStatus: func(status int, r *http.Request) bool {
			return status >= 500
		},
	})
	server := httptest.NewServer(middleware.Handle(mux))
	defer server.Close()

	resp, err := http.Get(server.URL + "/rates")
	if err != nil {
		fmt.Println(err)
		return
	}
	resp.Body.Close()
	client.Flush(5 * time.Second)

	for _, r := range backtrace.Reports() {
		fmt.Println(r.Attributes["error.message"])
		fmt.Println(r.Attributes["error.type"], r.Attributes["request.route"], r.Attributes["response.status"])
	}
	// Output:
	// HTTP 504 Gateway Timeout
	// Message GET /rates 504
}

func ExampleNewTransport() {
	backtrace := fakebacktrace.Start()
	defer backtrace.Close()
	client, err := bt.NewClient(bt.Config{Endpoint: backtrace.Endpoint})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer client.Close()

	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "out of stock", http.StatusConflict)
	}))
	defer inventory.Close()
	outbound := &http.Client{Transport: bthttp.NewTransport(nil, bthttp.TransportOptions{})}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /checkout", func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, inventory.URL+"/reserve", nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp, err := outbound.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			client.ReportContext(r.Context(), fmt.Errorf("reserve stock: %s", resp.Status), nil)
			http.Error(w, "checkout failed", http.StatusConflict)
		}
	})
	server := httptest.NewServer(bthttp.New(bthttp.Options{Client: client}).Handle(mux))
	defer server.Close()

	resp, err := http.Post(server.URL+"/checkout", "text/plain", nil)
	if err != nil {
		fmt.Println(err)
		return
	}
	resp.Body.Close()
	client.Flush(5 * time.Second)

	for _, r := range backtrace.Reports() {
		fmt.Println(r.Attributes["error.message"])
		for _, b := range r.Breadcrumbs {
			fmt.Println(b.Type, b.Level, b.Attributes["method"], b.Attributes["status"])
		}
	}
	// Output:
	// reserve stock: 409 Conflict
	// http info POST 409
}
