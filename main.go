package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultListenAddr    = ":8080"
	defaultDialTimeout   = 3 * time.Second
	defaultShutdownGrace = 10 * time.Second
	copyBufferSize       = 32 * 1024
)

var copyBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, copyBufferSize)
		return &buf
	},
}

type backendPool struct {
	backends []string
	next     atomic.Uint64
}

func newBackendPool(raw string) (*backendPool, error) {
	parts := strings.Split(raw, ",")
	backends := make([]string, 0, len(parts))
	for _, part := range parts {
		addr := strings.TrimSpace(part)
		if addr == "" {
			continue
		}
		backends = append(backends, addr)
	}
	if len(backends) == 0 {
		return nil, errors.New("at least one backend is required")
	}
	return &backendPool{backends: backends}, nil
}

func (p *backendPool) size() int {
	return len(p.backends)
}

func (p *backendPool) nextStartIndex() int {
	return int(p.next.Add(1)-1) % len(p.backends)
}

func (p *backendPool) backendAt(idx int) string {
	return p.backends[idx%len(p.backends)]
}

type proxy struct {
	listenAddr       string
	dialTimeout      time.Duration
	shutdownGrace    time.Duration
	tcpKeepAlive     time.Duration
	backendPool      *backendPool
	activeConnMu     sync.Mutex
	activeConns      map[net.Conn]struct{}
	listener         net.Listener
	wg               sync.WaitGroup
	shutdownOnce     sync.Once
	logger           *log.Logger
	disableKeepAlive bool
}

func newProxy(
	listenAddr string,
	dialTimeout time.Duration,
	shutdownGrace time.Duration,
	tcpKeepAlive time.Duration,
	disableKeepAlive bool,
	pool *backendPool,
	logger *log.Logger,
) *proxy {
	return &proxy{
		listenAddr:       listenAddr,
		dialTimeout:      dialTimeout,
		shutdownGrace:    shutdownGrace,
		tcpKeepAlive:     tcpKeepAlive,
		backendPool:      pool,
		activeConns:      make(map[net.Conn]struct{}),
		logger:           logger,
		disableKeepAlive: disableKeepAlive,
	}
}

func (p *proxy) run(ctx context.Context) error {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", p.listenAddr)
	if err != nil {
		return err
	}
	p.listener = ln
	p.logger.Printf("proxy listening on %s with %d backends", p.listenAddr, p.backendPool.size())

	go func() {
		<-ctx.Done()
		p.initiateShutdown()
	}()

	for {
		clientConn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Temporary() {
				p.logger.Printf("temporary accept error: %v", err)
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}

		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handleClient(ctx, clientConn)
		}()
	}
}

func (p *proxy) handleClient(ctx context.Context, clientConn net.Conn) {
	clientRemote := clientConn.RemoteAddr().String()
	if err := tuneTCPConn(clientConn, p.tcpKeepAlive, p.disableKeepAlive); err != nil {
		p.logger.Printf("client tune failed for %s: %v", clientRemote, err)
	}

	backendConn, backendAddr, err := p.dialBackend(ctx)
	if err != nil {
		p.logger.Printf("backend dial failed for %s: %v", clientRemote, err)
		_ = clientConn.Close()
		return
	}
	defer backendConn.Close()
	defer clientConn.Close()

	if err := tuneTCPConn(backendConn, p.tcpKeepAlive, p.disableKeepAlive); err != nil {
		p.logger.Printf("backend tune failed for %s -> %s: %v", clientRemote, backendAddr, err)
	}

	p.trackConn(clientConn)
	p.trackConn(backendConn)
	defer p.untrackConn(clientConn)
	defer p.untrackConn(backendConn)

	p.logger.Printf("accepted %s -> %s", clientRemote, backendAddr)

	errCh := make(chan error, 2)

	go func() {
		errCh <- proxyCopy(backendConn, clientConn)
	}()
	go func() {
		errCh <- proxyCopy(clientConn, backendConn)
	}()

	firstErr := <-errCh
	p.halfClose(clientConn, backendConn)
	secondErr := <-errCh

	if err := firstRelevantCopyErr(firstErr, secondErr); err != nil {
		p.logger.Printf("connection %s -> %s closed with error: %v", clientRemote, backendAddr, err)
	} else {
		p.logger.Printf("connection %s -> %s closed cleanly", clientRemote, backendAddr)
	}
}

func (p *proxy) dialBackend(parent context.Context) (net.Conn, string, error) {
	dialer := net.Dialer{Timeout: p.dialTimeout}
	var lastErr error
	start := p.backendPool.nextStartIndex()

	for attempt := 0; attempt < p.backendPool.size(); attempt++ {
		backendAddr := p.backendPool.backendAt(start + attempt)
		ctx, cancel := context.WithTimeout(parent, p.dialTimeout)
		conn, err := dialer.DialContext(ctx, "tcp", backendAddr)
		cancel()
		if err == nil {
			return conn, backendAddr, nil
		}
		lastErr = err
		p.logger.Printf("dial attempt %d to %s failed: %v", attempt+1, backendAddr, err)
	}

	return nil, "", lastErr
}

func (p *proxy) initiateShutdown() {
	p.shutdownOnce.Do(func() {
		if p.listener != nil {
			_ = p.listener.Close()
		}

		done := make(chan struct{})
		go func() {
			p.wg.Wait()
			close(done)
		}()

		timer := time.NewTimer(p.shutdownGrace)
		defer timer.Stop()

		select {
		case <-done:
			return
		case <-timer.C:
		}

		p.activeConnMu.Lock()
		for conn := range p.activeConns {
			_ = conn.Close()
		}
		p.activeConnMu.Unlock()

		<-done
	})
}

func (p *proxy) trackConn(conn net.Conn) {
	p.activeConnMu.Lock()
	p.activeConns[conn] = struct{}{}
	p.activeConnMu.Unlock()
}

func (p *proxy) untrackConn(conn net.Conn) {
	p.activeConnMu.Lock()
	delete(p.activeConns, conn)
	p.activeConnMu.Unlock()
}

func (p *proxy) halfClose(clientConn net.Conn, backendConn net.Conn) {
	if tcp, ok := clientConn.(*net.TCPConn); ok {
		_ = tcp.CloseRead()
		_ = tcp.CloseWrite()
	}
	if tcp, ok := backendConn.(*net.TCPConn); ok {
		_ = tcp.CloseRead()
		_ = tcp.CloseWrite()
	}
}

func proxyCopy(dst io.Writer, src io.Reader) error {
	bufp := copyBufferPool.Get().(*[]byte)
	defer copyBufferPool.Put(bufp)

	_, err := io.CopyBuffer(dst, src, *bufp)
	if err == nil {
		return nil
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		return nil
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil && strings.Contains(opErr.Err.Error(), "use of closed network connection") {
		return nil
	}
	return err
}

func firstRelevantCopyErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func tuneTCPConn(conn net.Conn, keepAlive time.Duration, disableKeepAlive bool) error {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return nil
	}
	if err := tcp.SetNoDelay(true); err != nil {
		return err
	}
	if disableKeepAlive {
		return tcp.SetKeepAlive(false)
	}
	if err := tcp.SetKeepAlive(true); err != nil {
		return err
	}
	return tcp.SetKeepAlivePeriod(keepAlive)
}

func main() {
	var (
		listenAddr       = flag.String("listen", defaultListenAddr, "listen address")
		backends         = flag.String("backends", "", "comma-separated backend addresses")
		dialTimeout      = flag.Duration("dial-timeout", defaultDialTimeout, "backend dial timeout")
		shutdownGrace    = flag.Duration("shutdown-grace", defaultShutdownGrace, "grace period before force-closing active connections")
		tcpKeepAlive     = flag.Duration("tcp-keepalive", 30*time.Second, "TCP keepalive period")
		disableKeepAlive = flag.Bool("disable-keepalive", false, "disable TCP keepalive")
	)
	flag.Parse()

	logger := log.New(os.Stdout, "unikernel-proxy ", log.Ldate|log.Ltime|log.Lmicroseconds|log.LUTC)

	pool, err := newBackendPool(*backends)
	if err != nil {
		logger.Fatalf("invalid backends: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	p := newProxy(
		*listenAddr,
		*dialTimeout,
		*shutdownGrace,
		*tcpKeepAlive,
		*disableKeepAlive,
		pool,
		logger,
	)

	if err := p.run(ctx); err != nil {
		logger.Fatalf("proxy failed: %v", err)
	}
}
