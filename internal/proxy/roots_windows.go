package proxy

import (
	"encoding/pem"
	"unsafe"

	"golang.org/x/sys/windows"
)

// systemRoots exports the current user's view of the Windows ROOT store (machine roots, corporate TLS-inspection
// roots, the azemu CA once trusted) as PEM: Windows has no bundle file for SSL_CERT_FILE / REQUESTS_CA_BUNDLE.
// ponytail: only roots already in the store; Windows fetches some public roots on demand, so a client that bypasses
// the proxy may miss one. Traffic through the proxy is unaffected (it verifies upstreams with the OS verifier).
func systemRoots() []byte {
	name, _ := windows.UTF16PtrFromString("ROOT")
	store, err := windows.CertOpenSystemStore(0, name)
	if err != nil {
		return systemRootsFile()
	}
	defer windows.CertCloseStore(store, 0)
	var out []byte
	var c *windows.CertContext
	for {
		if c, err = windows.CertEnumCertificatesInStore(store, c); err != nil || c == nil {
			return out
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: unsafe.Slice(c.EncodedCert, c.Length)})...)
	}
}
