//go:build js

package proxy

import (
	"strings"
	"syscall/js"
)

// systemRoots exports the OS and bundled root CAs via Node's tls module when running under GOOS=js.
func systemRoots() []byte {
	if b := systemRootsFile(); len(b) > 0 {
		return b
	}
	req := js.Global().Get("require")
	if req.Type() != js.TypeFunction {
		return nil
	}
	tlsMod := req.Invoke("tls")
	var certs js.Value
	if getCA := tlsMod.Get("getCACertificates"); getCA.Type() == js.TypeFunction {
		certs = tlsMod.Call("getCACertificates", "system")
	}
	if certs.IsUndefined() || certs.Length() == 0 {
		certs = tlsMod.Get("rootCertificates")
	}
	if certs.IsUndefined() {
		return nil
	}
	var sb strings.Builder
	for i := 0; i < certs.Length(); i++ {
		sb.WriteString(certs.Index(i).String())
		sb.WriteByte('\n')
	}
	return []byte(sb.String())
}
