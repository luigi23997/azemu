//go:build js

package watch

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type backend interface {
	Add(name string) error
	Close() error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
}

// pollBackend polls added directories for file/directory creates, writes, and removals when
// running under GOOS=js (where fsnotify has no OS backend).
type pollBackend struct {
	mu     sync.Mutex
	dirs   map[string]map[string]fileSnap
	events chan fsnotify.Event
	errors chan error
	stop   chan struct{}
	once   sync.Once
}

type fileSnap struct {
	modTime time.Time
	size    int64
	isDir   bool
}

func newBackend() (backend, error) {
	p := &pollBackend{
		dirs:   map[string]map[string]fileSnap{},
		events: make(chan fsnotify.Event, 64),
		errors: make(chan error, 8),
		stop:   make(chan struct{}),
	}
	go p.loop()
	return p, nil
}

func (p *pollBackend) Add(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.dirs[name]; !ok {
		p.dirs[name] = snapshotDir(name)
	}
	return nil
}

func (p *pollBackend) Close() error {
	p.once.Do(func() { close(p.stop) })
	return nil
}

func (p *pollBackend) Events() <-chan fsnotify.Event { return p.events }
func (p *pollBackend) Errors() <-chan error          { return p.errors }

func snapshotDir(dir string) map[string]fileSnap {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	m := make(map[string]fileSnap, len(ents))
	for _, e := range ents {
		if fi, err := e.Info(); err == nil {
			m[e.Name()] = fileSnap{modTime: fi.ModTime(), size: fi.Size(), isDir: fi.IsDir()}
		}
	}
	return m
}

func (p *pollBackend) loop() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.scan()
		}
	}
}

func (p *pollBackend) scan() {
	p.mu.Lock()
	dirs := make([]string, 0, len(p.dirs))
	for d := range p.dirs {
		dirs = append(dirs, d)
	}
	p.mu.Unlock()

	var evs []fsnotify.Event
	for _, d := range dirs {
		cur := snapshotDir(d)
		p.mu.Lock()
		prev, ok := p.dirs[d]
		if !ok {
			p.mu.Unlock()
			continue
		}
		p.dirs[d] = cur
		p.mu.Unlock()

		for name, cs := range cur {
			full := filepath.Join(d, name)
			ps, existed := prev[name]
			if !existed {
				evs = append(evs, fsnotify.Event{Name: full, Op: fsnotify.Create})
				if cs.isDir {
					// Also emit Create events for files already created inside a new directory tree.
					filepath.WalkDir(full, func(sub string, de fs.DirEntry, err error) error {
						if err == nil && sub != full {
							evs = append(evs, fsnotify.Event{Name: sub, Op: fsnotify.Create})
						}
						return nil
					})
				}
			} else if !cs.isDir && (!cs.modTime.Equal(ps.modTime) || cs.size != ps.size) {
				evs = append(evs, fsnotify.Event{Name: full, Op: fsnotify.Write})
			}
		}
		for name := range prev {
			if _, still := cur[name]; !still {
				evs = append(evs, fsnotify.Event{Name: filepath.Join(d, name), Op: fsnotify.Remove})
			}
		}
	}
	for _, ev := range evs {
		select {
		case p.events <- ev:
		case <-p.stop:
			return
		}
	}
}
