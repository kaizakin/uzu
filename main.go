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

// backendPool manages a list of backend server addresses to proxy traffic to.
// It keeps track of available backends and uses an atomic counter for round-robin selection.
type backendPool struct {
	backends []string
	next     atomic.Uint64
}

// newBackendPool parses a comma-separated string of backend addresses and initializes a backendPool.
// We use this to construct the pool of available servers that the proxy will forward connections to.
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

// size returns the number of backend servers in the pool.
// It is used to iterate over all possible backends when attempting to dial them.
func (p *backendPool) size() int {
	return len(p.backends)
}

// nextStartIndex atomically increments the internal counter and returns the next starting index.
// This is used to ensure a round-robin load balancing strategy across different connection attempts.
func (p *backendPool) nextStartIndex() int {
	return int(p.next.Add(1)-1) % len(p.backends)
}

// backendAt safely retrieves the backend address at a specific index using modulo arithmetic.
// We use this to wrap around the slice when trying multiple backends sequentially.
func (p *backendPool) backendAt(idx int) string {
	return p.backends[idx%len(p.backends)]
}

// proxy represents the main proxy server instance.
// It holds configuration, the backend pool, active connections state, and manages the listener lifecycle.
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

// newProxy initializes and returns a new proxy server instance.
// It sets up the configuration options and initializes the active connection tracking map.
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

// run starts the proxy server, listening for incoming TCP connections.
// It handles context cancellation for graceful shutdowns and accepts connections in a continuous loop.
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

// handleClient is responsible for processing an individual incoming client connection.
// It tunes the TCP connection, dials a backend server, and sets up bi-directional traffic copying.
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

// dialBackend attempts to establish a connection with one of the available backend servers.
// It iterates through the backend pool starting from a round-robin index, providing fault tolerance if a backend is down.
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

// initiateShutdown performs a graceful shutdown of the proxy server.
// It closes the main listener, waits for active connections to finish within a grace period, and forcefully closes any remaining ones.
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

// trackConn adds a network connection to the proxy's active connections map.
// This allows the proxy to keep track of active connections so they can be closed during shutdown.
func (p *proxy) trackConn(conn net.Conn) {
	p.activeConnMu.Lock()
	p.activeConns[conn] = struct{}{}
	p.activeConnMu.Unlock()
}

// untrackConn removes a network connection from the active connections map.
// This is called when a connection has finished processing and no longer needs to be managed for shutdown.
func (p *proxy) untrackConn(conn net.Conn) {
	p.activeConnMu.Lock()
	delete(p.activeConns, conn)
	p.activeConnMu.Unlock()
}

// halfClose explicitly closes the read and write sides of the client and backend TCP connections.
// This helps to cleanly terminate the connections and unblock any pending I/O operations.
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

// proxyCopy copies data from the source io.Reader to the destination io.Writer using a shared buffer pool.
// It filters out expected network errors (like EOF or closed connections) to prevent unnecessary error logging.
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

// firstRelevantCopyErr iterates through a list of errors and returns the first non-nil error.
// It is used to capture and report the primary reason a connection failed during bi-directional copying.
func firstRelevantCopyErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// tuneTCPConn applies specific TCP socket options like NoDelay (Nagle's algorithm) and KeepAlive settings.
// We use this to optimize the network connections for latency and to detect dead connections over time.
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

// main is the entry point of the unikernel-proxy application.
// It parses command-line flags, initializes the backend pool and proxy instance, and handles OS signals for graceful termination.
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
