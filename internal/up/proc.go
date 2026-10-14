package up

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luigi23997/azemu/internal/host"
)

// proc is a native child process (azurite, an app) with its whole process tree.
type proc struct {
	cmd      *exec.Cmd
	done     chan struct{}
	stopping atomic.Bool // set by stop: the exit is expected
}

// startProc starts argv (or, when shell is set, that command line through the OS shell) in dir.
func startProc(dir string, env []string, out io.Writer, shell string, argv ...string) (*proc, error) {
	cmd := host.NewProcessGroup(shell, argv)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second // a grandchild holding the output pipe must not block Wait
	if err := host.Start(cmd); err != nil {
		return nil, err
	}
	p := &proc{cmd: cmd, done: make(chan struct{})}
	go func() { host.Wait(cmd); close(p.done) }()
	return p, nil
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// stop ends the process tree: politely, then forcibly after 5s.
func (p *proc) stop() {
	p.stopping.Store(true)
	if p.exited() {
		return
	}
	host.Terminate(p.cmd, false)
	select {
	case <-p.done:
		return
	case <-time.After(5 * time.Second):
	}
	host.Terminate(p.cmd, true)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
}

// prefixer writes each line as "[name] line"; all prefixers sharing mu interleave whole lines.
type prefixer struct {
	mu   *sync.Mutex
	w    io.Writer
	name string
	part []byte
}

func (p *prefixer) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.part = append(p.part, b...)
	for {
		i := bytes.IndexByte(p.part, '\n')
		if i < 0 {
			return len(b), nil
		}
		fmt.Fprintf(p.w, "[%s] %s\n", p.name, bytes.TrimRight(p.part[:i], "\r"))
		p.part = p.part[i+1:]
	}
}
