package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/9seconds/mtg/v2/internal/config"
	"github.com/9seconds/mtg/v2/logger"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type proxyLifecycleMock struct {
	serveStarted chan struct{}
	serveErr     error
	shutdown     chan struct{}
	shutdownOnce sync.Once
}

func newProxyLifecycleMock() *proxyLifecycleMock {
	return &proxyLifecycleMock{
		serveStarted: make(chan struct{}),
		shutdown:     make(chan struct{}),
	}
}

func (p *proxyLifecycleMock) Serve(listener net.Listener) error {
	close(p.serveStarted)
	if p.serveErr != nil {
		return p.serveErr
	}

	_, err := listener.Accept()

	return err
}

func (p *proxyLifecycleMock) Shutdown() {
	p.shutdownOnce.Do(func() { close(p.shutdown) })
}

func TestNewWebSocketRelayHTTPServerBoundsRequests(t *testing.T) {
	t.Parallel()

	server := newWebSocketRelayHTTPServer(http.NotFoundHandler())
	require.Equal(t, webSocketRelayReadHeaderTimeout, server.ReadHeaderTimeout)
	require.Equal(t, webSocketRelayHTTPIdleTimeout, server.IdleTimeout)
	require.Equal(t, webSocketRelayMaxHeaderBytes, server.MaxHeaderBytes)
	require.Nil(t, server.TLSConfig)
}

func TestConfigurationDebugLogRedactsCredentials(t *testing.T) {
	t.Parallel()

	const secret = "7oe1GqLy6TBc38CV3jx7q09nb29nbGUuY29t"
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32))
	raw := fmt.Appendf(nil, `
secret = %q
bind-to = "0.0.0.0:3128"

[websocket-relay]
enabled = true
bind-to = "127.0.0.1:8081"
path = "/relay/v1/deployment-path"
tokens = [%q]
`, secret, token)
	conf, err := config.Parse(raw)
	require.NoError(t, err)
	require.NoError(t, conf.Validate())

	output := &bytes.Buffer{}
	baseLogger := zerolog.New(output).Level(zerolog.DebugLevel)
	testLogger := logger.NewZeroLogger(baseLogger)
	testLogger.BindJSON("configuration", conf.String()).Debug("configuration")

	logOutput := output.String()
	require.NotContains(t, logOutput, secret)
	require.NotContains(t, logOutput, token)
	require.Contains(t, logOutput, `"secret":"[redacted]"`)
	require.Contains(t, logOutput, `"tokens":"[redacted: 1 configured]"`)
}

func TestProxyProcessDisabledWebSocketRelayShutsDownCleanly(t *testing.T) {
	t.Parallel()

	listener := newLoopbackListener(t)
	proxy := newProxyLifecycleMock()
	ctx, cancel := context.WithCancel(context.Background())

	result := make(chan error, 1)
	go func() {
		result <- (&proxyProcess{
			proxy:          proxy,
			legacyListener: listener,
		}).run(ctx)
	}()

	requireChannelClosed(t, proxy.serveStarted)
	cancel()
	require.NoError(t, requireResult(t, result))
	requireChannelClosed(t, proxy.shutdown)
}

func TestProxyProcessServesWebSocketRelayAndShutsDownCleanly(t *testing.T) {
	t.Parallel()

	legacyListener := newLoopbackListener(t)
	webSocketListener := newLoopbackListener(t)
	proxy := newProxyLifecycleMock()
	requestReceived := make(chan struct{})
	server := newWebSocketRelayHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestReceived)
		w.WriteHeader(http.StatusNoContent)
	}))
	ctx, cancel := context.WithCancel(context.Background())

	result := make(chan error, 1)
	go func() {
		result <- (&proxyProcess{
			proxy:               proxy,
			legacyListener:      legacyListener,
			webSocketListener:   webSocketListener,
			webSocketHTTPServer: server,
			shutdownTimeout:     time.Second,
		}).run(ctx)
	}()

	requireChannelClosed(t, proxy.serveStarted)
	response, err := http.Get("http://" + webSocketListener.Addr().String())
	require.NoError(t, err)
	defer response.Body.Close() //nolint: errcheck
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	requireChannelClosed(t, requestReceived)

	cancel()
	require.NoError(t, requireResult(t, result))
	requireChannelClosed(t, proxy.shutdown)

	_, err = net.DialTimeout("tcp", webSocketListener.Addr().String(), 100*time.Millisecond)
	require.Error(t, err)
}

func TestProxyProcessPropagatesUnexpectedLegacyFailure(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("legacy serve failure")
	proxy := newProxyLifecycleMock()
	proxy.serveErr = expectedErr

	err := (&proxyProcess{
		proxy:          proxy,
		legacyListener: newLoopbackListener(t),
	}).run(context.Background())
	require.ErrorIs(t, err, expectedErr)
	require.ErrorContains(t, err, "legacy proxy listener stopped")
	requireChannelClosed(t, proxy.shutdown)
}

func TestProxyProcessPropagatesUnexpectedWebSocketFailure(t *testing.T) {
	t.Parallel()

	legacyListener := newLoopbackListener(t)
	webSocketListener := newLoopbackListener(t)
	require.NoError(t, webSocketListener.Close())
	proxy := newProxyLifecycleMock()

	err := (&proxyProcess{
		proxy:               proxy,
		legacyListener:      legacyListener,
		webSocketListener:   webSocketListener,
		webSocketHTTPServer: newWebSocketRelayHTTPServer(http.NotFoundHandler()),
		shutdownTimeout:     time.Second,
	}).run(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "websocket relay listener stopped")
	requireChannelClosed(t, proxy.shutdown)
}

func newLoopbackListener(t *testing.T) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() }) //nolint: errcheck

	return listener
}

func requireChannelClosed(t *testing.T, channel <-chan struct{}) {
	t.Helper()

	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lifecycle signal")
	}
}

func requireResult(t *testing.T, result <-chan error) error {
	t.Helper()

	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxy process result")
		return nil
	}
}
