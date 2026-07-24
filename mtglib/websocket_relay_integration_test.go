package mtglib

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/9seconds/mtg/v2/essentials"
	"github.com/9seconds/mtg/v2/mtglib/internal/dc"
	"github.com/9seconds/mtg/v2/mtglib/internal/obfuscation"
	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

const webSocketRelayIntegrationTimeout = 3 * time.Second

type webSocketRelayIntegrationNetwork struct {
	lock   sync.Mutex
	dials  int
	peers  chan net.Conn
	dialFn func(context.Context, string, string) (essentials.Conn, error)
}

func newWebSocketRelayIntegrationNetwork() *webSocketRelayIntegrationNetwork {
	return &webSocketRelayIntegrationNetwork{peers: make(chan net.Conn, 8)}
}

func (n *webSocketRelayIntegrationNetwork) Dial(network, address string) (essentials.Conn, error) {
	return n.DialContext(context.Background(), network, address)
}

func (n *webSocketRelayIntegrationNetwork) DialContext(
	ctx context.Context,
	network string,
	address string,
) (essentials.Conn, error) {
	n.lock.Lock()
	n.dials++
	dialFn := n.dialFn
	n.lock.Unlock()

	if dialFn != nil {
		return dialFn(ctx, network, address)
	}

	proxyConn, telegramConn := net.Pipe()
	select {
	case n.peers <- telegramConn:
		return essentials.WrapNetConn(proxyConn), nil
	case <-ctx.Done():
		proxyConn.Close()    //nolint: errcheck
		telegramConn.Close() //nolint: errcheck

		return nil, ctx.Err()
	}
}

func (n *webSocketRelayIntegrationNetwork) MakeHTTPClient(
	func(context.Context, string, string) (essentials.Conn, error),
) *http.Client {
	panic("unexpected MakeHTTPClient call")
}

func (n *webSocketRelayIntegrationNetwork) NativeDialer() *net.Dialer {
	panic("unexpected NativeDialer call")
}

func (n *webSocketRelayIntegrationNetwork) dialCount() int {
	n.lock.Lock()
	defer n.lock.Unlock()

	return n.dials
}

type webSocketRelayIntegrationFixtureOptions struct {
	handshakeTimeout time.Duration
	idleTimeout      time.Duration
	maxConnections   int
	maxMessageSize   int64
	tokens           []string
	network          *webSocketRelayIntegrationNetwork
	allowlist        IPBlocklist
}

type webSocketRelayIntegrationFixture struct {
	t           *testing.T
	proxy       *Proxy
	cancel      context.CancelFunc
	server      *httptest.Server
	network     *webSocketRelayIntegrationNetwork
	eventStream *relayCoreEventStream
	tokens      []string
}

func newWebSocketRelayIntegrationFixture(
	t *testing.T,
	opts webSocketRelayIntegrationFixtureOptions,
) *webSocketRelayIntegrationFixture {
	t.Helper()

	if opts.handshakeTimeout == 0 {
		opts.handshakeTimeout = time.Second
	}
	if opts.idleTimeout == 0 {
		opts.idleTimeout = time.Second
	}
	if opts.maxConnections == 0 {
		opts.maxConnections = 4
	}
	if opts.maxMessageSize == 0 {
		opts.maxMessageSize = 16 << 20
	}
	if len(opts.tokens) == 0 {
		opts.tokens = []string{testWebSocketRelayToken(0)}
	}
	if opts.network == nil {
		opts.network = newWebSocketRelayIntegrationNetwork()
	}

	eventStream := &relayCoreEventStream{}
	proxy, cancel := newRelayCoreTestProxy(t, opts.network, eventStream)
	proxy.handshakeTimeout = opts.handshakeTimeout
	proxy.idleTimeout = opts.idleTimeout
	proxy.blocklist = &relayCoreIPList{}
	if opts.allowlist == nil {
		proxy.allowlist = &relayCoreIPList{contains: true}
	} else {
		proxy.allowlist = opts.allowlist
	}

	handler, err := proxy.NewWebSocketRelayHandler(WebSocketRelayOptions{
		Path:           testWebSocketRelayPath,
		Tokens:         opts.tokens,
		MaxConnections: opts.maxConnections,
		MaxMessageSize: opts.maxMessageSize,
	})
	require.NoError(t, err)

	fixture := &webSocketRelayIntegrationFixture{
		t:           t,
		proxy:       proxy,
		cancel:      cancel,
		server:      httptest.NewServer(handler),
		network:     opts.network,
		eventStream: eventStream,
		tokens:      append([]string(nil), opts.tokens...),
	}

	t.Cleanup(func() {
		cancel()
		fixture.server.CloseClientConnections()
		fixture.server.Close()

		done := make(chan struct{})
		go func() {
			proxy.streamWaitGroup.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(webSocketRelayIntegrationTimeout):
			t.Error("websocket relay streams did not stop during fixture cleanup")
		}
	})

	return fixture
}

func (f *webSocketRelayIntegrationFixture) websocketURL() string {
	return "ws" + strings.TrimPrefix(f.server.URL, "http") + testWebSocketRelayPath
}

func (f *webSocketRelayIntegrationFixture) dial(token string) (*websocket.Conn, *http.Response, error) {
	opts := &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + token},
			"X-Real-IP":     []string{testWebSocketRelayClientIP},
		},
		Subprotocols: []string{webSocketRelaySubprotocol},
	}
	ctx, cancel := context.WithTimeout(context.Background(), webSocketRelayIntegrationTimeout)
	defer cancel()

	return websocket.Dial(ctx, f.websocketURL(), opts)
}

func (f *webSocketRelayIntegrationFixture) dialValid() *websocket.Conn {
	f.t.Helper()

	conn, response, err := f.dial(f.tokens[0])
	require.NoError(f.t, err)
	require.Equal(f.t, http.StatusSwitchingProtocols, response.StatusCode)
	require.Equal(f.t, webSocketRelaySubprotocol, conn.Subprotocol())

	return conn
}

func (f *webSocketRelayIntegrationFixture) acceptTelegram() (net.Conn, essentials.Conn) {
	f.t.Helper()

	var raw net.Conn
	select {
	case raw = <-f.network.peers:
	case <-time.After(webSocketRelayIntegrationTimeout):
		f.t.Fatal("timed out waiting for the fake Telegram connection")
	}

	_, conn, err := (obfuscation.Obfuscator{}).ReadHandshake(essentials.WrapNetConn(raw))
	require.NoError(f.t, err)

	return raw, conn
}

type webSocketRelayClientCodec struct {
	t      *testing.T
	stream essentials.Conn
	raw    net.Conn
	header []byte
}

func newWebSocketRelayClientCodec(t *testing.T) *webSocketRelayClientCodec {
	t.Helper()

	clientConn, raw := net.Pipe()
	type handshakeResult struct {
		conn essentials.Conn
		err  error
	}
	result := make(chan handshakeResult, 1)
	go func() {
		conn, err := (obfuscation.Obfuscator{}).
			SendHandshake(essentials.WrapNetConn(clientConn), dc.DefaultDC)
		result <- handshakeResult{conn: conn, err: err}
	}()

	header := make([]byte, 64)
	_, err := io.ReadFull(raw, header)
	require.NoError(t, err)

	handshake := <-result
	require.NoError(t, handshake.err)

	codec := &webSocketRelayClientCodec{t: t, stream: handshake.conn, raw: raw, header: header}
	t.Cleanup(func() {
		codec.stream.Close() //nolint: errcheck
		codec.raw.Close()    //nolint: errcheck
	})

	return codec
}

func (c *webSocketRelayClientCodec) encrypt(plaintext []byte) []byte {
	c.t.Helper()

	// The obfuscation writer applies AES-CTR in place. Keep fixture plaintext
	// immutable so assertions compare the relay result with the original frame.
	toEncrypt := append([]byte(nil), plaintext...)
	done := make(chan error, 1)
	go func() {
		done <- writeAll(c.stream, toEncrypt)
	}()

	ciphertext := make([]byte, len(plaintext))
	_, err := io.ReadFull(c.raw, ciphertext)
	require.NoError(c.t, err)
	require.NoError(c.t, <-done)

	return ciphertext
}

func (c *webSocketRelayClientCodec) decrypt(ciphertext []byte) []byte {
	c.t.Helper()

	plaintext := make([]byte, len(ciphertext))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(c.stream, plaintext)
		done <- err
	}()

	require.NoError(c.t, writeAll(c.raw, ciphertext))
	require.NoError(c.t, <-done)

	return plaintext
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}

	return nil
}

func paddedIntermediateFixture(payload []byte) []byte {
	frame := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(frame, uint32(len(payload)))
	copy(frame[4:], payload)

	return frame
}

func writeWebSocketFragments(
	t *testing.T,
	conn *websocket.Conn,
	data []byte,
	fragmentSizes ...int,
) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), webSocketRelayIntegrationTimeout)
	defer cancel()

	offset := 0
	for _, size := range fragmentSizes {
		if offset == len(data) {
			break
		}
		if size > len(data)-offset {
			size = len(data) - offset
		}
		require.NoError(t, conn.Write(ctx, websocket.MessageBinary, data[offset:offset+size]))
		offset += size
	}
	if offset < len(data) {
		require.NoError(t, conn.Write(ctx, websocket.MessageBinary, data[offset:]))
	}
}

func readWebSocketStream(t *testing.T, conn *websocket.Conn, size int) []byte {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), webSocketRelayIntegrationTimeout)
	defer cancel()

	result := make([]byte, 0, size)
	for len(result) < size {
		messageType, data, err := conn.Read(ctx)
		require.NoError(t, err)
		require.Equal(t, websocket.MessageBinary, messageType)
		result = append(result, data...)
	}
	require.Len(t, result, size)

	return result
}

func requireWebSocketCloseStatus(
	t *testing.T,
	conn *websocket.Conn,
	expected ...websocket.StatusCode,
) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), webSocketRelayIntegrationTimeout)
	defer cancel()

	_, _, err := conn.Read(ctx)
	require.Error(t, err)
	require.Contains(t, expected, websocket.CloseStatus(err))
}

func requireConnectionClosed(t *testing.T, conn net.Conn) {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(webSocketRelayIntegrationTimeout)); err != nil {
		return
	}

	_, err := conn.Read(make([]byte, 1))
	require.Error(t, err)
}

func TestWebSocketRelayEndToEndPaddedIntermediateFixture(t *testing.T) {
	fixture := newWebSocketRelayIntegrationFixture(t, webSocketRelayIntegrationFixtureOptions{})
	wsConn := fixture.dialValid()
	codec := newWebSocketRelayClientCodec(t)

	upstream := paddedIntermediateFixture([]byte("fixture-upstream"))
	upstreamCiphertext := codec.encrypt(upstream)
	headerAndPacket := append(append([]byte(nil), codec.header...), upstreamCiphertext...)
	writeWebSocketFragments(t, wsConn, headerAndPacket, len(headerAndPacket))

	_, telegram := fixture.acceptTelegram()
	receivedUpstream := make([]byte, len(upstream))
	_, err := io.ReadFull(telegram, receivedUpstream)
	require.NoError(t, err)
	require.Equal(t, upstream, receivedUpstream)

	secondUpstream := paddedIntermediateFixture([]byte("fragmented-fixture-upstream"))
	secondCiphertext := codec.encrypt(secondUpstream)
	writeWebSocketFragments(t, wsConn, secondCiphertext, 1, 2, 3, 5, 8, 13)
	receivedSecondUpstream := make([]byte, len(secondUpstream))
	_, err = io.ReadFull(telegram, receivedSecondUpstream)
	require.NoError(t, err)
	require.Equal(t, secondUpstream, receivedSecondUpstream)

	downstream := paddedIntermediateFixture([]byte("fixture-downstream"))
	expectedDownstream := append([]byte(nil), downstream...)
	require.NoError(t, writeAll(telegram, downstream))
	downstreamCiphertext := readWebSocketStream(t, wsConn, len(downstream))
	require.Equal(t, expectedDownstream, codec.decrypt(downstreamCiphertext))

	require.NoError(t, wsConn.Close(websocket.StatusNormalClosure, ""))
}

func TestWebSocketRelayMessageLimitAppliesPerMessage(t *testing.T) {
	fixture := newWebSocketRelayIntegrationFixture(t, webSocketRelayIntegrationFixtureOptions{
		maxMessageSize: 96,
	})
	wsConn := fixture.dialValid()
	codec := newWebSocketRelayClientCodec(t)
	writeWebSocketFragments(t, wsConn, codec.header, len(codec.header))

	_, telegram := fixture.acceptTelegram()
	first := paddedIntermediateFixture(make([]byte, 64))
	second := paddedIntermediateFixture(make([]byte, 64))
	writeWebSocketFragments(t, wsConn, codec.encrypt(first), len(first))
	writeWebSocketFragments(t, wsConn, codec.encrypt(second), len(second))

	received := make([]byte, len(first)+len(second))
	_, err := io.ReadFull(telegram, received)
	require.NoError(t, err)
	require.Equal(t, append(first, second...), received)
}

func TestWebSocketRelayHandshakeTimeoutEnforcesConnectionLimit(t *testing.T) {
	fixture := newWebSocketRelayIntegrationFixture(t, webSocketRelayIntegrationFixtureOptions{
		handshakeTimeout: 75 * time.Millisecond,
		maxConnections:   1,
	})
	first := fixture.dialValid()

	second, response, err := fixture.dial(fixture.tokens[0])
	if second != nil {
		second.CloseNow() //nolint: errcheck
	}
	requireWebSocketRelayRejected(t, response, err)
	requireWebSocketCloseStatus(
		t,
		first,
		websocket.StatusNormalClosure,
		websocket.StatusCode(-1),
	)
	require.Zero(t, fixture.network.dialCount())

	require.Eventually(t, func() bool {
		third, _, dialErr := fixture.dial(fixture.tokens[0])
		if dialErr != nil {
			return false
		}
		third.CloseNow() //nolint: errcheck

		return true
	}, webSocketRelayIntegrationTimeout, 10*time.Millisecond)
}

func TestWebSocketRelayIdleTimeoutClosesBothSides(t *testing.T) {
	fixture := newWebSocketRelayIntegrationFixture(t, webSocketRelayIntegrationFixtureOptions{
		idleTimeout: 75 * time.Millisecond,
	})
	wsConn := fixture.dialValid()
	codec := newWebSocketRelayClientCodec(t)
	writeWebSocketFragments(t, wsConn, codec.header, 7, 11, 19)

	rawTelegram, _ := fixture.acceptTelegram()
	requireWebSocketCloseStatus(
		t,
		wsConn,
		websocket.StatusNormalClosure,
		websocket.StatusCode(-1),
	)
	requireConnectionClosed(t, rawTelegram)
}

func TestWebSocketRelayRapidFailedAuthenticationProbesRemainBounded(t *testing.T) {
	fixture := newWebSocketRelayIntegrationFixture(t, webSocketRelayIntegrationFixtureOptions{
		maxConnections: 1,
	})

	type probeResult struct {
		response *http.Response
		err      error
	}
	results := make(chan probeResult, 64)
	var probes sync.WaitGroup
	for range 64 {
		probes.Add(1)
		go func() {
			defer probes.Done()
			conn, response, err := fixture.dial(testWebSocketRelayToken(200))
			if conn != nil {
				conn.CloseNow() //nolint: errcheck
			}
			results <- probeResult{response: response, err: err}
		}()
	}
	probes.Wait()
	close(results)

	for result := range results {
		requireWebSocketRelayRejected(t, result.response, result.err)
	}
	require.Zero(t, fixture.network.dialCount())
	require.Empty(t, fixture.eventStream.snapshot())

	valid := fixture.dialValid()
	require.NoError(t, valid.Close(websocket.StatusNormalClosure, ""))
}

type webSocketRelayBlockingIPList struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *webSocketRelayBlockingIPList) Contains(net.IP) bool {
	l.once.Do(func() { close(l.entered) })
	<-l.release

	return true
}

func (l *webSocketRelayBlockingIPList) Run(time.Duration) {}

func (l *webSocketRelayBlockingIPList) Shutdown() {}

func TestWebSocketRelayShutdownDuringAuthentication(t *testing.T) {
	allowlist := &webSocketRelayBlockingIPList{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	fixture := newWebSocketRelayIntegrationFixture(t, webSocketRelayIntegrationFixtureOptions{
		allowlist: allowlist,
	})

	type dialResult struct {
		conn     *websocket.Conn
		response *http.Response
		err      error
	}
	result := make(chan dialResult, 1)
	go func() {
		conn, response, err := fixture.dial(fixture.tokens[0])
		result <- dialResult{conn: conn, response: response, err: err}
	}()

	select {
	case <-allowlist.entered:
	case <-time.After(webSocketRelayIntegrationTimeout):
		t.Fatal("authentication did not reach the allowlist gate")
	}
	fixture.cancel()
	close(allowlist.release)

	dial := <-result
	if dial.conn != nil {
		dial.conn.CloseNow() //nolint: errcheck
	}
	requireWebSocketRelayRejected(t, dial.response, dial.err)
}

func TestWebSocketRelayShutdownDuringHeaderRead(t *testing.T) {
	fixture := newWebSocketRelayIntegrationFixture(t, webSocketRelayIntegrationFixtureOptions{})
	wsConn := fixture.dialValid()
	fixture.cancel()

	requireWebSocketCloseStatus(t, wsConn, websocket.StatusGoingAway)
}

func TestWebSocketRelayShutdownDuringTelegramDial(t *testing.T) {
	dialStarted := make(chan struct{})
	var dialStartedOnce sync.Once
	network := newWebSocketRelayIntegrationNetwork()
	network.dialFn = func(ctx context.Context, _, _ string) (essentials.Conn, error) {
		dialStartedOnce.Do(func() { close(dialStarted) })
		<-ctx.Done()

		return nil, ctx.Err()
	}
	fixture := newWebSocketRelayIntegrationFixture(t, webSocketRelayIntegrationFixtureOptions{
		network: network,
	})
	wsConn := fixture.dialValid()
	codec := newWebSocketRelayClientCodec(t)
	writeWebSocketFragments(t, wsConn, codec.header, len(codec.header))

	select {
	case <-dialStarted:
	case <-time.After(webSocketRelayIntegrationTimeout):
		t.Fatal("Telegram dial did not start")
	}
	fixture.cancel()

	requireWebSocketCloseStatus(t, wsConn, websocket.StatusGoingAway)
}

func TestWebSocketRelayShutdownDuringActiveRelay(t *testing.T) {
	fixture := newWebSocketRelayIntegrationFixture(t, webSocketRelayIntegrationFixtureOptions{})
	wsConn := fixture.dialValid()
	codec := newWebSocketRelayClientCodec(t)
	writeWebSocketFragments(t, wsConn, codec.header, len(codec.header))

	rawTelegram, _ := fixture.acceptTelegram()
	fixture.cancel()
	requireWebSocketCloseStatus(t, wsConn, websocket.StatusGoingAway)
	requireConnectionClosed(t, rawTelegram)
}

func TestWebSocketRelaySlowConsumersPreserveOrderedBackpressure(t *testing.T) {
	fixture := newWebSocketRelayIntegrationFixture(t, webSocketRelayIntegrationFixtureOptions{
		idleTimeout:    2 * time.Second,
		maxMessageSize: 64 << 10,
	})
	wsConn := fixture.dialValid()
	codec := newWebSocketRelayClientCodec(t)
	writeWebSocketFragments(t, wsConn, codec.header, 3, 5, 7, 11)
	_, telegram := fixture.acceptTelegram()

	upstream := paddedIntermediateFixture(make([]byte, 512<<10))
	for index := range upstream {
		upstream[index] = byte(index)
	}
	upstreamCiphertext := codec.encrypt(upstream)
	upstreamWrite := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), webSocketRelayIntegrationTimeout)
		defer cancel()

		for offset := 0; offset < len(upstreamCiphertext); offset += 32 << 10 {
			end := min(offset+(32<<10), len(upstreamCiphertext))
			if err := wsConn.Write(ctx, websocket.MessageBinary, upstreamCiphertext[offset:end]); err != nil {
				upstreamWrite <- err
				return
			}
		}
		upstreamWrite <- nil
	}()
	time.Sleep(50 * time.Millisecond)
	receivedUpstream := make([]byte, len(upstream))
	_, err := io.ReadFull(telegram, receivedUpstream)
	require.NoError(t, err)
	require.NoError(t, <-upstreamWrite)
	require.Equal(t, upstream, receivedUpstream)

	downstream := paddedIntermediateFixture(make([]byte, 512<<10))
	for index := range downstream {
		downstream[index] = byte(255 - index)
	}
	expectedDownstream := append([]byte(nil), downstream...)
	downstreamWrite := make(chan error, 1)
	go func() { downstreamWrite <- writeAll(telegram, downstream) }()
	time.Sleep(50 * time.Millisecond)
	downstreamCiphertext := readWebSocketStream(t, wsConn, len(downstream))
	require.NoError(t, <-downstreamWrite)
	decryptedDownstream := codec.decrypt(downstreamCiphertext)
	require.Equal(t, len(expectedDownstream), len(decryptedDownstream))
	require.Equal(t, expectedDownstream[:64], decryptedDownstream[:64])
	require.Equal(
		t,
		expectedDownstream[len(expectedDownstream)-64:],
		decryptedDownstream[len(decryptedDownstream)-64:],
	)
}

func TestWebSocketRelayRepeatedTokenRotationsDoNotRetainRetiredTokens(t *testing.T) {
	for generation := byte(1); generation <= 8; generation++ {
		t.Run(string(rune('0'+generation)), func(t *testing.T) {
			current := testWebSocketRelayToken(generation)
			next := testWebSocketRelayToken(generation + 1)
			fixture := newWebSocketRelayIntegrationFixture(t, webSocketRelayIntegrationFixtureOptions{
				tokens:         []string{current, next},
				maxConnections: 4,
			})

			for _, token := range []string{current, next} {
				conn, response, err := fixture.dial(token)
				require.NoError(t, err)
				require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
				conn.CloseNow() //nolint: errcheck
			}

			retired, response, err := fixture.dial(testWebSocketRelayToken(generation - 1))
			if retired != nil {
				retired.CloseNow() //nolint: errcheck
			}
			requireWebSocketRelayRejected(t, response, err)
		})
	}
}

func TestWebSocketRelayBlockedOperationsEndOnShutdown(t *testing.T) {
	fixture := newWebSocketRelayIntegrationFixture(t, webSocketRelayIntegrationFixtureOptions{
		idleTimeout: 10 * time.Second,
	})
	wsConn := fixture.dialValid()
	codec := newWebSocketRelayClientCodec(t)
	writeWebSocketFragments(t, wsConn, codec.header, len(codec.header))
	rawTelegram, _ := fixture.acceptTelegram()

	blockedPayload := codec.encrypt(paddedIntermediateFixture(make([]byte, 32<<10)))
	require.NoError(t, wsConn.Write(context.Background(), websocket.MessageBinary, blockedPayload))
	time.Sleep(25 * time.Millisecond)

	fixture.cancel()
	requireWebSocketCloseStatus(t, wsConn, websocket.StatusGoingAway)
	requireConnectionClosed(t, rawTelegram)
}
