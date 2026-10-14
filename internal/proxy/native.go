package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/graph"
	"github.com/luigi23997/azemu/internal/host"
)

// NativeOptions configures Native.
type NativeOptions struct {
	CADir, CertsDir, GraphDir string
	ProxyPort, IdentityPort   int
	Upstream                  http.RoundTripper // reaches real hosts; default honours the user's own HTTPS_PROXY
}

// Native runs the proxy inside the azemu process (`azemu up --native`), for machines without containers.
// There are no DNS aliases, so apps reach it as their HTTPS_PROXY: every CONNECT to :443 is TLS-terminated with a
// leaf from the local CA; emulated hosts are routed as in the container proxy, every other host is forwarded to the
// real one. Identity and ingress listen on 127.0.0.1. Returns once every listener is bound; errc reports a listener
// that dies. Everything stops when ctx is done.
func Native(ctx context.Context, cfg contract.ProxyConfig, o NativeOptions) (<-chan error, error) {
	c, err := loadCA(o.CADir)
	if err != nil {
		return nil, err
	}
	if err := c.writeCerts(o.CertsDir, systemRoots()); err != nil {
		return nil, err
	}
	id, err := newIdentity(cfg.IdentityHeader)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(c.pem)
	tr := &http.Transport{DialContext: host.DialContext, TLSClientConfig: &tls.Config{RootCAs: pool}, MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second}
	up := o.Upstream
	if up == nil {
		var sysPool *x509.CertPool
		if p, err := x509.SystemCertPool(); err == nil {
			sysPool = p
		} else {
			sysPool = x509.NewCertPool()
			sysPool.AppendCertsFromPEM(systemRoots())
		}
		up = &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         host.DialContext,
			TLSClientConfig:     &tls.Config{RootCAs: sysPool},
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		}
	}

	own := map[string]http.Handler{}
	if cfg.Graph != nil {
		g := newGraphGate(id, cfg.Graph.ReadOnly, filepath.Join(o.GraphDir, graph.TokenFile), up)
		own[contract.LoginHost], own[contract.GraphHost] = g.loginHandler(), g.graphHandler()
	}
	routed := logged(routeHandler(cfg.Routes, tr, own, passthrough(up)))

	servers := map[int]http.Handler{
		o.IdentityPort: id.handler(),
		o.ProxyPort:    forward(c, routed),
	}
	for _, ing := range cfg.Ingress {
		h, err := ingressHandler(ing, cfg.Principal, tr)
		if err != nil {
			return nil, err
		}
		servers[ing.Port] = logged(h)
	}
	errc := make(chan error, len(servers))
	var started []*http.Server
	for port, h := range servers {
		l, err := host.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			for _, s := range started {
				s.Close()
			}
			return nil, fmt.Errorf("proxy: %w", err)
		}
		s := &http.Server{Handler: h}
		started = append(started, s)
		go func() {
			if err := s.Serve(l); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	go func() {
		<-ctx.Done()
		for _, s := range started {
			s.Close()
		}
	}()
	log.Printf("azemu proxy up: %d routes, %d ingress, HTTPS_PROXY http://127.0.0.1:%d", len(cfg.Routes), len(cfg.Ingress), o.ProxyPort)
	return errc, nil
}

// forward is the HTTPS_PROXY endpoint. Inside a CONNECT, a TLS client (first byte 0x16, a ClientHello) is answered
// with a leaf from the local CA and its decrypted requests go to h; anything else is tunnelled to the target as is.
// Absolute-form http:// requests go to h directly.
// ponytail: HTTP/1.1 only inside the tunnel (no h2/gRPC); fine for Azure SDKs and the Functions host.
func forward(c *ca, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			h.ServeHTTP(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			conn.Close()
			return
		}
		first, err := rw.Reader.Peek(1)
		if err != nil {
			conn.Close()
			return
		}
		pc := &peeked{Conn: conn, r: rw.Reader}
		if first[0] != 0x16 {
			tunnel(pc, r.Host)
			return
		}
		tc := tls.Server(pc, &tls.Config{GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName == "" { // client dialed an IP: name the leaf after the CONNECT target
				hh := *hello
				hh.ServerName = host
				hello = &hh
			}
			return c.getCertificate(hello)
		}})
		l := &oneConn{c: tc, done: make(chan struct{})}
		var once sync.Once
		(&http.Server{Handler: h, ErrorLog: log.New(io.Discard, "", 0), ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateClosed || s == http.StateHijacked {
				once.Do(func() { close(l.done) })
			}
		}}).Serve(l)
	})
}

// peeked is a hijacked conn whose first bytes sit in r.
type peeked struct {
	net.Conn
	r *bufio.Reader
}

func (p *peeked) Read(b []byte) (int, error) { return p.r.Read(b) }

func tunnel(client net.Conn, addr string) {
	defer client.Close()
	up, err := host.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, client); done <- struct{}{} }()
	go func() { io.Copy(client, up); done <- struct{}{} }()
	<-done
}

// passthrough forwards a request for a non-emulated host to the real host, unchanged.
func passthrough(up http.RoundTripper) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			if pr.In.TLS != nil {
				pr.Out.URL.Scheme = "https"
			}
			pr.Out.URL.Host = pr.In.Host
			pr.Out.Host = pr.In.Host
		},
		Transport:     up,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "azemu proxy: "+r.Host+": "+err.Error(), http.StatusBadGateway)
		},
	}
}

// oneConn is a net.Listener that yields c once, then reports closed when done is closed (the connection ended).
// c stays a bare *tls.Conn so http.Server sets Request.TLS.
type oneConn struct {
	c      net.Conn
	done   chan struct{}
	served bool // Serve calls Accept from one goroutine
}

func (l *oneConn) Accept() (net.Conn, error) {
	if !l.served {
		l.served = true
		return l.c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *oneConn) Close() error   { return nil }
func (l *oneConn) Addr() net.Addr { return l.c.LocalAddr() }

// systemRootsFile is the first readable PEM bundle of the usual Linux/macOS locations.
func systemRootsFile() []byte {
	for _, p := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/cert.pem"} {
		if b, err := os.ReadFile(p); err == nil {
			return b
		}
	}
	return nil
}
