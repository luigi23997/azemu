package up

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/host"
	"github.com/luigi23997/azemu/internal/proxy"
)

// Trust adds the azemu root CA to the current user's trust store (`azemu trust`). Native mode needs it on Windows
// and macOS: the Functions host is .NET, which trusts only the OS store there, not SSL_CERT_FILE.
// Anything holding ~/.azemu/ca/ca-key.pem can then mint certificates this user's programs trust.
func Trust() error {
	path, err := caFile()
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch host.GOOS() {
	case "windows": // no admin needed; Windows asks for confirmation
		cmd = host.Command("certutil", "-user", "-addstore", "Root", path)
	case "darwin": // macOS asks for your password
		home, _ := os.UserHomeDir()
		cmd = host.Command("security", "add-trusted-cert", "-r", "trustRoot", "-k", filepath.Join(home, "Library", "Keychains", "login.keychain-db"), path)
	default:
		fmt.Println("nothing to do on Linux: native apps find the CA through SSL_CERT_FILE")
		return nil
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := host.Run(cmd); err != nil {
		return fmt.Errorf("adding %s to the trust store: %w", path, err)
	}
	fmt.Println("trusted:", path)
	return nil
}

// Trusted reports whether the OS trust store already has the azemu CA, and if not, how to fix it.
func Trusted() (bool, string) {
	path, err := caFile()
	if err != nil {
		return false, err.Error()
	}
	switch host.GOOS() {
	case "windows":
		b, _ := os.ReadFile(path)
		blk, _ := pem.Decode(b)
		if blk == nil {
			return false, "invalid " + path
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return false, err.Error()
		}
		if host.Run(host.Command("certutil", "-user", "-verifystore", "Root", c.SerialNumber.Text(16))) == nil {
			return true, ""
		}
	case "darwin":
		if host.Run(host.Command("security", "verify-cert", "-c", path)) == nil {
			return true, ""
		}
	default:
		return true, ""
	}
	return false, "run `azemu trust`"
}

func caFile() (string, error) {
	_, err := proxy.EnsureCA(contract.HostCADir())
	return filepath.Join(contract.HostCADir(), "ca.pem"), err
}
