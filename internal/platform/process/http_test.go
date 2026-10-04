package process_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/process"
)

func TestHTTPShutdownWaitsForHandlerAfterDrainTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	server := &http.Server{Addr: address, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(canceled)
		<-release
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- process.ServeHTTP(ctx, server, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	go func() {
		client := &http.Client{Timeout: 15 * time.Second}
		for range 100 {
			response, err := client.Get("http://" + address)
			if err == nil {
				response.Body.Close()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP request not started")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(8 * time.Second):
		t.Fatal("timed out drain did not cancel request")
	}
	select {
	case <-done:
		t.Fatal("HTTP runtime returned before request cleanup")
	default:
	}
	finish()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("HTTP runtime did not finish")
	}
}
