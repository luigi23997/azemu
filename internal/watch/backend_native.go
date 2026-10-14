//go:build !js

package watch

import "github.com/fsnotify/fsnotify"

type backend interface {
	Add(name string) error
	Close() error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
}

type fsnotifyBackend struct {
	*fsnotify.Watcher
}

func newBackend() (backend, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &fsnotifyBackend{fw}, nil
}

func (b *fsnotifyBackend) Events() <-chan fsnotify.Event { return b.Watcher.Events }
func (b *fsnotifyBackend) Errors() <-chan error          { return b.Watcher.Errors }
