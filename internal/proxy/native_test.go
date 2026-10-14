package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/host"
)

func freeTestPort(t *testing.T) int {
	l, err := host.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestNativeForwardProxy tests an app with HTTPS_PROXY set: emulated hosts reach their local upstream, other hosts reach the real one.
func TestNativeForwardProxy(t *testing.T) {
	al, err := host.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	app := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "app %s %s", r.Host, r.URL.Path)
	}))
	app.Listener = al
	app.Start()
	defer app.Close()
	rl, err := host.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	real := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "real %s", r.URL.Path)
	}))
	real.Listener = rl
	real.StartTLS()
	defer real.Close()
	realPool := x509.NewCertPool()
	realPool.AddCert(real.Certificate())
	realTr := &http.Transport{DialContext: host.DialContext, TLSClientConfig: &tls.Config{RootCAs: realPool}}

	dir := t.TempDir()
	pp, ip := freeTestPort(t), freeTestPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := contract.ProxyConfig{IdentityHeader: "h", Routes: map[string]string{"myapp.azurewebsites.net": app.URL}}
	if _, err := Native(ctx, cfg, NativeOptions{
		CADir: filepath.Join(dir, "ca"), CertsDir: filepath.Join(dir, "certs"), ProxyPort: pp, IdentityPort: ip,
		Upstream: realTr,
	}); err != nil {
		t.Fatal(err)
	}

	// the app trusts only bundle.pem, as REQUESTS_CA_BUNDLE makes it
	bundle, err := os.ReadFile(filepath.Join(dir, "certs", "bundle.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(bundle)
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", pp))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DialContext: host.DialContext, TLSClientConfig: &tls.Config{RootCAs: pool}}}
	get := func(u string) string {
		t.Helper()
		resp, err := client.Get(u)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	if got := get("https://myapp.azurewebsites.net/api/x"); got != "app myapp.azurewebsites.net /api/x" {
		t.Errorf("routed over CONNECT: %q", got)
	}
	if got := get("http://myapp.azurewebsites.net/plain"); got != "app myapp.azurewebsites.net /plain" {
		t.Errorf("routed plain http: %q", got)
	}
	// a non-emulated host goes to the real one (here the TLS test server, reached through Upstream)
	if got := get("https://" + real.Listener.Addr().String() + "/y"); got != "real /y" {
		t.Errorf("passthrough: %q", got)
	}

	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s?resource=https://storage.azure.com", ip, contract.IdentityPath), nil)
	req.Header.Set("X-IDENTITY-HEADER", "h")
	idClient := &http.Client{Transport: &http.Transport{DialContext: host.DialContext}}
	resp, err := idClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("identity: %v %v", resp, err)
	}
	resp.Body.Close()
}
