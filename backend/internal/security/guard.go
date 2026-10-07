package security

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"fixlab.dev/fixlab/backend/internal/common"
)

const soh = 0x01

// frameInfo summarizes one complete FIX frame observed on the wire.
type frameInfo struct {
	length     int
	msgType    string
	heartBtInt int
	hasHeartBt bool
}

// frameParser incrementally scans a byte stream for FIX frames. It is
// deliberately byte-level: it enforces the maximum frame size and sniffs
// the 35= (MsgType) and 108= (HeartBtInt) fields. It does NOT implement
// any FIX session semantics — those belong to the FIX engine.
type frameParser struct {
	maxBytes int

	tagBuf      []byte
	valBuf      []byte
	inValue     bool
	frameLen    int
	curMsgType  string
	curHeartBt  int
	curHasHeart bool
}

// newFrameParser creates a parser enforcing maxBytes per frame.
func newFrameParser(maxBytes int) *frameParser {
	return &frameParser{maxBytes: maxBytes}
}

func (p *frameParser) resetFrame() {
	p.tagBuf = p.tagBuf[:0]
	p.valBuf = p.valBuf[:0]
	p.inValue = false
	p.frameLen = 0
	p.curMsgType = ""
	p.curHeartBt = 0
	p.curHasHeart = false
}

// Write consumes chunk and returns completed frames. oversized is true
// when the in-progress frame exceeded maxBytes; hbViolation is true when
// a Logon frame carried a HeartBtInt below minHeartBtInt.
func (p *frameParser) Write(chunk []byte, minHeartBtInt int) (frames []frameInfo, oversized, hbViolation bool) {
	for _, b := range chunk {
		p.frameLen++
		if p.frameLen > p.maxBytes {
			return frames, true, false
		}
		if !p.inValue {
			if b == '=' {
				p.inValue = true
			} else {
				p.tagBuf = append(p.tagBuf, b)
			}
			continue
		}
		if b != soh {
			p.valBuf = append(p.valBuf, b)
			continue
		}
		// Field complete: tag=value<SOH>.
		tag := string(p.tagBuf)
		switch tag {
		case "35":
			p.curMsgType = string(p.valBuf)
		case "108":
			if n, err := strconv.Atoi(string(p.valBuf)); err == nil {
				p.curHeartBt = n
				p.curHasHeart = true
			}
		case "10":
			// Checksum field ends the frame.
			frames = append(frames, frameInfo{
				length:     p.frameLen,
				msgType:    p.curMsgType,
				heartBtInt: p.curHeartBt,
				hasHeartBt: p.curHasHeart,
			})
			if p.curMsgType == "A" && p.curHasHeart && p.curHeartBt < minHeartBtInt {
				hbViolation = true
			}
			p.resetFrame()
			continue
		}
		p.tagBuf = p.tagBuf[:0]
		p.valBuf = p.valBuf[:0]
		p.inValue = false
	}
	return frames, false, hbViolation
}

// GuardConfig configures one TCP guard proxy instance.
type GuardConfig struct {
	// PublicPort is the externally reachable port to listen on.
	PublicPort int
	// BackendAddr is the loopback address of the real FIX acceptor,
	// e.g. "127.0.0.1:54321".
	BackendAddr string
	// SessionLabel identifies the session in logs.
	SessionLabel  string
	MaxFrameBytes int
	IdleTimeout   time.Duration
	LogonTimeout  time.Duration
	MinHeartBtInt int
	// ConnLimiter rate-limits connection attempts per remote IP.
	ConnLimiter *RateLimiter
	Metrics     *common.Metrics
	Logger      *slog.Logger
	// OnConnect/OnDisconnect report TCP-level session state changes.
	// OnInboundLogon fires when a 35=A frame is seen from the client.
	OnConnect      func()
	OnDisconnect   func()
	OnInboundLogon func()
	// TLSConfig, when non-nil, terminates TLS at the edge: the public
	// listener decrypts (MinVersion TLS 1.2, stdlib only) and the guard
	// keeps enforcing its byte-level protections on the decrypted
	// stream (phase 7, spec §15). Nil means plaintext TCP.
	TLSConfig *tls.Config
	// TLSHandshakeTimeout bounds the TLS handshake per connection.
	// Zero selects the 10s default.
	TLSHandshakeTimeout time.Duration
}

// Guard is a TCP-level shield in front of a QuickFIX/Go acceptor. It owns
// the public listener and forwards bytes to the loopback acceptor,
// enforcing per-IP connection rate limits, a maximum FIX frame size,
// idle timeouts, a logon grace period, and the minimum heartbeat
// interval. It never interprets FIX session state beyond framing.
type Guard struct {
	cfg      GuardConfig
	listener net.Listener
	tlsMode  bool
	wg       sync.WaitGroup
	closed   atomic.Bool
}

// NewGuard binds the public port immediately; the bind is authoritative
// for port availability (spec §13). When GuardConfig.TLSConfig is set,
// the public port terminates TLS at the edge (tls.Listener, MinVersion
// TLS 1.2); decrypted bytes flow through the same guard pipeline, so
// frame caps, rate limits, and timeouts apply identically (phase 7).
func NewGuard(cfg GuardConfig) (*Guard, error) {
	if cfg.Metrics == nil {
		cfg.Metrics = common.DefaultMetrics()
	}
	if cfg.Logger == nil {
		cfg.Logger = common.NewLogger()
	}
	if cfg.TLSHandshakeTimeout <= 0 {
		cfg.TLSHandshakeTimeout = 10 * time.Second
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.PublicPort)))
	if err != nil {
		return nil, fmt.Errorf("guard: listen on port %d: %w", cfg.PublicPort, err)
	}
	var listener net.Listener = ln
	tlsMode := false
	if cfg.TLSConfig != nil {
		listener = tls.NewListener(ln, cfg.TLSConfig)
		tlsMode = true
	}
	g := &Guard{cfg: cfg, listener: listener, tlsMode: tlsMode}
	g.wg.Add(1)
	go g.acceptLoop()
	return g, nil
}

// Port returns the bound public port.
func (g *Guard) Port() int {
	if a, ok := g.listener.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return g.cfg.PublicPort
}

// Close shuts down the listener; in-flight connections drain.
func (g *Guard) Close() error {
	if g.closed.Swap(true) {
		return nil
	}
	err := g.listener.Close()
	g.wg.Wait()
	return err
}

func remoteIP(c net.Conn) string {
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		return a.IP.String()
	}
	return c.RemoteAddr().String()
}

func (g *Guard) acceptLoop() {
	defer g.wg.Done()
	for {
		conn, err := g.listener.Accept()
		if err != nil {
			if !g.closed.Load() {
				g.cfg.Logger.Error("guard: accept failed", "session", g.cfg.SessionLabel, "err", err)
			}
			return
		}
		g.wg.Add(1)
		go g.handle(conn)
	}
}

// guardConn tracks one proxied TCP connection.
type guardConn struct {
	g        *Guard
	client   net.Conn
	backend  net.Conn
	loggedOn atomic.Bool
	once     sync.Once
	// timer is an atomic pointer because the runtime timer goroutine is
	// not happens-before ordered with the field assignment in handle().
	timer atomic.Pointer[time.Timer]
}

func (g *Guard) handle(client net.Conn) {
	defer g.wg.Done()
	ip := remoteIP(client)

	if g.cfg.ConnLimiter != nil && !g.cfg.ConnLimiter.Allow(ip) {
		g.cfg.Metrics.RejectedConnections.Add(1)
		g.cfg.Metrics.GuardRateLimited.Add(1)
		g.cfg.Logger.Warn("guard: connection rate-limited",
			"session", g.cfg.SessionLabel, "remote_ip", ip)
		client.Close()
		return
	}

	// TLS mode: the rate limit above ran pre-handshake (cheap, no
	// crypto spent on abusive IPs). Now complete the handshake with a
	// deadline; plaintext sent to a TLS port fails here and the
	// connection is dropped without ever reaching the FIX engine.
	if g.tlsMode {
		tc, ok := client.(*tls.Conn)
		if !ok {
			g.cfg.Logger.Error("guard: TLS mode but non-TLS connection",
				"session", g.cfg.SessionLabel, "remote_ip", ip)
			client.Close()
			return
		}
		_ = tc.SetDeadline(time.Now().Add(g.cfg.TLSHandshakeTimeout))
		if err := tc.Handshake(); err != nil {
			g.cfg.Metrics.TLSHandshakeFails.Add(1)
			g.cfg.Logger.Warn("guard: TLS handshake failed",
				"session", g.cfg.SessionLabel, "remote_ip", ip, "err", err)
			client.Close()
			return
		}
		// Clear the handshake deadline; the pump sets its own
		// per-read deadlines from here on.
		_ = tc.SetDeadline(time.Time{})
	}

	backend, err := net.DialTimeout("tcp", g.cfg.BackendAddr, 5*time.Second)
	if err != nil {
		g.cfg.Logger.Error("guard: backend dial failed",
			"session", g.cfg.SessionLabel, "backend", g.cfg.BackendAddr, "err", err)
		client.Close()
		return
	}

	g.cfg.Metrics.AcceptedConnections.Add(1)
	if g.cfg.OnConnect != nil {
		g.cfg.OnConnect()
	}
	c := &guardConn{g: g, client: client, backend: backend}
	c.timer.Store(time.AfterFunc(g.cfg.LogonTimeout, func() {
		if !c.loggedOn.Load() {
			g.cfg.Metrics.GuardLogonTimeouts.Add(1)
			g.cfg.Logger.Warn("guard: logon timeout, closing connection",
				"session", g.cfg.SessionLabel, "remote_ip", ip)
			c.close()
		}
	}))

	var pumpWg sync.WaitGroup
	pumpWg.Add(2)
	go func() { defer pumpWg.Done(); g.pump(c, client, backend, true) }()  // backend -> client (outbound)
	go func() { defer pumpWg.Done(); g.pump(c, backend, client, false) }() // client -> backend (inbound)
	pumpWg.Wait()
	c.close()
}

func (c *guardConn) close() {
	c.once.Do(func() {
		if t := c.timer.Load(); t != nil {
			t.Stop()
		}
		c.client.Close()
		c.backend.Close()
		if c.g.cfg.OnDisconnect != nil {
			c.g.cfg.OnDisconnect()
		}
	})
}

func (c *guardConn) noteLogon() {
	if c.loggedOn.CompareAndSwap(false, true) {
		if t := c.timer.Load(); t != nil {
			t.Stop()
		}
	}
}

// pump copies bytes from src to dst, enforcing frame limits (inbound
// only) and idle timeouts. outbound=false means client → backend.
func (g *Guard) pump(c *guardConn, dst, src net.Conn, outbound bool) {
	parser := newFrameParser(g.cfg.MaxFrameBytes)
	buf := make([]byte, 32*1024)
	for {
		_ = src.SetReadDeadline(time.Now().Add(g.cfg.IdleTimeout))
		n, rerr := src.Read(buf)
		if n > 0 {
			frames, oversized, hbViolation := parser.Write(buf[:n], g.cfg.MinHeartBtInt)
			for _, f := range frames {
				if f.msgType == "A" {
					c.noteLogon()
					if !outbound && g.cfg.OnInboundLogon != nil {
						g.cfg.OnInboundLogon()
					}
				}
			}
			if oversized && !outbound {
				g.cfg.Metrics.GuardDroppedFrames.Add(1)
				g.cfg.Logger.Warn("guard: oversized FIX frame, closing connection",
					"session", g.cfg.SessionLabel, "max_bytes", g.cfg.MaxFrameBytes)
				c.close()
				return
			}
			if hbViolation && !outbound {
				g.cfg.Metrics.AuthenticationFails.Add(1)
				g.cfg.Logger.Warn("guard: heartbeat interval below minimum, closing connection",
					"session", g.cfg.SessionLabel, "min_heartbeat", g.cfg.MinHeartBtInt)
				c.close()
				return
			}
			_ = dst.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, werr := dst.Write(buf[:n]); werr != nil {
				c.close()
				return
			}
		}
		if rerr != nil {
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
				if c.loggedOn.Load() {
					g.cfg.Metrics.GuardIdleTimeouts.Add(1)
					g.cfg.Logger.Info("guard: idle timeout, closing connection",
						"session", g.cfg.SessionLabel)
				}
				// If not logged on, the logon timer owns the accounting.
			}
			c.close()
			return
		}
	}
}
