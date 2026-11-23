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
