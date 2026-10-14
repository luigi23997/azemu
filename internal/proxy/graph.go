package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/graph"
)

// graphGate poses as an app registration with Graph application roles, backed by the signed-in user's delegated token:
//  1. login.microsoftonline.com: client-credentials requests for Graph get a fake token (any client id/secret works);
//     every other request goes to the real Entra, so apps using a service principal for other resources keep working.
//  2. graph.microsoft.com: the fake token is replaced by the user's real token; any other token passes through untouched.
type graphGate struct {
	id       *identity
	src      *graph.Source
	readOnly bool
	entra    http.Handler // real login.microsoftonline.com
	graph    http.Handler // real graph.microsoft.com
}

const graphMarker = "azemu" // claim set to "graph" in the fake token

var clientCredsPath = regexp.MustCompile(`^/[^/]+/oauth2(/v2\.0)?/token$`)

// newGraphGate reaches the real Entra and Graph through tr.
func newGraphGate(id *identity, readOnly bool, tokenFile string, tr http.RoundTripper) *graphGate {
	mk := func(host string) http.Handler { return newProxy(&url.URL{Scheme: "https", Host: host}, "", tr, nil) }
	return &graphGate{id: id, readOnly: readOnly, src: &graph.Source{Path: tokenFile},
		entra: mk(contract.LoginHost), graph: mk(contract.GraphHost)}
}

func isGraphScope(s string) bool {
	return strings.Contains(s, contract.GraphHost) || strings.Contains(s, "00000003-0000-0000-c000-000000000000")
}

func (g *graphGate) loginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && clientCredsPath.MatchString(r.URL.Path) {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body = io.NopCloser(bytes.NewReader(body))
			form, _ := url.ParseQuery(string(body))
			if form.Get("grant_type") == "client_credentials" && isGraphScope(form.Get("scope")+" "+form.Get("resource")) {
				tok, err := g.id.mint("https://"+contract.GraphHost, map[string]any{graphMarker: "graph"})
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer", "expires_in": 3599, "ext_expires_in": 3599, "access_token": tok})
				return
			}
		}
		g.entra.ServeHTTP(w, r)
	})
}

func (g *graphGate) graphHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isFakeGraphToken(r.Header.Get("Authorization")) {
			g.graph.ServeHTTP(w, r)
			return
		}
		if g.readOnly && !readOnlyRequest(r) {
			graphError(w, http.StatusForbidden, "azemu_read_only", "azemu: Graph is read-only for local runs (set graph.writable: true in azemu.yaml, and request write scopes at `azemu graph login`)")
			return
		}
		tok, err := g.src.Access(r.Context())
		if err != nil {
			graphError(w, http.StatusUnauthorized, "azemu_no_graph_login", "azemu: "+err.Error())
			return
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		g.graph.ServeHTTP(w, r)
	})
}

func graphError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// isFakeGraphToken reports whether the bearer token is one minted by loginHandler (marker only: nothing is trusted because of it).
func isFakeGraphToken(auth string) bool {
	parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
	if len(parts) != 3 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var c map[string]any
	return json.Unmarshal(b, &c) == nil && c[graphMarker] == "graph"
}

// readOnlyRequest allows reads, search/query (a POST that only reads) and $batch made of GETs only.
// It restores r.Body so the request can still be forwarded.
func readOnlyRequest(r *http.Request) bool {
	switch r.Method {
	case "GET", "HEAD", "OPTIONS":
		return true
	case "POST":
	default:
		return false
	}
	if strings.HasSuffix(r.URL.Path, "/search/query") {
		return true
	}
	if !strings.HasSuffix(r.URL.Path, "/$batch") {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	r.Body = io.NopCloser(bytes.NewReader(body))
	var b struct{ Requests []struct{ Method string } }
	if json.Unmarshal(body, &b) != nil {
		return false
	}
	for _, q := range b.Requests {
		if !strings.EqualFold(q.Method, "GET") {
			return false
		}
	}
	return true
}

// realTransport dials the real Entra/Graph. Docker's embedded DNS answers these names with the proxy itself (they are its
// network aliases), so lookups go straight to the host's own upstream resolvers. System roots only: the local CA must not vouch.
func realTransport() *http.Transport {
	servers := upstreamDNS(readFile("/etc/resolv.conf"))
	var n atomic.Uint32
	d := &net.Dialer{Timeout: 10 * time.Second, Resolver: &net.Resolver{
		PreferGo: true,
		// ponytail: rotates servers per dial, so a dead one costs a retry; no health tracking.
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			s := servers[int(n.Add(1)-1)%len(servers)]
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, net.JoinHostPort(s, "53"))
		},
	}}
	return &http.Transport{DialContext: d.DialContext, ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second}
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }

// upstreamDNS lists the non-loopback resolvers Docker recorded in resolv.conf ("# ExtServers: [host(1.2.3.4) 8.8.8.8]"),
// then public fallbacks.
func upstreamDNS(resolvConf string) []string {
	var out []string
	for _, line := range strings.Split(resolvConf, "\n") {
		_, rest, ok := strings.Cut(line, "ExtServers:")
		if !ok {
			continue
		}
		for _, f := range strings.Fields(strings.Trim(strings.TrimSpace(rest), "[]")) {
			f = strings.TrimSuffix(strings.TrimPrefix(f, "host("), ")")
			if ip := net.ParseIP(f); ip != nil && !ip.IsLoopback() {
				out = append(out, f)
			}
		}
	}
	return append(out, "1.1.1.1", "8.8.8.8")
}
