package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Bind every listener before accepting traffic. A failed private bind must not
// leave a public process running without its required authority boundary.
func serveHTTPServers(ctx context.Context, servers ...*http.Server) error {
	listeners := make([]net.Listener, 0, len(servers))
	for _, server := range servers {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			for _, prior := range listeners {
				_ = prior.Close()
			}
			return fmt.Errorf("billing HTTP listener unavailable: %w", err)
		}
		listeners = append(listeners, listener)
	}
	errCh := make(chan error, len(servers))
	var workers sync.WaitGroup
	for i, server := range servers {
		workers.Add(1)
		go func(server *http.Server, listener net.Listener) {
			defer workers.Done()
			errCh <- server.Serve(listener)
		}(server, listeners[i])
	}
	var result error
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			if result == nil {
				result = err
			}
		}
	}
	workers.Wait()
	return result
}
