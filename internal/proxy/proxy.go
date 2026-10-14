// Package proxy is the in-network TLS router, fake managed identity and ingress.
package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/graph"
)

const systemBundle = "/etc/ssl/certs/ca-certificates.crt"

// Run starts the proxy using the config at path. Blocks.
func Run(configPath string) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var cfg contract.ProxyConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("%s: %w", configPath, err)
	}
	c, err := loadCA(contract.ProxyCADir)
	if err != nil {
		return err
	}
	for h := range cfg.Routes {
		c.noSNI = append(c.noSNI, h)
	}
	sort.Strings(c.noSNI)
	sys, err := os.ReadFile(systemBundle)
	if err != nil {
		return err
	}
	if err := c.writeCerts(contract.CertsDir, sys); err != nil {
		return err
	}
	id, err := newIdentity(cfg.IdentityHeader)
	if err != nil {
		return err
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(c.pem)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second}

	own := map[string]http.Handler{} // hosts the proxy answers itself instead of forwarding to a container
	if cfg.Graph != nil {
		g := newGraphGate(id, cfg.Graph.ReadOnly, filepath.Join(contract.GraphDir, graph.TokenFile), realTransport())
		own[contract.LoginHost], own[contract.GraphHost] = g.loginHandler(), g.graphHandler()
	}

	errc := make(chan error, 2+len(cfg.Ingress))
	serve := func(s *http.Server, tlsMode bool) {
		if tlsMode {
			errc <- s.ListenAndServeTLS("", "")
		} else {
			errc <- s.ListenAndServe()
		}
	}
	go serve(&http.Server{Addr: ":" + strconv.Itoa(contract.IdentityPort), Handler: id.handler()}, false)
	go serve(&http.Server{
		Addr:      ":" + strconv.Itoa(contract.ProxyTLSPort),
		Handler:   logged(routeHandler(cfg.Routes, tr, own, nil)),
		TLSConfig: &tls.Config{GetCertificate: c.getCertificate},
	}, true)
	for _, ing := range cfg.Ingress {
		h, err := ingressHandler(ing, cfg.Principal, tr)
		if err != nil {
			return err
		}
		go serve(&http.Server{Addr: ":" + strconv.Itoa(ing.Port), Handler: logged(h)}, false)
	}
	log.Printf("azemu proxy up: %d routes, %d ingress", len(cfg.Routes), len(cfg.Ingress))
	return <-errc
}

// newProxy forwards to target keeping the original Host header (Azurite sees production-style hosts).
func newProxy(target *url.URL, host string, tr http.RoundTripper, edit func(*http.Request)) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = host
			if host == "" {
				pr.Out.Host = pr.In.Host
			}
			if edit != nil {
				edit(pr.Out)
			}
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "azemu proxy: upstream "+target.Host+": "+err.Error(), http.StatusBadGateway)
		},
	}
}

// routeHandler sends each request to the upstream of its host; unknown hosts go to fallback (nil: 502).
func routeHandler(routes map[string]string, tr http.RoundTripper, own map[string]http.Handler, fallback http.Handler) http.Handler {
	proxies := map[string]http.Handler{}
	for host, up := range routes {
		if u, err := url.Parse(up); err == nil {
			proxies[strings.ToLower(host)] = newProxy(u, "", tr, nil)
		}
	}
	for host, h := range own {
		proxies[host] = h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		p := proxies[strings.ToLower(host)]
		if p == nil {
			p = fallback
		}
		if p == nil {
			http.Error(w, "azemu proxy: no route for host "+host, http.StatusBadGateway)
			return
		}
		p.ServeHTTP(w, r)
	})
}

func ingressHandler(ing contract.Ingress, pr contract.Principal, tr http.RoundTripper) (http.Handler, error) {
	u, err := url.Parse(ing.Upstream)
	if err != nil {
		return nil, err
	}
	return newProxy(u, ing.Host, tr, func(r *http.Request) {
		for k := range r.Header {
			if strings.HasPrefix(strings.ToUpper(k), "X-MS-") {
				delete(r.Header, k)
			}
		}
		if !ing.EasyAuth {
			return
		}
		principal, _ := json.Marshal(map[string]any{
			"auth_typ": "aad", "name_typ": "preferred_username", "role_typ": "roles",
			"claims": []map[string]string{
				{"typ": "preferred_username", "val": pr.Name},
				{"typ": "http://schemas.microsoft.com/identity/claims/objectidentifier", "val": pr.ID},
			},
		})
		r.Header.Set("X-MS-CLIENT-PRINCIPAL-NAME", pr.Name)
		r.Header.Set("X-MS-CLIENT-PRINCIPAL-ID", pr.ID)
		r.Header.Set("X-MS-CLIENT-PRINCIPAL-IDP", "aad")
		r.Header.Set("X-MS-CLIENT-PRINCIPAL", base64.StdEncoding.EncodeToString(principal))
	}), nil
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(c int)           { s.status = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// logged writes one greppable line per request: METHOD host path status ms.
func logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: 200}
		t := time.Now()
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %s %d %dms", r.Method, r.Host, r.URL.Path, sw.status, time.Since(t).Milliseconds())
	})
}
