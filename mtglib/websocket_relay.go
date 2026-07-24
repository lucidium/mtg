package mtglib

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/9seconds/mtg/v2/essentials"
	"github.com/coder/websocket"
)

const (
	webSocketRelaySubprotocol   = "telegram-mtproto-relay.v1"
	webSocketRelayMaxTokens     = 4
	webSocketRelayMinTokenBytes = 32
	webSocketRelayMaxTokenBytes = 64
	webSocketRelayRejectionBody = "not found\n"
	webSocketRelayRejectionCode = http.StatusNotFound
)

// WebSocketRelayOptions configures an authenticated WebSocket relay handler.
// TLS termination and the HTTP server lifecycle remain the caller's
// responsibility.
type WebSocketRelayOptions struct {
	Path           string
	Tokens         []string
	MaxConnections int
	MaxMessageSize int64
}

type webSocketRelayHandler struct {
	proxy           *Proxy
	path            string
	tokenDigests    [][sha256.Size]byte
	connectionSlots chan struct{}
	maxMessageSize  int64
}

type webSocketRelayConn struct {
	webSocket net.Conn

	ctx            context.Context
	conn           *websocket.Conn
	status         atomic.Int32
	maxMessageSize int64
	closeOnce      sync.Once
	closeErr       error
}

var _ essentials.Conn = (*webSocketRelayConn)(nil)

// NewWebSocketRelayHandler builds an authenticated WebSocket ingress for the
// proxy. The returned handler must be served only through a loopback HTTP
// listener behind a trusted reverse proxy.
func (p *Proxy) NewWebSocketRelayHandler(opts WebSocketRelayOptions) (http.Handler, error) {
	if opts.Path == "" || !strings.HasPrefix(opts.Path, "/") {
		return nil, errors.New("websocket relay path must be absolute")
	}
	if opts.MaxConnections <= 0 {
		return nil, errors.New("websocket relay max connections must be positive")
	}
	if opts.MaxMessageSize <= 0 {
		return nil, errors.New("websocket relay max message size must be positive")
	}

	tokenDigests, err := makeWebSocketRelayTokenDigests(opts.Tokens)
	if err != nil {
		return nil, err
	}

	return &webSocketRelayHandler{
		proxy:           p,
		path:            opts.Path,
		tokenDigests:    tokenDigests,
		connectionSlots: make(chan struct{}, opts.MaxConnections),
		maxMessageSize:  opts.MaxMessageSize,
	}, nil
}

func makeWebSocketRelayTokenDigests(tokens []string) ([][sha256.Size]byte, error) {
	if len(tokens) == 0 || len(tokens) > webSocketRelayMaxTokens {
		return nil, fmt.Errorf(
			"websocket relay requires between 1 and %d tokens",
			webSocketRelayMaxTokens,
		)
	}

	digests := make([][sha256.Size]byte, 0, len(tokens))

	for index, token := range tokens {
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != token {
			return nil, fmt.Errorf("websocket relay token %d is not unpadded base64url", index+1)
		}
		if len(decoded) < webSocketRelayMinTokenBytes || len(decoded) > webSocketRelayMaxTokenBytes {
			return nil, fmt.Errorf(
				"websocket relay token %d must decode to between %d and %d bytes",
				index+1,
				webSocketRelayMinTokenBytes,
				webSocketRelayMaxTokenBytes,
			)
		}

		digest := sha256.Sum256([]byte(token))
		for _, existingDigest := range digests {
			if subtle.ConstantTimeCompare(existingDigest[:], digest[:]) == 1 {
				return nil, fmt.Errorf("websocket relay token %d is duplicated", index+1)
			}
		}

		digests = append(digests, digest)
	}

	return digests, nil
}

func (h *webSocketRelayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	peerIP, ok := webSocketRelayPeerIP(r.RemoteAddr)
	if !ok || !peerIP.IsLoopback() {
		rejectWebSocketRelay(w)
		return
	}

	if r.URL.Path != h.path || r.URL.EscapedPath() != h.path || r.URL.RawQuery != "" || r.URL.ForceQuery {
		rejectWebSocketRelay(w)
		return
	}

	if r.Method != http.MethodGet {
		rejectWebSocketRelay(w)
		return
	}

	token, ok := webSocketRelayBearerToken(r.Header)
	if !ok || !h.matchesToken(token) {
		rejectWebSocketRelay(w)
		return
	}

	if !webSocketRelayOffersSubprotocol(r.Header) {
		rejectWebSocketRelay(w)
		return
	}

	clientIP, ok := webSocketRelayClientIP(r.Header)
	if !ok || !h.proxy.allowClientIP(clientIP) {
		rejectWebSocketRelay(w)
		return
	}

	select {
	case h.connectionSlots <- struct{}{}:
		defer func() { <-h.connectionSlots }()
	default:
		rejectWebSocketRelay(w)
		return
	}

	// Native relay clients do not send Origin. Rejecting every present Origin
	// keeps browser-origin behavior explicit instead of enabling permissive
	// cross-origin matching.
	if len(r.Header.Values("Origin")) != 0 || !validWebSocketUpgradeRequest(r) {
		rejectWebSocketRelay(w)
		return
	}

	select {
	case <-h.proxy.ctx.Done():
		rejectWebSocketRelay(w)
		return
	default:
	}

	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{webSocketRelaySubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}

	clientConn := &webSocketRelayConn{
		webSocket:      newWebSocketRelayStreamConn(wsConn, websocket.MessageBinary),
		ctx:            h.proxy.ctx,
		conn:           wsConn,
		maxMessageSize: h.maxMessageSize,
	}
	// NetConn disables coder/websocket's read limit. Keep its limit disabled and
	// enforce the bound here so a message ending exactly at a stream read
	// boundary cannot bypass the per-message limit.
	clientConn.status.Store(int32(websocket.StatusNormalClosure))

	h.proxy.serveConn(clientConn, clientIP, webSocketRelay)
}

func (h *webSocketRelayHandler) matchesToken(token string) bool {
	presentedDigest := sha256.Sum256([]byte(token))
	matched := 0

	for _, configuredDigest := range h.tokenDigests {
		matched |= subtle.ConstantTimeCompare(configuredDigest[:], presentedDigest[:])
	}

	return matched == 1
}

func webSocketRelayPeerIP(remoteAddr string) (net.IP, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return nil, false
	}

	ip := net.ParseIP(host)

	return ip, ip != nil
}

func webSocketRelayBearerToken(header http.Header) (string, bool) {
	values := header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}

	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}

	return parts[1], true
}

func webSocketRelayOffersSubprotocol(header http.Header) bool {
	for _, value := range header.Values("Sec-WebSocket-Protocol") {
		for offered := range strings.SplitSeq(value, ",") {
			if strings.TrimSpace(offered) == webSocketRelaySubprotocol {
				return true
			}
		}
	}

	return false
}

func webSocketRelayClientIP(header http.Header) (net.IP, bool) {
	values := header.Values("X-Real-IP")
	if len(values) != 1 {
		return nil, false
	}

	ip := net.ParseIP(strings.TrimSpace(values[0]))

	return ip, ip != nil
}

func validWebSocketUpgradeRequest(r *http.Request) bool {
	if !r.ProtoAtLeast(1, 1) ||
		!webSocketHeaderContainsToken(r.Header, "Connection", "Upgrade") ||
		!webSocketHeaderContainsToken(r.Header, "Upgrade", "websocket") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" {
		return false
	}

	keys := r.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 {
		return false
	}

	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keys[0]))

	return err == nil && len(key) == 16
}

func webSocketHeaderContainsToken(header http.Header, key, expected string) bool {
	for _, value := range header.Values(key) {
		for token := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), expected) {
				return true
			}
		}
	}

	return false
}

func rejectWebSocketRelay(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(webSocketRelayRejectionCode)
	_, _ = w.Write([]byte(webSocketRelayRejectionBody))
}

func (c *webSocketRelayConn) Read(p []byte) (int, error) {
	readBuffer := p
	if int64(len(p)) > c.maxMessageSize {
		readBuffer = p[:c.maxMessageSize]
	}

	n, err := c.webSocket.Read(readBuffer)
	if int64(n) == c.maxMessageSize {
		var extraByte [1]byte
		extraN, extraErr := c.webSocket.Read(extraByte[:])
		if extraN != 0 {
			c.status.Store(int32(websocket.StatusMessageTooBig))
			return 0, websocket.ErrMessageTooBig
		}
		if extraErr != nil {
			err = extraErr
		}
	}
	if errors.Is(err, websocket.ErrMessageTooBig) ||
		websocket.CloseStatus(err) == websocket.StatusMessageTooBig {
		c.status.Store(int32(websocket.StatusMessageTooBig))
	}

	return n, err
}

func (c *webSocketRelayConn) CloseRead() error {
	return c.Close()
}

func (c *webSocketRelayConn) CloseWrite() error {
	return nil
}

func (c *webSocketRelayConn) Close() error {
	c.closeOnce.Do(func() {
		status := websocket.StatusCode(c.status.Load())
		if c.ctx.Err() != nil {
			status = websocket.StatusGoingAway
		}

		webSocketErr := c.conn.Close(status, "")
		c.closeErr = webSocketErr
	})

	return c.closeErr
}

func (c *webSocketRelayConn) Write(p []byte) (int, error) {
	return c.webSocket.Write(p)
}

func (c *webSocketRelayConn) LocalAddr() net.Addr {
	return c.webSocket.LocalAddr()
}

func (c *webSocketRelayConn) RemoteAddr() net.Addr {
	return c.webSocket.RemoteAddr()
}

func (c *webSocketRelayConn) SetDeadline(deadline time.Time) error {
	return c.webSocket.SetDeadline(deadline)
}

func (c *webSocketRelayConn) SetReadDeadline(deadline time.Time) error {
	return c.webSocket.SetReadDeadline(deadline)
}

func (c *webSocketRelayConn) SetWriteDeadline(deadline time.Time) error {
	return c.webSocket.SetWriteDeadline(deadline)
}

func newWebSocketRelayStreamConn(conn *websocket.Conn, messageType websocket.MessageType) net.Conn {
	return websocket.NetConn(context.Background(), conn, messageType)
}
