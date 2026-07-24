package mtglib

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/9seconds/mtg/v2/essentials"
	"github.com/9seconds/mtg/v2/mtglib/internal/dc"
	"github.com/9seconds/mtg/v2/mtglib/internal/obfuscation"
	"github.com/stretchr/testify/require"
)

type relayCoreLogEntry struct {
	message string
	err     error
	fields  map[string]any
}

type relayCoreCaptureLogger struct {
	lock    *sync.Mutex
	entries *[]relayCoreLogEntry
	fields  map[string]any
}

func newRelayCoreCaptureLogger() (*relayCoreCaptureLogger, *[]relayCoreLogEntry) {
	entries := &[]relayCoreLogEntry{}

	return &relayCoreCaptureLogger{
		lock:    &sync.Mutex{},
		entries: entries,
		fields:  map[string]any{},
	}, entries
}

func (l *relayCoreCaptureLogger) clone() *relayCoreCaptureLogger {
	fields := make(map[string]any, len(l.fields))
	for key, value := range l.fields {
		fields[key] = value
	}

	return &relayCoreCaptureLogger{lock: l.lock, entries: l.entries, fields: fields}
}

func (l *relayCoreCaptureLogger) Named(name string) Logger {
	clone := l.clone()
	clone.fields["logger"] = name
	return clone
}

func (l *relayCoreCaptureLogger) BindInt(name string, value int) Logger {
	clone := l.clone()
	clone.fields[name] = value
	return clone
}

func (l *relayCoreCaptureLogger) BindStr(name, value string) Logger {
	clone := l.clone()
	clone.fields[name] = value
	return clone
}

func (l *relayCoreCaptureLogger) BindJSON(name, value string) Logger {
	clone := l.clone()
	clone.fields[name] = value
	return clone
}

func (l *relayCoreCaptureLogger) Printf(format string, args ...any) {
	l.Debug(fmt.Sprintf(format, args...))
}

func (l *relayCoreCaptureLogger) Info(message string) {
	l.record(message, nil)
}

func (l *relayCoreCaptureLogger) Warning(message string) {
	l.record(message, nil)
}

func (l *relayCoreCaptureLogger) Debug(message string) {
	l.record(message, nil)
}

func (l *relayCoreCaptureLogger) InfoError(message string, err error) {
	l.record(message, err)
}

func (l *relayCoreCaptureLogger) WarningError(message string, err error) {
	l.record(message, err)
}

func (l *relayCoreCaptureLogger) DebugError(message string, err error) {
	l.record(message, err)
}

func (l *relayCoreCaptureLogger) record(message string, err error) {
	fields := make(map[string]any, len(l.fields))
	for key, value := range l.fields {
		fields[key] = value
	}

	l.lock.Lock()
	*l.entries = append(*l.entries, relayCoreLogEntry{message: message, err: err, fields: fields})
	l.lock.Unlock()
}

type relayCoreNetwork struct {
	lock        sync.Mutex
	dialConn    essentials.Conn
	dialCount   int
	dialNetwork string
	dialAddress string
}

func (n *relayCoreNetwork) Dial(network, address string) (essentials.Conn, error) {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.dialCount++
	n.dialNetwork = network
	n.dialAddress = address

	return n.dialConn, nil
}

func (n *relayCoreNetwork) DialContext(
	_ context.Context,
	network string,
	address string,
) (essentials.Conn, error) {
	return n.Dial(network, address)
}

func (n *relayCoreNetwork) MakeHTTPClient(
	func(context.Context, string, string) (essentials.Conn, error),
) *http.Client {
	panic("unexpected MakeHTTPClient call")
}

func (n *relayCoreNetwork) NativeDialer() *net.Dialer {
	panic("unexpected NativeDialer call")
}

func (n *relayCoreNetwork) dial() (int, string, string) {
	n.lock.Lock()
	defer n.lock.Unlock()

	return n.dialCount, n.dialNetwork, n.dialAddress
}

type relayCoreEventStream struct {
	lock   sync.Mutex
	events []Event
}

func (s *relayCoreEventStream) Send(_ context.Context, event Event) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.events = append(s.events, event)
}

func (s *relayCoreEventStream) snapshot() []Event {
	s.lock.Lock()
	defer s.lock.Unlock()

	return append([]Event(nil), s.events...)
}

type relayCoreIPList struct {
	lock     sync.Mutex
	contains bool
	seen     []net.IP
}

func (l *relayCoreIPList) Contains(ip net.IP) bool {
	l.lock.Lock()
	defer l.lock.Unlock()

	l.seen = append(l.seen, append(net.IP(nil), ip...))

	return l.contains
}

func (l *relayCoreIPList) Run(time.Duration) {}

func (l *relayCoreIPList) Shutdown() {}

func (l *relayCoreIPList) snapshot() []net.IP {
	l.lock.Lock()
	defer l.lock.Unlock()

	return append([]net.IP(nil), l.seen...)
}

func newRelayCoreTestProxy(
	t *testing.T,
	network Network,
	eventStream EventStream,
) (*Proxy, context.CancelFunc) {
	t.Helper()

	telegram, err := dc.New("only-ipv4")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	return &Proxy{
		ctx:              ctx,
		ctxCancel:        cancel,
		handshakeTimeout: time.Second,
		idleTimeout:      time.Second,
		telegram:         telegram,
		network:          network,
		eventStream:      eventStream,
		logger:           NoopLogger{},
	}, cancel
}

func waitRelayCoreDone(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay core did not terminate")
	}
}

func requireKnownDCAddress(t *testing.T, dcID int, network, address string) {
	t.Helper()

	require.Equal(t, "tcp4", network)
	require.Contains(t, essentials.TelegramCoreAddresses[dcID], address)
}

func TestWebSocketRelayAcceptsSecretlessPaddedIntermediate(t *testing.T) {
	t.Parallel()

	clientConn, proxyClientConn := net.Pipe()
	proxyTelegramConn, telegramConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()        //nolint: errcheck
		proxyClientConn.Close()   //nolint: errcheck
		proxyTelegramConn.Close() //nolint: errcheck
		telegramConn.Close()      //nolint: errcheck
	})

	network := &relayCoreNetwork{dialConn: essentials.WrapNetConn(proxyTelegramConn)}
	eventStream := &relayCoreEventStream{}
	proxy, cancel := newRelayCoreTestProxy(t, network, eventStream)
	t.Cleanup(cancel)

	clientIP := net.ParseIP("203.0.113.10")
	done := make(chan struct{})

	go func() {
		defer close(done)
		proxy.serveConn(essentials.WrapNetConn(proxyClientConn), clientIP, webSocketRelay)
	}()

	obfuscatedClient, err := (obfuscation.Obfuscator{}).
		SendHandshake(essentials.WrapNetConn(clientConn), dc.DefaultDC)
	require.NoError(t, err)

	telegramHandshake := make([]byte, 64)
	_, err = io.ReadFull(telegramConn, telegramHandshake)
	require.NoError(t, err)

	require.NoError(t, obfuscatedClient.Close())
	waitRelayCoreDone(t, done)

	dialCount, dialNetwork, dialAddress := network.dial()
	require.Equal(t, 1, dialCount)
	requireKnownDCAddress(t, dc.DefaultDC, dialNetwork, dialAddress)

	events := eventStream.snapshot()
	require.Len(t, events, 3)

	startEvent, ok := events[0].(EventStart)
	require.True(t, ok)
	require.True(t, clientIP.Equal(startEvent.RemoteIP))

	connectedEvent, ok := events[1].(EventConnectedToDC)
	require.True(t, ok)
	require.Equal(t, dc.DefaultDC, connectedEvent.DC)

	_, ok = events[2].(EventFinish)
	require.True(t, ok)
}

func TestWebSocketRelayRejectsUnknownDCWithoutDialing(t *testing.T) {
	t.Parallel()

	clientConn, proxyClientConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()      //nolint: errcheck
		proxyClientConn.Close() //nolint: errcheck
	})

	network := &relayCoreNetwork{}
	eventStream := &relayCoreEventStream{}
	proxy, cancel := newRelayCoreTestProxy(t, network, eventStream)
	t.Cleanup(cancel)

	done := make(chan struct{})

	go func() {
		defer close(done)
		proxy.serveConn(
			essentials.WrapNetConn(proxyClientConn),
			net.ParseIP("203.0.113.11"),
			webSocketRelay,
		)
	}()

	obfuscatedClient, err := (obfuscation.Obfuscator{}).
		SendHandshake(essentials.WrapNetConn(clientConn), 999)
	require.NoError(t, err)
	require.NoError(t, obfuscatedClient.Close())
	waitRelayCoreDone(t, done)

	dialCount, _, _ := network.dial()
	require.Zero(t, dialCount)

	events := eventStream.snapshot()
	require.Len(t, events, 2)
	_, ok := events[0].(EventStart)
	require.True(t, ok)
	_, ok = events[1].(EventFinish)
	require.True(t, ok)
}

func TestStockIngressCanStillFallbackOnUnknownDC(t *testing.T) {
	t.Parallel()

	clientConn, proxyClientConn := net.Pipe()
	proxyTelegramConn, telegramConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()        //nolint: errcheck
		proxyClientConn.Close()   //nolint: errcheck
		proxyTelegramConn.Close() //nolint: errcheck
		telegramConn.Close()      //nolint: errcheck
	})

	network := &relayCoreNetwork{dialConn: essentials.WrapNetConn(proxyTelegramConn)}
	eventStream := &relayCoreEventStream{}
	proxy, cancel := newRelayCoreTestProxy(t, network, eventStream)
	t.Cleanup(cancel)

	streamCtx := newStreamContext(
		proxy.ctx,
		proxy.logger,
		essentials.WrapNetConn(proxyClientConn),
		net.ParseIP("203.0.113.12"),
	)
	defer streamCtx.Close()
	streamCtx.dc = 999

	done := make(chan error, 1)
	go func() {
		done <- proxy.doTelegramCall(streamCtx, true)
	}()

	telegramHandshake := make([]byte, 64)
	_, err := io.ReadFull(telegramConn, telegramHandshake)
	require.NoError(t, err)
	require.NoError(t, <-done)

	require.Equal(t, dc.DefaultDC, streamCtx.dc)
	dialCount, dialNetwork, dialAddress := network.dial()
	require.Equal(t, 1, dialCount)
	requireKnownDCAddress(t, dc.DefaultDC, dialNetwork, dialAddress)
}

func TestAllowClientIPUsesExplicitClientIdentity(t *testing.T) {
	t.Parallel()

	clientIP := net.ParseIP("203.0.113.13")
	allowlist := &relayCoreIPList{contains: true}
	blocklist := &relayCoreIPList{contains: true}
	eventStream := &relayCoreEventStream{}
	proxy := &Proxy{
		ctx:         context.Background(),
		allowlist:   allowlist,
		blocklist:   blocklist,
		eventStream: eventStream,
		logger:      NoopLogger{},
	}

	require.False(t, proxy.allowClientIP(clientIP))

	allowlistSeen := allowlist.snapshot()
	require.Len(t, allowlistSeen, 1)
	require.True(t, clientIP.Equal(allowlistSeen[0]))

	blocklistSeen := blocklist.snapshot()
	require.Len(t, blocklistSeen, 1)
	require.True(t, clientIP.Equal(blocklistSeen[0]))

	events := eventStream.snapshot()
	require.Len(t, events, 1)
	blockedEvent, ok := events[0].(EventIPBlocklisted)
	require.True(t, ok)
	require.True(t, blockedEvent.IsBlockList)
	require.True(t, clientIP.Equal(blockedEvent.RemoteIP))
}

func TestWebSocketRelayHandshakeDiagnosticReportsPartialPeerClose(t *testing.T) {
	t.Parallel()

	clientConn, proxyClientConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()      //nolint: errcheck
		proxyClientConn.Close() //nolint: errcheck
	})

	captureLogger, logEntries := newRelayCoreCaptureLogger()

	network := &relayCoreNetwork{}
	eventStream := &relayCoreEventStream{}
	proxy, cancel := newRelayCoreTestProxy(t, network, eventStream)
	proxy.logger = captureLogger
	t.Cleanup(cancel)

	done := make(chan struct{})
	go func() {
		defer close(done)
		proxy.serveConn(
			essentials.WrapNetConn(proxyClientConn),
			net.ParseIP("203.0.113.13"),
			webSocketRelay,
		)
	}()

	_, err := clientConn.Write(bytes.Repeat([]byte{0x42}, 17))
	require.NoError(t, err)
	require.NoError(t, clientConn.Close())
	waitRelayCoreDone(t, done)

	diagnostic := requireRelayCoreDiagnostic(t, *logEntries)
	require.Equal(t, 17, diagnostic.fields["bytes-read"])
	require.Equal(t, "peer-closed", diagnostic.fields["failure-category"])
	require.NotContains(t, diagnostic.fields, "connection-type")
}

func TestWebSocketRelayHandshakeDiagnosticReportsConnectionType(t *testing.T) {
	t.Parallel()

	clientConn, proxyClientConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()      //nolint: errcheck
		proxyClientConn.Close() //nolint: errcheck
	})

	captureLogger, logEntries := newRelayCoreCaptureLogger()

	network := &relayCoreNetwork{}
	eventStream := &relayCoreEventStream{}
	proxy, cancel := newRelayCoreTestProxy(t, network, eventStream)
	proxy.logger = captureLogger
	t.Cleanup(cancel)

	done := make(chan struct{})
	go func() {
		defer close(done)
		proxy.serveConn(
			essentials.WrapNetConn(proxyClientConn),
			net.ParseIP("203.0.113.14"),
			webSocketRelay,
		)
	}()

	header := makeUnsupportedConnectionTypeHeader(t, [4]byte{0xef, 0xef, 0xef, 0xef})
	_, err := clientConn.Write(header)
	require.NoError(t, err)
	waitRelayCoreDone(t, done)

	diagnostic := requireRelayCoreDiagnostic(t, *logEntries)
	require.Equal(t, 64, diagnostic.fields["bytes-read"])
	require.Equal(t, "unsupported-connection-type", diagnostic.fields["failure-category"])
	require.Equal(t, "efefefef", diagnostic.fields["connection-type"])
}

func makeUnsupportedConnectionTypeHeader(t *testing.T, connectionType [4]byte) []byte {
	t.Helper()

	header := bytes.Repeat([]byte{0x42}, 64)
	key := header[8:40]
	iv := header[40:56]
	copy(header[56:60], connectionType[:])
	header[60] = byte(dc.DefaultDC)
	header[61] = 0

	plaintext := bytes.Clone(header)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	stream := cipher.NewCTR(block, iv)
	stream.XORKeyStream(header, header)
	copy(header[8:56], plaintext[8:56])

	return header
}

func requireRelayCoreDiagnostic(t *testing.T, entries []relayCoreLogEntry) relayCoreLogEntry {
	t.Helper()

	for _, entry := range entries {
		if entry.message == "WebSocket obfuscated handshake diagnostic" {
			return entry
		}
	}

	require.FailNow(t, "WebSocket handshake diagnostic was not logged")
	return relayCoreLogEntry{}
}
