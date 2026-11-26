package bt_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
	"github.com/backtrace-labs/backtrace-go/internal/fakebacktrace"
)

func ExampleNewClient() {
	backtrace := fakebacktrace.Start()
	defer backtrace.Close()

	client, err := bt.NewClient(bt.Config{
		Endpoint: backtrace.Endpoint, // https://submit.backtrace.io/{universe}/{token}/json
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer client.Close()

	_, err = strconv.Atoi("twelve")
	client.Report(fmt.Errorf("parse quantity: %w", err), map[string]any{
		"order.id": "o-1001",
	})
	client.Flush(5 * time.Second)

	for _, r := range backtrace.Reports() {
		fmt.Println(r.Attributes["error.message"])
		fmt.Println(r.Attributes["error.type"], r.Classifiers, r.Attributes["order.id"])
	}
	// Output:
	// parse quantity: strconv.Atoi: parsing "twelve": invalid syntax
	// Exception [*strconv.NumError] o-1001
}

func ExampleWithScope() {
	backtrace := fakebacktrace.Start()
	defer backtrace.Close()
	client, err := bt.NewClient(bt.Config{Endpoint: backtrace.Endpoint})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer client.Close()

	ctx := bt.WithScope(context.Background())
	scope := bt.ScopeFromContext(ctx)
	scope.SetAttribute("user.id", "u-42")
	scope.AddBreadcrumb(bt.Breadcrumb{Message: "checkout started", Type: bt.BreadcrumbTypeUser})

	client.ReportContext(ctx, errors.New("card declined"), nil)
	client.Report(errors.New("cache miss"), nil)
	client.Flush(5 * time.Second)

	for _, r := range backtrace.Reports() {
		fmt.Println(r.Attributes["error.message"], r.Attributes["user.id"], r.BreadcrumbMessages())
	}
	// Output:
	// card declined u-42 [checkout started]
	// cache miss <nil> []
}

func ExampleConfig_beforeSend() {
	backtrace := fakebacktrace.Start()
	defer backtrace.Close()
	client, err := bt.NewClient(bt.Config{
		Endpoint: backtrace.Endpoint,
		BeforeSend: func(r *bt.ReportData) *bt.ReportData {
			if errors.Is(r.Error, context.Canceled) {
				return nil
			}
			delete(r.Attributes, "hostname")
			r.Attributes["team"] = "payments"
			return r
		},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer client.Close()

	client.Report(fmt.Errorf("fetch rates: %w", context.Canceled), nil)
	client.Report(errors.New("rates unavailable"), nil)
	client.Flush(5 * time.Second)

	for _, r := range backtrace.Reports() {
		fmt.Println(r.Attributes["error.message"], r.Attributes["team"], r.Attributes["hostname"])
	}
	fmt.Println("dropped by BeforeSend:", client.Stats().BeforeSend)
	// Output:
	// rates unavailable payments <nil>
	// dropped by BeforeSend: 1
}

func ExampleClient_ReportAndRecoverPanic() {
	backtrace := fakebacktrace.Start()
	defer backtrace.Close()
	client, err := bt.NewClient(bt.Config{Endpoint: backtrace.Endpoint})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer client.Close()

	resize := func(format string) {
		defer client.ReportAndRecoverPanic(map[string]any{"job": "resize"})
		panic("unsupported image format " + format)
	}
	resize("tiff")
	fmt.Println("worker still running")
	client.Flush(5 * time.Second)

	for _, r := range backtrace.Reports() {
		fmt.Println(r.Attributes["error.message"])
		fmt.Println(r.Attributes["error.type"], r.Classifiers, r.Attributes["report_type"], r.Attributes["job"])
	}
	// Output:
	// worker still running
	// unsupported image format tiff
	// Unhandled exception [panic] panic resize
}

func ExampleClient_ReportPanicValueAndFlush() {
	backtrace := fakebacktrace.Start()
	defer backtrace.Close()
	client, err := bt.NewClient(bt.Config{Endpoint: backtrace.Endpoint})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer client.Close()

	settle := func() {
		defer func() {
			if v := recover(); v != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				fmt.Println("processed:", client.ReportPanicValueAndFlush(ctx, v, nil))
			}
		}()
		panic(errors.New("ledger out of balance"))
	}
	settle()
	fmt.Println("delivered:", client.Stats().Delivered)

	for _, r := range backtrace.Reports() {
		fmt.Println(r.Attributes["error.message"], r.Attributes["error.type"])
	}
	// Output:
	// processed: true
	// delivered: 1
	// ledger out of balance Unhandled exception
}

func ExampleSetDefaultClient() {
	backtrace := fakebacktrace.Start()
	defer backtrace.Close()
	client, err := bt.NewClient(bt.Config{Endpoint: backtrace.Endpoint})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer client.Close()

	bt.SetDefaultClient(client)
	defer bt.SetDefaultClient(nil)

	bt.SetAttribute("deployment", "blue")
	bt.Report(errors.New("legacy call site"), nil)
	bt.Flush(5 * time.Second)

	for _, r := range backtrace.Reports() {
		fmt.Println(r.Attributes["error.message"], r.Attributes["deployment"])
	}
	// Output:
	// legacy call site blue
}
