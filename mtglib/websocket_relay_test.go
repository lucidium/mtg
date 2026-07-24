package mtglib

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/9seconds/mtg/v2/essentials"
	"github.com/9seconds/mtg/v2/mtglib/internal/dc"
	"github.com/9seconds/mtg/v2/mtglib/internal/obfuscation"
	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

const (
	testWebSocketRelayPath      = "/relay/v1/test-path"
	testWebSocketRelayClientIP  = "203.0.113.42"
	testWebSocketRelayTokenSeed = "websocket-relay-token"
)

type webSocketRelayFixture struct {
	t            *testing.T
	proxy        *Proxy
	handler      http.Handler
	server       *httptest.Server
	network      *relayCoreNetwork
	eventStream  *relayCoreEventStream
	telegramConn net.Conn
	tokens       []string
}

type webSocketRelayRequestRoundTripper func(*http.Request) (*http.Response, error)

func (f webSocketRelayRequestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func newWebSocketRelayFixture(
	t *testing.T,
	maxConnections int,
	maxMessageSize int64,
	tokens ...string,
) *webSocketRelayFixture {
	t.Helper()

	if len(tokens) == 0 {
		tokens = []string{testWebSocketRelayToken(0)}
	}

	proxyTelegramConn, telegramConn := net.Pipe()
	network := &relayCoreNetwork{dialConn: essentials.WrapNetConn(proxyTelegramConn)}
	eventStream := &relayCoreEventStream{}
	proxy, cancel := newRelayCoreTestProxy(t, network, eventStream)
	proxy.allowlist = &relayCoreIPList{contains: true}
	proxy.blocklist = &relayCoreIPList{}

	handler, err := proxy.NewWebSocketRelayHandler(WebSocketRelayOptions{
		Path:           testWebSocketRelayPath,
		Tokens:         tokens,
		MaxConnections: maxConnections,
		MaxMessageSize: maxMessageSize,
	})
	require.NoError(t, err)

	server := httptest.NewServer(handler)
	fixture := &webSocketRelayFixture{
		t:            t,
		proxy:        proxy,
		handler:      handler,
		server:       server,
		network:      network,
		eventStream:  eventStream,
		telegramConn: telegramConn,
		tokens:       tokens,
	}

	t.Cleanup(func() {
		cancel()
		server.CloseClientConnections()
		server.Close()
		proxyTelegramConn.Close() //nolint: errcheck
		telegramConn.Close()      //nolint: errcheck
	})

	return fixture
}

func testWebSocketRelayToken(index byte) string {
	seed := append([]byte(testWebSocketRelayTokenSeed), index)
	digest := sha256.Sum256(seed)

	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (f *webSocketRelayFixture) webSocketURL() string {
	return "ws" + strings.TrimPrefix(f.server.URL, "http") + testWebSocketRelayPath
}

func (f *webSocketRelayFixture) dial(
	token string,
	configure func(*websocket.DialOptions),
) (*websocket.Conn, *http.Response, error) {
	f.t.Helper()

	opts := &websocket.DialOptions{
		HTTPHeader: http.Header{
			http.CanonicalHeaderKey("Authorization"): []string{"Bearer " + token},
			http.CanonicalHeaderKey("X-Real-IP"):     []string{testWebSocketRelayClientIP},
		},
		Subprotocols: []string{webSocketRelaySubprotocol},
	}
	if configure != nil {
		configure(opts)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	return websocket.Dial(ctx, f.webSocketURL(), opts)
}

func (f *webSocketRelayFixture) dialValid() *websocket.Conn {
	f.t.Helper()

	conn, response, err := f.dial(f.tokens[0], nil)
	require.NoError(f.t, err)
	require.NotNil(f.t, response)
	require.Equal(f.t, http.StatusSwitchingProtocols, response.StatusCode)
	require.Equal(f.t, webSocketRelaySubprotocol, conn.Subprotocol())
	require.Empty(f.t, response.Header.Get("Sec-WebSocket-Extensions"))

	return conn
}

func requireWebSocketRelayRejected(t *testing.T, response *http.Response, err error) {
	t.Helper()

	require.Error(t, err)
	require.NotNil(t, response)
	require.Equal(t, webSocketRelayRejectionCode, response.StatusCode)

	body, readErr := io.ReadAll(response.Body)
	require.NoError(t, readErr)
	require.Equal(t, webSocketRelayRejectionBody, string(body))
}

func sendWebSocketRelayHandshake(
	t *testing.T,
	conn *websocket.Conn,
	fragmentSizes ...int,
) (essentials.Conn, net.Conn) {
	t.Helper()

	clientConn, rawPeer := net.Pipe()
	handshakeReady := make(chan struct{})
	var obfuscatedClient essentials.Conn

	go func() {
		defer close(handshakeReady)

		var err error
		obfuscatedClient, err = (obfuscation.Obfuscator{}).
			SendHandshake(essentials.WrapNetConn(clientConn), dc.DefaultDC)
		require.NoError(t, err)
	}()

	handshake := make([]byte, 64)
	_, err := io.ReadFull(rawPeer, handshake)
	require.NoError(t, err)
	<-handshakeReady

	if len(fragmentSizes) == 0 {
		fragmentSizes = []int{len(handshake)}
	}

	offset := 0
	for _, size := range fragmentSizes {
		require.Positive(t, size)
		require.LessOrEqual(t, offset+size, len(handshake))
		require.NoError(t, conn.Write(context.Background(), websocket.MessageBinary, handshake[offset:offset+size]))
		offset += size
	}
	require.Equal(t, len(handshake), offset)

	return obfuscatedClient, rawPeer
}

func TestWebSocketRelayOptionsValidateTokens(t *testing.T) {
	t.Parallel()

	validToken := testWebSocketRelayToken(0)
	validToken2 := testWebSocketRelayToken(1)

	testCases := []struct {
		name   string
		tokens []string
		valid  bool
	}{
		{name: "one token", tokens: []string{validToken}, valid: true},
		{name: "rotation tokens", tokens: []string{validToken, validToken2}, valid: true},
		{name: "missing token"},
		{name: "padded token", tokens: []string{validToken + "="}},
		{name: "short token", tokens: []string{base64.RawURLEncoding.EncodeToString(make([]byte, 31))}},
		{name: "long token", tokens: []string{base64.RawURLEncoding.EncodeToString(make([]byte, 65))}},
		{name: "duplicate token", tokens: []string{validToken, validToken}},
		{name: "too many tokens", tokens: []string{validToken, validToken2, testWebSocketRelayToken(2), testWebSocketRelayToken(3), testWebSocketRelayToken(4)}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			digests, err := makeWebSocketRelayTokenDigests(testCase.tokens)
			if testCase.valid {
				require.NoError(t, err)
				require.Len(t, digests, len(testCase.tokens))
				return
			}

			require.Error(t, err)
			require.NotContains(t, err.Error(), validToken)
			require.NotContains(t, err.Error(), validToken2)
		})
	}
}

func TestWebSocketRelayAcceptsRotationTokensAndNegotiatesProtocol(t *testing.T) {
	t.Parallel()

	token1 := testWebSocketRelayToken(0)
	token2 := testWebSocketRelayToken(1)
	fixture := newWebSocketRelayFixture(t, 2, 16<<20, token1, token2)

	for _, token := range []string{token1, token2} {
		conn, response, err := fixture.dial(token, func(opts *websocket.DialOptions) {
			opts.CompressionMode = websocket.CompressionContextTakeover
			opts.Subprotocols = []string{"other", webSocketRelaySubprotocol}
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
		require.Equal(t, webSocketRelaySubprotocol, conn.Subprotocol())
		require.Empty(t, response.Header.Get("Sec-WebSocket-Extensions"))
		require.NoError(t, conn.Close(websocket.StatusNormalClosure, ""))
	}
}

func TestWebSocketRelayRejectsPreUpgradeFailuresUniformly(t *testing.T) {
	t.Parallel()

	fixture := newWebSocketRelayFixture(t, 1, 16<<20)
	validToken := fixture.tokens[0]

	testCases := []struct {
		name      string
		url       string
		configure func(*websocket.DialOptions)
	}{
		{name: "wrong path", url: "ws" + strings.TrimPrefix(fixture.server.URL, "http") + "/wrong"},
		{name: "query", url: fixture.webSocketURL() + "?probe=1"},
		{name: "missing token", configure: func(opts *websocket.DialOptions) { opts.HTTPHeader.Del("Authorization") }},
		{name: "wrong token", configure: func(opts *websocket.DialOptions) {
			opts.HTTPHeader.Set("Authorization", "Bearer "+testWebSocketRelayToken(9))
		}},
		{name: "multiple authorization", configure: func(opts *websocket.DialOptions) { opts.HTTPHeader.Add("Authorization", "Bearer "+validToken) }},
		{name: "missing subprotocol", configure: func(opts *websocket.DialOptions) { opts.Subprotocols = nil }},
		{name: "wrong subprotocol", configure: func(opts *websocket.DialOptions) { opts.Subprotocols = []string{"other"} }},
		{name: "missing client IP", configure: func(opts *websocket.DialOptions) { opts.HTTPHeader.Del("X-Real-IP") }},
		{name: "invalid client IP", configure: func(opts *websocket.DialOptions) { opts.HTTPHeader.Set("X-Real-IP", "not-an-ip") }},
		{name: "multiple client IP", configure: func(opts *websocket.DialOptions) { opts.HTTPHeader.Add("X-Real-IP", "198.51.100.1") }},
		{name: "present origin", configure: func(opts *websocket.DialOptions) { opts.HTTPHeader.Set("Origin", fixture.server.URL) }},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			opts := &websocket.DialOptions{
				HTTPHeader: http.Header{
					http.CanonicalHeaderKey("Authorization"): []string{"Bearer " + validToken},
					http.CanonicalHeaderKey("X-Real-IP"):     []string{testWebSocketRelayClientIP},
				},
				Subprotocols: []string{webSocketRelaySubprotocol},
			}
			if testCase.configure != nil {
				testCase.configure(opts)
			}

			url := testCase.url
			if url == "" {
				url = fixture.webSocketURL()
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			conn, response, err := websocket.Dial(ctx, url, opts)
			if conn != nil {
				conn.CloseNow() //nolint: errcheck
			}
			requireWebSocketRelayRejected(t, response, err)
		})
	}
}

func TestWebSocketRelayRejectsWrongMethodAndNonLoopbackPeer(t *testing.T) {
	t.Parallel()

	fixture := newWebSocketRelayFixture(t, 1, 16<<20)

	for _, testCase := range []struct {
		name       string
		method     string
		remoteAddr string
	}{
		{name: "wrong method", method: http.MethodPost, remoteAddr: "127.0.0.1:12345"},
		{name: "non-loopback peer", method: http.MethodGet, remoteAddr: "198.51.100.20:12345"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, fixture.webSocketURL(), nil)
			request.RemoteAddr = testCase.remoteAddr
			recorder := httptest.NewRecorder()

			fixture.handler.ServeHTTP(recorder, request)

			require.Equal(t, webSocketRelayRejectionCode, recorder.Code)
			require.Equal(t, webSocketRelayRejectionBody, recorder.Body.String())
		})
	}
}

func TestWebSocketRelayRejectsBlockedClientIP(t *testing.T) {
	t.Parallel()

	fixture := newWebSocketRelayFixture(t, 1, 16<<20)
	fixture.proxy.blocklist = &relayCoreIPList{contains: true}

	conn, response, err := fixture.dial(fixture.tokens[0], nil)
	if conn != nil {
		conn.CloseNow() //nolint: errcheck
	}
	requireWebSocketRelayRejected(t, response, err)

	seen := fixture.proxy.blocklist.(*relayCoreIPList).snapshot() //nolint: forcetypeassert
	require.Len(t, seen, 1)
	require.True(t, net.ParseIP(testWebSocketRelayClientIP).Equal(seen[0]))
}

func TestWebSocketRelayReassemblesFragmentedHandshakeAndRelaysOrderedBytes(t *testing.T) {
	t.Parallel()

	fixture := newWebSocketRelayFixture(t, 1, 16<<20)
	wsConn := fixture.dialValid()
	obfuscatedClient, rawClientPeer := sendWebSocketRelayHandshake(t, wsConn, 1, 7, 13, 43)

	_, telegramPeer, err := (obfuscation.Obfuscator{}).
		ReadHandshake(essentials.WrapNetConn(fixture.telegramConn))
	require.NoError(t, err)

	upstream := []byte("client-to-telegram")
	expectedUpstream := append([]byte(nil), upstream...)
	downstream := []byte("telegram-to-client")
	expectedDownstream := append([]byte(nil), downstream...)

	encryptedUpstream := make([]byte, len(upstream))
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := obfuscatedClient.Write(upstream)
		writeDone <- writeErr
	}()
	_, err = io.ReadFull(rawClientPeer, encryptedUpstream)
	require.NoError(t, err)
	require.NoError(t, <-writeDone)
	require.NoError(t, wsConn.Write(context.Background(), websocket.MessageBinary, encryptedUpstream[:7]))
	require.NoError(t, wsConn.Write(context.Background(), websocket.MessageBinary, encryptedUpstream[7:]))

	receivedUpstream := make([]byte, len(upstream))
	_, err = io.ReadFull(telegramPeer, receivedUpstream)
	require.NoError(t, err)
	require.Equal(t, expectedUpstream, receivedUpstream)

	_, err = telegramPeer.Write(downstream)
	require.NoError(t, err)

	messageType, encryptedDownstream, err := wsConn.Read(context.Background())
	require.NoError(t, err)
	require.Equal(t, websocket.MessageBinary, messageType)

	receivedDownstream := make([]byte, len(downstream))
	readDone := make(chan error, 1)
	go func() {
		_, readErr := io.ReadFull(obfuscatedClient, receivedDownstream)
		readDone <- readErr
	}()
	_, err = rawClientPeer.Write(encryptedDownstream)
	require.NoError(t, err)
	require.NoError(t, <-readDone)
	require.Equal(t, expectedDownstream, receivedDownstream)

	require.NoError(t, obfuscatedClient.Close())
	rawClientPeer.Close() //nolint: errcheck
	telegramPeer.Close()  //nolint: errcheck
}

func TestWebSocketRelayRejectsTextAndOversizedMessages(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		maxMessageSize int64
		messageType    websocket.MessageType
		payload        []byte
		status         websocket.StatusCode
	}{
		{
			name:           "text",
			maxMessageSize: 16 << 20,
			messageType:    websocket.MessageText,
			payload:        []byte("not binary"),
			status:         websocket.StatusUnsupportedData,
		},
		{
			name:           "oversized",
			maxMessageSize: 63,
			messageType:    websocket.MessageBinary,
			payload:        make([]byte, 64),
			status:         websocket.StatusMessageTooBig,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			fixture := newWebSocketRelayFixture(t, 1, testCase.maxMessageSize)
			wsConn := fixture.dialValid()
			require.NoError(t, wsConn.Write(context.Background(), testCase.messageType, testCase.payload))

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			_, _, err := wsConn.Read(ctx)
			require.Error(t, err)
			require.Equal(t, testCase.status, websocket.CloseStatus(err))

			dialCount, _, _ := fixture.network.dial()
			require.Zero(t, dialCount)
		})
	}
}

func TestWebSocketRelayConnectionSlotIsReleased(t *testing.T) {
	t.Parallel()

	fixture := newWebSocketRelayFixture(t, 1, 16<<20)
	first := fixture.dialValid()

	second, response, err := fixture.dial(fixture.tokens[0], nil)
	if second != nil {
		second.CloseNow() //nolint: errcheck
	}
	requireWebSocketRelayRejected(t, response, err)

	require.NoError(t, first.Close(websocket.StatusNormalClosure, ""))

	require.Eventually(t, func() bool {
		third, _, dialErr := fixture.dial(fixture.tokens[0], nil)
		if dialErr != nil {
			return false
		}
		third.CloseNow() //nolint: errcheck
		return true
	}, time.Second, 10*time.Millisecond)
}

func TestWebSocketRelayShutdownClosesActiveSession(t *testing.T) {
	t.Parallel()

	fixture := newWebSocketRelayFixture(t, 1, 16<<20)
	wsConn := fixture.dialValid()

	require.Eventually(t, func() bool {
		events := fixture.eventStream.snapshot()
		return len(events) > 0
	}, time.Second, 10*time.Millisecond)

	fixture.proxy.ctxCancel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, _, err := wsConn.Read(ctx)
	require.Error(t, err)
	require.Equal(t, websocket.StatusGoingAway, websocket.CloseStatus(err))
}

func TestWebSocketRelayRejectsMultipleAuthorizationFieldsBeforeUpgrade(t *testing.T) {
	t.Parallel()

	fixture := newWebSocketRelayFixture(t, 1, 16<<20)
	client := &http.Client{Transport: webSocketRelayRequestRoundTripper(func(request *http.Request) (*http.Response, error) {
		request.Header["Authorization"] = []string{
			"Bearer " + fixture.tokens[0],
			"Bearer " + fixture.tokens[0],
		}

		return http.DefaultTransport.RoundTrip(request)
	})}

	conn, response, err := websocket.Dial(context.Background(), fixture.webSocketURL(), &websocket.DialOptions{
		HTTPClient:   client,
		HTTPHeader:   http.Header{"X-Real-IP": []string{testWebSocketRelayClientIP}},
		Subprotocols: []string{webSocketRelaySubprotocol},
	})
	if conn != nil {
		conn.CloseNow() //nolint: errcheck
	}
	requireWebSocketRelayRejected(t, response, err)
}
