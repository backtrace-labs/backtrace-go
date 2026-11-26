package btslog_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
	"github.com/backtrace-labs/backtrace-go/btslog"
	"github.com/backtrace-labs/backtrace-go/internal/fakebacktrace"
)

func ExampleNewHandler() {
	backtrace := fakebacktrace.Start()
	defer backtrace.Close()
	client, err := bt.NewClient(bt.Config{Endpoint: backtrace.Endpoint})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer client.Close()

	text := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.Attr{}
			}
			return a
		},
	})
	logger := slog.New(btslog.NewHandler(text, btslog.Options{Client: client}))

	ctx := bt.WithScope(context.Background())
	logger.InfoContext(ctx, "charging card", "order", 1001)
	logger.ErrorContext(ctx, "charge failed", "order", 1001, "err", errors.New("card declined"))
	client.Flush(5 * time.Second)

	for _, r := range backtrace.Reports() {
		fmt.Println("error.message:", r.Attributes["error.message"])
		fmt.Println("log.message:", r.Attributes["log.message"])
		fmt.Println("order:", r.Attributes["order"])
		for _, b := range r.Breadcrumbs {
			fmt.Println("breadcrumb:", b.Type, b.Level, b.Message)
		}
	}
	// Output:
	// level=INFO msg="charging card" order=1001
	// level=ERROR msg="charge failed" order=1001 err="card declined"
	// error.message: card declined
	// log.message: charge failed
	// order: 1001
	// breadcrumb: log info charging card
}
