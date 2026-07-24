package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	webSocketRelayReadHeaderTimeout = 5 * time.Second
	webSocketRelayHTTPIdleTimeout   = 30 * time.Second
	webSocketRelayShutdownTimeout   = 10 * time.Second
	webSocketRelayMaxHeaderBytes    = 16 << 10
)

type proxyLifecycle interface {
	Serve(net.Listener) error
	Shutdown()
}

type proxyProcess struct {
	proxy               proxyLifecycle
	legacyListener      net.Listener
	webSocketListener   net.Listener
	webSocketHTTPServer *http.Server
	shutdownTimeout     time.Duration
}

func newWebSocketRelayHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: webSocketRelayReadHeaderTimeout,
		IdleTimeout:       webSocketRelayHTTPIdleTimeout,
		MaxHeaderBytes:    webSocketRelayMaxHeaderBytes,
	}
}

func (p *proxyProcess) run(ctx context.Context) error {
	serveErrors := make(chan error, 2)
	serveCount := 1

	go func() {
		serveErrors <- fmt.Errorf("legacy proxy listener stopped: %w", p.proxy.Serve(p.legacyListener))
	}()

	if p.webSocketHTTPServer != nil {
		serveCount++

		go func() {
			serveErrors <- fmt.Errorf(
				"websocket relay listener stopped: %w",
				p.webSocketHTTPServer.Serve(p.webSocketListener),
			)
		}()
	}

	var runErr error
	consumedServeResults := 0
	select {
	case <-ctx.Done():
	case runErr = <-serveErrors:
		consumedServeResults++
	}

	if p.webSocketHTTPServer != nil {
		shutdownTimeout := p.shutdownTimeout
		if shutdownTimeout <= 0 {
			shutdownTimeout = webSocketRelayShutdownTimeout
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		shutdownErr := p.webSocketHTTPServer.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil && runErr == nil {
			runErr = fmt.Errorf("cannot shut down websocket relay listener: %w", shutdownErr)
		}
	}

	p.legacyListener.Close() //nolint: errcheck
	p.proxy.Shutdown()

	for range serveCount - consumedServeResults {
		<-serveErrors
	}

	return runErr
}
