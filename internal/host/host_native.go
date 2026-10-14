//go:build !js

// Package host bridges OS primitives (process execution, TCP sockets, OS identity)
// that use the standard library on native targets and the Node.js host bridge under GOOS=js.
package host

import (
	"context"
	"net"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

// GOOS is the operating system where azemu is running (the Node host OS when built for GOOS=js).
func GOOS() string { return runtime.GOOS }

// Listen binds a TCP listener on the host.
func Listen(network, address string) (net.Listener, error) {
	return net.Listen(network, address)
}

// DialTimeout connects to address on network with a timeout.
func DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout(network, address, timeout)
}

// DialContext connects to address on network using ctx.
func DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

// Command returns a Cmd to execute name with args.
func Command(name string, arg ...string) *exec.Cmd {
	return exec.Command(name, arg...)
}

// CommandContext returns a Cmd to execute name with args bound to ctx.
func CommandContext(ctx context.Context, name string, arg ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, arg...)
}

// NewProcessGroup configures cmd so it runs in its own process group (or shell command line).
func NewProcessGroup(shell string, argv []string) *exec.Cmd {
	return newProcessGroup(shell, argv)
}

// Terminate stops the process tree rooted at cmd.
func Terminate(cmd *exec.Cmd, force bool) {
	terminateProc(cmd, force)
}

// Start begins cmd asynchronously.
func Start(cmd *exec.Cmd) error { return cmd.Start() }

// Wait waits for cmd to exit.
func Wait(cmd *exec.Cmd) error { return cmd.Wait() }

// Run runs cmd to completion.
func Run(cmd *exec.Cmd) error { return cmd.Run() }

// Output runs cmd and returns its standard output.
func Output(cmd *exec.Cmd) ([]byte, error) { return cmd.Output() }

// CombinedOutput runs cmd and returns its combined standard output and standard error.
func CombinedOutput(cmd *exec.Cmd) ([]byte, error) { return cmd.CombinedOutput() }

// ExitState returns a human-readable exit status for a finished cmd.
func ExitState(cmd *exec.Cmd) string {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.String()
	}
	return "exited"
}

var _ = syscall.Stdout
