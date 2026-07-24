package mtglib

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/9seconds/mtg/v2/essentials"
	"github.com/9seconds/mtg/v2/mtglib/internal/dc"
	"github.com/9seconds/mtg/v2/mtglib/internal/doppel"
	"github.com/9seconds/mtg/v2/mtglib/internal/obfuscation"
	"github.com/9seconds/mtg/v2/mtglib/internal/relay"
	"github.com/9seconds/mtg/v2/mtglib/internal/tls"
	"github.com/9seconds/mtg/v2/mtglib/internal/tls/fake"
	"github.com/panjf2000/ants/v2"
)

type ingressMode uint8

const (
	stockFakeTLS ingressMode = iota
	webSocketRelay
)

type authenticatedStreamPolicy struct {
	clientObfuscator         obfuscation.Obfuscator
	allowFallbackOnUnknownDC bool
	handshakeReadCounter     *countingReadConn
}

type countingReadConn struct {
	essentials.Conn
	bytesRead int
}

func (c *countingReadConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.bytesRead += n

	return n, err
}

func (c *countingReadConn) BytesRead() int {
	return c.bytesRead
}

func obfuscatedHandshakeFailureCategory(err error) string {
	var netErr net.Error

	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		return "read-timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "peer-closed"
	case strings.Contains(err.Error(), "unsupported connection type:"):
		return "unsupported-connection-type"
	default:
		return "read-or-parse-error"
	}
}

func obfuscatedHandshakeConnectionType(err error) string {
	const marker = "unsupported connection type: "

	errMessage := err.Error()
	markerIndex := strings.LastIndex(errMessage, marker)
	if markerIndex == -1 {
		return ""
	}

	connectionType := errMessage[markerIndex+len(marker):]
	if len(connectionType) != 8 {
		return ""
	}
	for _, value := range connectionType {
		if !strings.ContainsRune("0123456789abcdef", value) {
			return ""
		}
	}

	return connectionType
}

// Proxy is an MTPROTO proxy structure.
type Proxy struct {
	ctx             context.Context
	ctxCancel       context.CancelFunc
	streamWaitGroup sync.WaitGroup

	allowFallbackOnUnknownDC    bool
	tolerateTimeSkewness        time.Duration
	idleTimeout                 time.Duration
	handshakeTimeout            time.Duration
	domainFrontingPort          int
	domainFrontingIP            string
	domainFrontingProxyProtocol bool
	workerPool                  *ants.PoolWithFunc
	telegram                    *dc.Telegram
	configUpdater               *dc.PublicConfigUpdater
	doppelGanger                *doppel.Ganger
	clientObfuscatror           obfuscation.Obfuscator

	secret          Secret
	network         Network
	antiReplayCache AntiReplayCache
	blocklist       IPBlocklist
	allowlist       IPBlocklist
	eventStream     EventStream
	logger          Logger
}

// DomainFrontingAddress returns a host:port pair for a fronting domain.
// If DomainFrontingIP is set, it is used instead of resolving the hostname.
func (p *Proxy) DomainFrontingAddress() string {
	host := p.secret.Host
	if p.domainFrontingIP != "" {
		host = p.domainFrontingIP
	}

	return net.JoinHostPort(host, strconv.Itoa(p.domainFrontingPort))
}

// ServeConn serves a connection. We do not check IP blocklist and concurrency
// limit here.
func (p *Proxy) ServeConn(conn essentials.Conn) {
	clientIP := conn.RemoteAddr().(*net.TCPAddr).IP //nolint: forcetypeassert

	p.serveConn(conn, clientIP, stockFakeTLS)
}

func (p *Proxy) serveConn(conn essentials.Conn, clientIP net.IP, ingress ingressMode) {
	p.streamWaitGroup.Add(1)
	defer p.streamWaitGroup.Done()

	policy := authenticatedStreamPolicy{}
	if ingress == webSocketRelay {
		readCounter := &countingReadConn{Conn: conn}
		conn = readCounter
		policy.clientObfuscator = obfuscation.Obfuscator{Secret: nil}
		policy.handshakeReadCounter = readCounter
	}

	ctx := newStreamContext(p.ctx, p.logger, conn, clientIP)
	defer ctx.Close()

	if err := ctx.clientConn.SetDeadline(time.Now().Add(p.handshakeTimeout)); err != nil {
		ctx.logger.WarningError("cannot set handshake timeout", err)
		return
	}

	stop := context.AfterFunc(ctx, func() {
		ctx.Close()
	})
	defer stop()

	p.eventStream.Send(ctx, NewEventStart(ctx.streamID, ctx.ClientIP()))
	ctx.logger.Info("Stream has been started")

	defer func() {
		p.eventStream.Send(ctx, NewEventFinish(ctx.streamID))
		ctx.logger.Info("Stream has been finished")
	}()

	switch ingress {
	case stockFakeTLS:
		if !p.doFakeTLSHandshake(ctx) {
			return
		}

		clientConn, err := p.doppelGanger.NewConn(ctx.clientConn)
		if err != nil {
			ctx.logger.InfoError("cannot wrap into doppelganger connection", err)
			return
		}
		defer clientConn.Stop()

		ctx.clientConn = clientConn
		policy.clientObfuscator = p.clientObfuscatror
		policy.allowFallbackOnUnknownDC = p.allowFallbackOnUnknownDC
	case webSocketRelay:
	default:
		panic(fmt.Sprintf("unsupported ingress mode: %d", ingress))
	}

	p.serveAuthenticatedStream(ctx, policy)
}

func (p *Proxy) serveAuthenticatedStream(ctx *streamContext, policy authenticatedStreamPolicy) {
	if err := p.doObfuscatedHandshake(ctx, policy.clientObfuscator); err != nil {
		if policy.handshakeReadCounter != nil {
			logger := ctx.logger.
				BindInt("bytes-read", policy.handshakeReadCounter.BytesRead()).
				BindStr("failure-category", obfuscatedHandshakeFailureCategory(err))

			if connectionType := obfuscatedHandshakeConnectionType(err); connectionType != "" {
				logger = logger.BindStr("connection-type", connectionType)
			}

			logger.Debug("WebSocket obfuscated handshake diagnostic")
		}

		ctx.logger.InfoError("obfuscated handshake is failed", err)
		return
	}

	if err := ctx.clientConn.SetDeadline(time.Time{}); err != nil {
		ctx.logger.WarningError("cannot set deadline", err)
		return
	}

	if policy.handshakeReadCounter != nil && len(p.telegram.GetAddresses(ctx.dc)) == 0 {
		ctx.logger.
			BindInt("bytes-read", policy.handshakeReadCounter.BytesRead()).
			BindStr("failure-category", "invalid-dc").
			Debug("WebSocket obfuscated handshake diagnostic")
	}

	if err := p.doTelegramCall(ctx, policy.allowFallbackOnUnknownDC); err != nil {
		ctx.logger.WarningError("cannot dial to telegram", err)
		return
	}

	tracker := newIdleTracker(p.idleTimeout)

	relay.Relay(
		ctx,
		ctx.logger.Named("relay"),
		connIdleTimeout{Conn: ctx.telegramConn, tracker: tracker},
		connIdleTimeout{Conn: ctx.clientConn, tracker: tracker},
	)
}

// Serve starts a proxy on a given listener.
func (p *Proxy) Serve(listener net.Listener) error {
	p.streamWaitGroup.Add(1)
	defer p.streamWaitGroup.Done()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-p.ctx.Done():
				return nil
			default:
				return fmt.Errorf("cannot accept a new connection: %w", err)
			}
		}

		ipAddr := conn.RemoteAddr().(*net.TCPAddr).IP //nolint: forcetypeassert
		logger := p.logger.BindStr("ip", ipAddr.String())

		if !p.allowClientIP(ipAddr) {
			conn.Close() //nolint: errcheck
			continue
		}

		err = p.workerPool.Invoke(conn)

		switch {
		case err == nil:
		case errors.Is(err, ants.ErrPoolClosed):
			return nil
		case errors.Is(err, ants.ErrPoolOverload):
			conn.Close() //nolint: errcheck
			logger.Info("connection was concurrency limited")
			p.eventStream.Send(p.ctx, NewEventConcurrencyLimited())
		}
	}
}

func (p *Proxy) allowClientIP(clientIP net.IP) bool {
	logger := p.logger.BindStr("ip", clientIP.String())

	if !p.allowlist.Contains(clientIP) {
		logger.Info("ip was rejected by allowlist")
		p.eventStream.Send(p.ctx, NewEventIPAllowlisted(clientIP))

		return false
	}

	if p.blocklist.Contains(clientIP) {
		logger.Info("ip was blacklisted")
		p.eventStream.Send(p.ctx, NewEventIPBlocklisted(clientIP))

		return false
	}

	return true
}

// Shutdown 'gracefully' shutdowns all connections. Please remember that it
// does not close an underlying listener.
func (p *Proxy) Shutdown() {
	p.ctxCancel()
	p.streamWaitGroup.Wait()
	p.workerPool.Release()
	p.configUpdater.Wait()
	p.doppelGanger.Shutdown()

	p.allowlist.Shutdown()
	p.blocklist.Shutdown()
}

func (p *Proxy) doFakeTLSHandshake(ctx *streamContext) bool {
	rewind := newConnRewind(ctx.clientConn)

	clientHello, err := fake.ReadClientHello(
		rewind,
		p.secret.Key[:],
		p.secret.Host,
		p.tolerateTimeSkewness,
	)
	if err != nil {
		p.logger.InfoError("cannot read client hello", err)
		p.doDomainFronting(ctx, rewind)
		return false
	}

	if p.antiReplayCache.SeenBefore(clientHello.SessionID) {
		p.logger.Warning("replay attack has been detected!")
		p.eventStream.Send(p.ctx, NewEventReplayAttack(ctx.streamID))
		p.doDomainFronting(ctx, rewind)
		return false
	}

	gangerNoise := p.doppelGanger.NoiseParams()
	noiseParams := fake.NoiseParams{Mean: gangerNoise.Mean, Jitter: gangerNoise.Jitter}

	if err := fake.SendServerHello(ctx.clientConn, p.secret.Key[:], clientHello, noiseParams); err != nil {
		p.logger.InfoError("cannot send welcome packet", err)
		return false
	}

	ctx.clientConn = tls.New(ctx.clientConn, true, false)

	return true
}

func (p *Proxy) doObfuscatedHandshake(
	ctx *streamContext,
	clientObfuscator obfuscation.Obfuscator,
) error {
	dc, conn, err := clientObfuscator.ReadHandshake(ctx.clientConn)
	if err != nil {
		return fmt.Errorf("cannot process client handshake: %w", err)
	}

	ctx.dc = dc
	ctx.clientConn = conn
	ctx.logger = ctx.logger.BindInt("dc", dc)

	return nil
}

func (p *Proxy) doTelegramCall(ctx *streamContext, allowFallbackOnUnknownDC bool) error {
	dcid := ctx.dc

	addresses := p.telegram.GetAddresses(dcid)
	if len(addresses) == 0 && allowFallbackOnUnknownDC {
		ctx.logger = ctx.logger.BindInt("original_dc", dcid)
		ctx.logger.Warning("unknown DC, fallbacks")
		ctx.dc = dc.DefaultDC
		addresses = p.telegram.GetAddresses(dc.DefaultDC)
	}

	var (
		conn      essentials.Conn
		err       error
		foundAddr dc.Addr
	)

	for _, addr := range addresses {
		conn, err = p.network.DialContext(ctx, addr.Network, addr.Address)
		if err == nil {
			foundAddr = addr
			break
		}
	}
	if err != nil {
		return fmt.Errorf("no addresses to call: %w", err)
	}
	if conn == nil {
		return fmt.Errorf("no available addresses for DC %d", ctx.dc)
	}

	tgConn, err := foundAddr.Obfuscator.SendHandshake(conn, ctx.dc)
	if err != nil {
		conn.Close() // nolint: errcheck
		return fmt.Errorf("cannot perform server handshake: %w", err)
	}

	ctx.telegramConn = connTraffic{
		Conn:     tgConn,
		streamID: ctx.streamID,
		stream:   p.eventStream,
		ctx:      ctx,
	}

	telegramHost, _, err := net.SplitHostPort(foundAddr.Address)
	if err != nil {
		conn.Close() //nolint: errcheck

		return fmt.Errorf("cannot parse telegram address %s: %w", foundAddr.Address, err)
	}

	p.eventStream.Send(ctx,
		NewEventConnectedToDC(ctx.streamID,
			net.ParseIP(telegramHost),
			ctx.dc),
	)

	return nil
}

func (p *Proxy) doDomainFronting(ctx *streamContext, conn *connRewind) {
	p.eventStream.Send(p.ctx, NewEventDomainFronting(ctx.streamID))
	conn.Rewind()

	nativeDialer := p.network.NativeDialer()
	fConn, err := nativeDialer.DialContext(ctx, "tcp", p.DomainFrontingAddress())
	if err != nil {
		p.logger.WarningError("cannot dial to the fronting domain", err)

		return
	}

	frontConn := essentials.WrapNetConn(fConn)

	if p.domainFrontingProxyProtocol {
		frontConn = newConnProxyProtocol(ctx.clientConn, frontConn)
	}

	frontConn = connTraffic{
		Conn:     frontConn,
		ctx:      ctx,
		streamID: ctx.streamID,
		stream:   p.eventStream,
	}

	tracker := newIdleTracker(p.idleTimeout)

	relay.Relay(
		ctx,
		ctx.logger.Named("domain-fronting"),
		connIdleTimeout{Conn: frontConn, tracker: tracker},
		connIdleTimeout{Conn: conn, tracker: tracker},
	)
}

// NewProxy makes a new proxy instance.
func NewProxy(opts ProxyOpts) (*Proxy, error) {
	if err := opts.valid(); err != nil {
		return nil, fmt.Errorf("invalid settings: %w", err)
	}

	tg, err := dc.New(opts.getPreferIP())
	if err != nil {
		return nil, fmt.Errorf("cannot build telegram dc fetcher: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	logger := opts.getLogger("proxy")
	updatersLogger := logger.Named("telegram-updaters")

	proxy := &Proxy{
		ctx:                      ctx,
		ctxCancel:                cancel,
		secret:                   opts.Secret,
		network:                  opts.Network,
		antiReplayCache:          opts.AntiReplayCache,
		blocklist:                opts.IPBlocklist,
		allowlist:                opts.IPAllowlist,
		eventStream:              opts.EventStream,
		logger:                   logger,
		domainFrontingPort:       opts.getDomainFrontingPort(),
		domainFrontingIP:         opts.DomainFrontingIP,
		tolerateTimeSkewness:     opts.getTolerateTimeSkewness(),
		idleTimeout:              opts.getIdleTimeout(),
		handshakeTimeout:         opts.getHandshakeTimeout(),
		allowFallbackOnUnknownDC: opts.AllowFallbackOnUnknownDC,
		telegram:                 tg,
		doppelGanger: doppel.NewGanger(
			ctx,
			opts.Network,
			logger.Named("doppelganger"),
			opts.DoppelGangerEach,
			int(opts.DoppelGangerPerRaid),
			opts.DoppelGangerURLs,
			opts.DoppelGangerDRS,
		),
		configUpdater: dc.NewPublicConfigUpdater(
			tg,
			updatersLogger.Named("public-config"),
			opts.Network.MakeHTTPClient(nil),
		),
		clientObfuscatror: obfuscation.Obfuscator{
			Secret: opts.Secret.Key[:],
		},
		domainFrontingProxyProtocol: opts.DomainFrontingProxyProtocol,
	}

	proxy.doppelGanger.Run()

	if opts.AutoUpdate {
		proxy.configUpdater.Run(ctx, dc.PublicConfigUpdateURLv4, "tcp4")
		proxy.configUpdater.Run(ctx, dc.PublicConfigUpdateURLv6, "tcp6")
	}

	pool, err := ants.NewPoolWithFunc(opts.getConcurrency(),
		func(arg any) {
			proxy.ServeConn(arg.(essentials.Conn)) //nolint: forcetypeassert
		},
		ants.WithLogger(opts.getLogger("ants")),
		ants.WithNonblocking(true))
	if err != nil {
		panic(err)
	}

	proxy.workerPool = pool

	return proxy, nil
}
