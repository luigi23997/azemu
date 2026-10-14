package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	// noSNI are the hosts on the cert served to clients that send no SNI (e.g. nginx proxy_pass to https://host);
	// they verify it against the host they dialed, so it must cover every route host.
	noSNI []string
	mu    sync.Mutex // ponytail: one lock over minting; per-host singleflight if cold-start latency matters
	// leaves caches *tls.Certificate by SNI.
	leaves sync.Map
}

// EnsureCA loads ca.pem/ca-key.pem from dir or creates them (ECDSA P-256, 10y).
// Usable by the host CLI too. ponytail: no cross-process lock; call it once before starting proxies.
func EnsureCA(dir string) ([]byte, error) {
	c, err := loadCA(dir)
	if err != nil {
		return nil, err
	}
	return c.pem, nil
}

func loadCA(dir string) (*ca, error) {
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	certPEM, err1 := os.ReadFile(certPath)
	keyPEM, err2 := os.ReadFile(keyPath)
	if err1 == nil && err2 == nil {
		cb, _ := pem.Decode(certPEM)
		kb, _ := pem.Decode(keyPEM)
		if cb == nil || kb == nil {
			return nil, fmt.Errorf("%s: invalid PEM in CA files", dir)
		}
		cert, err := x509.ParseCertificate(cb.Bytes)
		if err != nil {
			return nil, err
		}
		key, err := x509.ParseECPrivateKey(kb.Bytes)
		if err != nil {
			return nil, err
		}
		return &ca{cert: cert, key: key, pem: certPEM}, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "azemu local CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	return &ca{cert: cert, key: key, pem: certPEM}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

// mint signs a fresh leaf (ECDSA P-256, 1y) for the given hosts (DNS names or IPs).
func (c *ca) mint(hosts ...string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// getCertificate is tls.Config.GetCertificate: one cached leaf per SNI.
func (c *ca) getCertificate(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := h.ServerName
	hosts := []string{host}
	if host == "" {
		hosts = append([]string{"localhost"}, c.noSNI...)
	}
	if v, ok := c.leaves.Load(host); ok {
		return v.(*tls.Certificate), nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.leaves.Load(host); ok {
		return v.(*tls.Certificate), nil
	}
	certPEM, keyPEM, err := c.mint(hosts...)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	c.leaves.Store(host, &pair)
	return &pair, nil
}

// writeCerts publishes the shared-volume files: ca.pem, bundle.pem (sys roots + CA), azurite(.pem|-key.pem).
// All world-readable: containers run as various users.
func (c *ca) writeCerts(dir string, sys []byte) error {
	hosts := []string{"azurite", "localhost", "127.0.0.1"}
	for _, svc := range []string{"blob", "queue", "table"} {
		hosts = append(hosts, "*."+svc+".core.windows.net")
	}
	certPEM, keyPEM, err := c.mint(hosts...)
	if err != nil {
		return err
	}
	// azurite.pem last: it is the signal that the volume is ready.
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"ca.pem", c.pem},
		{"bundle.pem", append(append(sys, '\n'), c.pem...)},
		{"azurite-key.pem", keyPEM},
		{"azurite.pem", certPEM},
	} {
		if err := writeReadable(filepath.Join(dir, f.name), f.data); err != nil {
			return err
		}
	}
	return nil
}

func writeReadable(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil { // umask may have narrowed it
		return err
	}
	return os.Rename(tmp, path)
}
