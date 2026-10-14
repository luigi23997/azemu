//go:build js

package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"syscall/js"
	"time"
)

var (
	uint8Array = js.Global().Get("Uint8Array")
	jsHost     = js.Global().Get("__azemuHost")
)

func hostBridge() js.Value {
	if jsHost.IsUndefined() || jsHost.IsNull() {
		jsHost = js.Global().Get("__azemuHost")
	}
	return jsHost
}

func hasBridge() bool {
	h := hostBridge()
	return !h.IsUndefined() && !h.IsNull()
}

// GOOS returns the underlying host OS reported by the Node.js runner.
func GOOS() string {
	if hasBridge() {
		if v := hostBridge().Get("goos"); v.Type() == js.TypeString {
			return v.String()
		}
	}
	if p := js.Global().Get("process"); p.Type() == js.TypeObject {
		switch p.Get("platform").String() {
		case "win32":
			return "windows"
		case "darwin":
			return "darwin"
		case "linux":
			return "linux"
		}
	}
	return "js"
}

// ---- Process Execution ----

type procMeta struct {
	ctx    context.Context
	shell  string
	handle js.Value
	done   chan error
	state  string
}

var (
	procMu  sync.Mutex
	procMap = map[*exec.Cmd]*procMeta{}
)

func getMeta(cmd *exec.Cmd) *procMeta {
	procMu.Lock()
	defer procMu.Unlock()
	m := procMap[cmd]
	if m == nil {
		m = &procMeta{}
		procMap[cmd] = m
	}
	return m
}

// Command returns a Cmd to execute name with args.
func Command(name string, arg ...string) *exec.Cmd {
	cmd := &exec.Cmd{
		Path: name,
		Args: append([]string{name}, arg...),
	}
	return cmd
}

// CommandContext returns a Cmd to execute name with args bound to ctx.
func CommandContext(ctx context.Context, name string, arg ...string) *exec.Cmd {
	cmd := Command(name, arg...)
	getMeta(cmd).ctx = ctx
	return cmd
}

// NewProcessGroup configures cmd so it runs in its own process group (or shell command line).
func NewProcessGroup(shell string, argv []string) *exec.Cmd {
	var cmd *exec.Cmd
	if len(argv) > 0 {
		cmd = Command(argv[0], argv[1:]...)
	} else {
		cmd = Command(shell)
	}
	getMeta(cmd).shell = shell
	return cmd
}

// Start begins cmd asynchronously via the Node.js host bridge.
func Start(cmd *exec.Cmd) error {
	if !hasBridge() {
		return errors.New("host bridge (__azemuHost) not initialized")
	}
	m := getMeta(cmd)
	m.done = make(chan error, 1)
	stopKeepAlive := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopKeepAlive:
				return
			case <-t.C:
			}
		}
	}()

	opts := js.Global().Get("Object").New()
	opts.Set("dir", cmd.Dir)
	opts.Set("shell", m.shell)
	argsJS := js.Global().Get("Array").New(len(cmd.Args))
	for i, a := range cmd.Args {
		argsJS.SetIndex(i, a)
	}
	opts.Set("args", argsJS)
	if len(cmd.Env) > 0 {
		envJS := js.Global().Get("Array").New(len(cmd.Env))
		for i, kv := range cmd.Env {
			envJS.SetIndex(i, kv)
		}
		opts.Set("env", envJS)
	}
	opts.Set("inheritStdin", cmd.Stdin == os.Stdin)

	type chunk struct {
		w io.Writer
		b []byte
	}
	outCh := make(chan chunk, 128)
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		for c := range outCh {
			c.w.Write(c.b)
		}
	}()

	var onStdout, onStderr, onExit js.Func
	onStdout = js.FuncOf(func(_ js.Value, args []js.Value) any {
		if cmd.Stdout != nil && len(args) > 0 {
			b := make([]byte, args[0].Get("byteLength").Int())
			js.CopyBytesToGo(b, args[0])
			outCh <- chunk{w: cmd.Stdout, b: b}
		}
		return nil
	})
	onStderr = js.FuncOf(func(_ js.Value, args []js.Value) any {
		if cmd.Stderr != nil && len(args) > 0 {
			b := make([]byte, args[0].Get("byteLength").Int())
			js.CopyBytesToGo(b, args[0])
			outCh <- chunk{w: cmd.Stderr, b: b}
		}
		return nil
	})
	onExit = js.FuncOf(func(_ js.Value, args []js.Value) any {
		onStdout.Release()
		onStderr.Release()
		onExit.Release()
		close(outCh)
		close(stopKeepAlive)
		code := 0
		errStr := ""
		if len(args) > 0 && args[0].Type() == js.TypeNumber {
			code = args[0].Int()
		}
		if len(args) > 1 && args[1].Type() == js.TypeString {
			errStr = args[1].String()
		}
		go func() {
			<-outDone
			procMu.Lock()
			if errStr != "" {
				m.state = errStr
			} else {
				m.state = fmt.Sprintf("exit status %d", code)
			}
			procMu.Unlock()
			if errStr != "" {
				m.done <- errors.New(errStr)
			} else if code != 0 {
				m.done <- fmt.Errorf("exit status %d", code)
			} else {
				m.done <- nil
			}
			close(m.done)
		}()
		return nil
	})
	opts.Set("onStdout", onStdout)
	opts.Set("onStderr", onStderr)
	opts.Set("onExit", onExit)

	res := hostBridge().Call("spawnProc", opts)
	if errVal := res.Get("error"); errVal.Type() == js.TypeString && errVal.String() != "" {
		onStdout.Release()
		onStderr.Release()
		onExit.Release()
		close(outCh)
		close(stopKeepAlive)
		return errors.New(errVal.String())
	}
	m.handle = res
	pid := 1
	if p := res.Get("pid"); p.Type() == js.TypeNumber {
		pid = p.Int()
	}
	cmd.Process = &os.Process{Pid: pid}

	if m.ctx != nil {
		go func() {
			select {
			case <-m.ctx.Done():
				Terminate(cmd, true)
			case <-m.done:
			}
		}()
	}
	return nil
}

// Wait waits for cmd to exit.
func Wait(cmd *exec.Cmd) error {
	m := getMeta(cmd)
	if m.done == nil {
		return errors.New("exec: not started")
	}
	err := <-m.done
	procMu.Lock()
	delete(procMap, cmd)
	procMu.Unlock()
	return err
}

// Terminate stops the process tree rooted at cmd.
func Terminate(cmd *exec.Cmd, force bool) {
	procMu.Lock()
	m := procMap[cmd]
	procMu.Unlock()
	if m == nil || m.handle.IsUndefined() || m.handle.IsNull() {
		return
	}
	m.handle.Call("terminate", force)
}

// ExitState returns a human-readable exit status for a finished cmd.
func ExitState(cmd *exec.Cmd) string {
	procMu.Lock()
	defer procMu.Unlock()
	if m := procMap[cmd]; m != nil && m.state != "" {
		return m.state
	}
	return "exited"
}

// Run runs cmd to completion.
func Run(cmd *exec.Cmd) error {
	if err := Start(cmd); err != nil {
		return err
	}
	return Wait(cmd)
}

// Output runs cmd and returns its standard output.
func Output(cmd *exec.Cmd) ([]byte, error) {
	var out bytes.Buffer
	cmd.Stdout = &out
	err := Run(cmd)
	return out.Bytes(), err
}

// CombinedOutput runs cmd and returns its combined standard output and standard error.
func CombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := Run(cmd)
	return out.Bytes(), err
}

// ---- TCP Networking ----

// Listen binds a TCP listener on the host (or falls back to in-memory net.Listen in pure unit tests without a host bridge).
func Listen(network, address string) (net.Listener, error) {
	if !hasBridge() || hostBridge().Get("listen").IsUndefined() {
		return net.Listen(network, address)
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if host == "" {
		host = "127.0.0.1"
	}
	port, _ := strconv.Atoi(portStr)

	readyCh := make(chan int, 1)
	errCh := make(chan error, 1)
	connCh := make(chan net.Conn, 64)
	closed := make(chan struct{})
	var closeOnce sync.Once

	var onListening, onError, onConnection js.Func
	onListening = js.FuncOf(func(_ js.Value, args []js.Value) any {
		readyCh <- args[0].Int()
		return nil
	})
	onError = js.FuncOf(func(_ js.Value, args []js.Value) any {
		msg := "listen error"
		if len(args) > 0 {
			msg = args[0].String()
		}
		select {
		case errCh <- &net.OpError{Op: "listen", Net: network, Err: syscall.EADDRINUSE, Source: nil, Addr: nil}:
			_ = msg
		default:
		}
		return nil
	})
	onConnection = js.FuncOf(func(_ js.Value, args []js.Value) any {
		sock := args[0]
		localPort := args[1].Int()
		remotePort := args[2].Int()
		c := newJSConn(sock,
			&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: localPort},
			&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: remotePort},
		)
		select {
		case connCh <- c:
		case <-closed:
			c.Close()
		}
		return nil
	})

	srv := hostBridge().Call("listen", host, port, onListening, onError, onConnection)
	select {
	case actualPort := <-readyCh:
		onListening.Release()
		go func() {
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-closed:
					return
				case <-t.C:
				}
			}
		}()
		return &jsListener{
			srv:          srv,
			addr:         &net.TCPAddr{IP: net.ParseIP(host), Port: actualPort},
			connCh:       connCh,
			closed:       closed,
			closeOnce:    &closeOnce,
			onError:      onError,
			onConnection: onConnection,
		}, nil
	case err := <-errCh:
		onListening.Release()
		onError.Release()
		onConnection.Release()
		return nil, err
	}
}

type jsListener struct {
	srv          js.Value
	addr         *net.TCPAddr
	connCh       chan net.Conn
	closed       chan struct{}
	closeOnce    *sync.Once
	onError      js.Func
	onConnection js.Func
}

func (l *jsListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connCh:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *jsListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
		l.srv.Call("close")
		l.onError.Release()
		l.onConnection.Release()
	})
	return nil
}

func (l *jsListener) Addr() net.Addr { return l.addr }

// DialTimeout connects to address on network with a timeout.
func DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return DialContext(ctx, network, address)
}

// DialContext connects to address on network using ctx.
func DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if !hasBridge() || hostBridge().Get("connect").IsUndefined() {
		var d net.Dialer
		return d.DialContext(ctx, network, address)
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if host == "" {
		host = "127.0.0.1"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}

	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	var onConnect, onErr js.Func
	onConnect = js.FuncOf(func(_ js.Value, args []js.Value) any {
		onConnect.Release()
		onErr.Release()
		sock := args[0]
		localPort := args[1].Int()
		ip := net.ParseIP(host)
		if ip == nil {
			ip = net.ParseIP("127.0.0.1")
		}
		ch <- res{c: newJSConn(sock,
			&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: localPort},
			&net.TCPAddr{IP: ip, Port: port},
		)}
		return nil
	})
	onErr = js.FuncOf(func(_ js.Value, args []js.Value) any {
		onConnect.Release()
		onErr.Release()
		msg := "connect failed"
		if len(args) > 0 {
			msg = args[0].String()
		}
		ch <- res{err: errors.New(msg)}
		return nil
	})

	sock := hostBridge().Call("connect", host, port, onConnect, onErr)
	select {
	case r := <-ch:
		return r.c, r.err
	case <-ctx.Done():
		sock.Call("destroy")
		return nil, ctx.Err()
	}
}

type jsConn struct {
	sock      js.Value
	laddr     net.Addr
	raddr     net.Addr
	mu        sync.Mutex
	buf       bytes.Buffer
	notify    chan struct{}
	closed    bool
	eof       bool
	readErr   error
	rDeadline time.Time
	onData    js.Func
	onEnd     js.Func
	onError   js.Func
	onClose   js.Func
	closeOnce sync.Once
}

func newJSConn(sock js.Value, laddr, raddr net.Addr) *jsConn {
	c := &jsConn{
		sock:   sock,
		laddr:  laddr,
		raddr:  raddr,
		notify: make(chan struct{}, 1),
	}
	wake := func() {
		select {
		case c.notify <- struct{}{}:
		default:
		}
	}
	c.onData = js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) > 0 {
			b := make([]byte, args[0].Get("byteLength").Int())
			js.CopyBytesToGo(b, args[0])
			c.mu.Lock()
			c.buf.Write(b)
			c.mu.Unlock()
			wake()
		}
		return nil
	})
	c.onEnd = js.FuncOf(func(_ js.Value, _ []js.Value) any {
		c.mu.Lock()
		c.eof = true
		c.mu.Unlock()
		wake()
		return nil
	})
	c.onError = js.FuncOf(func(_ js.Value, args []js.Value) any {
		msg := "socket error"
		if len(args) > 0 {
			msg = args[0].String()
		}
		c.mu.Lock()
		if !strings.Contains(msg, "ECONNRESET") {
			c.readErr = errors.New(msg)
		} else {
			c.eof = true
		}
		c.mu.Unlock()
		wake()
		return nil
	})
	c.onClose = js.FuncOf(func(_ js.Value, _ []js.Value) any {
		c.mu.Lock()
		c.eof = true
		c.mu.Unlock()
		wake()
		return nil
	})
	sock.Call("attach", c.onData, c.onEnd, c.onError, c.onClose)
	return c
}

func (c *jsConn) Read(b []byte) (int, error) {
	for {
		c.mu.Lock()
		if c.buf.Len() > 0 {
			n, err := c.buf.Read(b)
			c.mu.Unlock()
			return n, err
		}
		if c.closed {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
		if c.readErr != nil {
			err := c.readErr
			c.mu.Unlock()
			return 0, err
		}
		if c.eof {
			c.mu.Unlock()
			return 0, io.EOF
		}
		dl := c.rDeadline
		c.mu.Unlock()

		var timer <-chan time.Time
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.After(d)
		}
		select {
		case <-c.notify:
		case <-timer:
			return 0, os.ErrDeadlineExceeded
		}
	}
}

func (c *jsConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	c.mu.Unlock()
	if len(b) == 0 {
		return 0, nil
	}
	u8 := uint8Array.New(len(b))
	js.CopyBytesToJS(u8, b)
	errStr := c.sock.Call("writeBytes", u8).String()
	if errStr != "" {
		return 0, errors.New(errStr)
	}
	return len(b), nil
}

func (c *jsConn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.eof = true
		c.mu.Unlock()
		select {
		case c.notify <- struct{}{}:
		default:
		}
		c.sock.Call("close")
		c.onData.Release()
		c.onEnd.Release()
		c.onError.Release()
		c.onClose.Release()
	})
	return nil
}

func (c *jsConn) LocalAddr() net.Addr  { return c.laddr }
func (c *jsConn) RemoteAddr() net.Addr { return c.raddr }

func (c *jsConn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	return nil
}

func (c *jsConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.rDeadline = t
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return nil
}

func (c *jsConn) SetWriteDeadline(t time.Time) error { return nil }
