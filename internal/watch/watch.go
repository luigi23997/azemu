// Package watch rebuilds apps when files in their build context change.
package watch

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

// Target is one watched app.
type Target struct {
	Key        string   // app key
	Dir        string   // build context dir (absolute)
	Dockerfile string   // absolute; its dockerignore (<Dockerfile>.dockerignore or <Dir>/.dockerignore) filters events
	Ignore     []string // extra dockerignore patterns, e.g. build output written into Dir by a native build
}

// Quiet is the debounce period: a batch fires after this long without new events.
var Quiet = 300 * time.Millisecond

// Log reports rebuild errors; watching continues.
var Log = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[watch] "+format+"\n", args...)
}

// Run watches targets and calls rebuild with batches of changed app keys (debounced, coalesced,
// never more than one rebuild in flight; changes made meanwhile form exactly one follow-up batch).
// Blocks until ctx is done.
func Run(ctx context.Context, targets []Target, rebuild func(ctx context.Context, keys []string) error) error {
	return run(ctx, targets, rebuild, nil)
}

var alwaysSkip = map[string]bool{".git": true, "node_modules": true, ".venv": true, "__pycache__": true, ".azemu": true}

type watcher struct {
	w        *fsnotify.Watcher
	targets  []Target
	ignore   []string // dockerignore path per target
	matchers []*patternmatcher.PatternMatcher
}

func run(ctx context.Context, targets []Target, rebuild func(ctx context.Context, keys []string) error, ready chan<- struct{}) error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fw.Close()
	s := &watcher{w: fw, targets: targets, ignore: make([]string, len(targets)), matchers: make([]*patternmatcher.PatternMatcher, len(targets))}
	for i, t := range targets {
		s.ignore[i] = t.Dockerfile + ".dockerignore"
		if _, err := os.Stat(s.ignore[i]); err != nil {
			s.ignore[i] = filepath.Join(t.Dir, ".dockerignore")
		}
		s.load(i)
	}
	for _, t := range targets {
		s.addTree(t.Dir)
	}
	if ready != nil {
		close(ready)
	}

	b := newBatcher(Quiet)
	done := make(chan struct{}, 1)
	fire := func() {
		keys, ok := b.poll(time.Now())
		if !ok {
			return
		}
		go func() {
			if err := rebuild(ctx, keys); err != nil {
				Log("rebuild %v: %v", keys, err)
			}
			done <- struct{}{}
		}()
	}
	for {
		var tick <-chan time.Time
		if d, ok := b.next(); ok {
			tick = time.After(time.Until(d))
		}
		select {
		case <-ctx.Done():
			if b.inflight {
				<-done
			}
			return nil
		case ev := <-fw.Events:
			b.add(time.Now(), s.handle(ev)...)
		case err := <-fw.Errors:
			Log("fsnotify: %v", err)
		case <-tick:
			fire()
		case <-done:
			b.finish()
			fire()
		}
	}
}

// load (re)reads target i's dockerignore; a missing or bad file means nothing is ignored.
func (s *watcher) load(i int) {
	s.matchers[i] = nil
	pats := s.targets[i].Ignore
	if f, err := os.Open(s.ignore[i]); err == nil {
		p, err := ignorefile.ReadAll(f)
		f.Close()
		if err != nil {
			Log("%s: %v", s.ignore[i], err)
		}
		pats = append(p, pats...)
	}
	if len(pats) == 0 {
		return
	}
	if m, err := patternmatcher.New(pats); err == nil {
		s.matchers[i] = m
	}
}

// rel returns p relative to target i's dir, slash-separated; ok=false if outside it.
func (s *watcher) rel(i int, p string) (string, bool) {
	r, err := filepath.Rel(s.targets[i].Dir, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(r), true
}

func (s *watcher) ignored(i int, rel string) bool {
	if rel == "." {
		return false
	}
	if skipped(rel) {
		return true
	}
	if m := s.matchers[i]; m != nil {
		ok, _ := m.MatchesOrParentMatches(rel)
		return ok
	}
	return false
}

func skipped(rel string) bool {
	for _, c := range strings.Split(rel, "/") {
		if alwaysSkip[c] {
			return true
		}
	}
	return false
}

func noise(base string) bool {
	return strings.HasSuffix(base, "~") || strings.HasSuffix(base, ".swp") || strings.HasSuffix(base, ".swx") ||
		base == "4913" || base == ".DS_Store"
}

// handle returns the keys of targets affected by ev, and keeps watches/ignore rules current.
func (s *watcher) handle(ev fsnotify.Event) []string {
	if ev.Op&^fsnotify.Chmod == 0 || noise(filepath.Base(ev.Name)) {
		return nil
	}
	var keys []string
	for i, t := range s.targets {
		rel, ok := s.rel(i, ev.Name)
		if !ok {
			continue
		}
		if ev.Name == s.ignore[i] {
			s.load(i)
		}
		if !s.ignored(i, rel) {
			keys = append(keys, t.Key)
		}
	}
	if ev.Has(fsnotify.Create) {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			s.addTree(ev.Name)
		}
	}
	return keys
}

// wantDir: some target contains dir p and does not ignore it. ponytail: with `!` exceptions we
// can't prune ignored dirs (an exception may re-include something inside), so only alwaysSkip prunes.
func (s *watcher) wantDir(p string) bool {
	for i := range s.targets {
		rel, ok := s.rel(i, p)
		if !ok {
			continue
		}
		if m := s.matchers[i]; m != nil && m.Exclusions() {
			if !skipped(rel) {
				return true
			}
		} else if !s.ignored(i, rel) {
			return true
		}
	}
	return false
}

// addTree watches root and every wanted directory below it (fsnotify is not recursive).
func (s *watcher) addTree(root string) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if !s.wantDir(p) {
			return filepath.SkipDir
		}
		if err := s.w.Add(p); err != nil {
			Log("watch %s: %v", p, err)
		}
		return nil
	})
}
