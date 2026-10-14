//go:build !windows

package proxy

// systemRoots is the OS trust bundle, written into bundle.pem next to the local CA.
func systemRoots() []byte { return systemRootsFile() }
