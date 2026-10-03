package main

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestHTTPServersCancelShutsDownBothListeners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 2)
	servers := []*http.Server{{Addr: "127.0.0.1:0"}, {Addr: "127.0.0.1:0"}}
	for _, server := range servers {
		// BaseContext runs once per Serve, even without a connection.
		server.BaseContext = func(_ net.Listener) context.Context {
			started <- struct{}{}
			return context.Background()
		}
	}
	result := make(chan error, 1)
	go func() { result <- serveHTTPServers(ctx, servers...) }()
	for range servers {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("both listeners did not start")
		}
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener shutdown did not finish")
	}
	for _, server := range servers {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			t.Fatalf("server not shut down: %v", err)
		}
	}
}

func TestHTTPServersBindFailureStartsNeitherListener(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	var mu sync.Mutex
	started := 0
	public := &http.Server{Addr: "127.0.0.1:0", BaseContext: func(net.Listener) context.Context {
		mu.Lock()
		started++
		mu.Unlock()
		return context.Background()
	}}
	if err := serveHTTPServers(context.Background(), public, &http.Server{Addr: occupied.Addr().String()}); err == nil {
		t.Fatal("private bind failure ignored")
	}
	mu.Lock()
	defer mu.Unlock()
	if started != 0 {
		t.Fatal("public listener started after required private bind failed")
	}
}
