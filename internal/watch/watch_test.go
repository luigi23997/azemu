package watch

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestBatcher(t *testing.T) {
	t0 := time.Unix(0, 0)
	ms := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }
	b := newBatcher(300 * time.Millisecond)

	b.add(ms(0), "b")
	b.add(ms(100), "a")
	if _, ok := b.poll(ms(350)); ok {
		t.Fatal("fired before quiet period (restarted at 100ms)")
	}
	keys, ok := b.poll(ms(400))
	if !ok || !reflect.DeepEqual(keys, []string{"a", "b"}) {
		t.Fatalf("got %v %v", keys, ok)
	}

	// changes while in flight: no batch until finish, then exactly one with both.
	b.add(ms(500), "a")
	b.add(ms(600), "c")
	if _, ok := b.next(); ok {
		t.Fatal("next must be idle while in flight")
	}
	if _, ok := b.poll(ms(2000)); ok {
		t.Fatal("second in-flight batch")
	}
	b.finish()
	keys, ok = b.poll(ms(2000))
	if !ok || !reflect.DeepEqual(keys, []string{"a", "c"}) {
		t.Fatalf("follow-up: %v %v", keys, ok)
	}
	b.finish()
	if _, ok := b.poll(ms(9000)); ok {
		t.Fatal("phantom batch")
	}
}

type harness struct {
	batches chan []string
	cancel  context.CancelFunc
	dir     string
}

func start(t *testing.T, dockerignore string, targets func(dir string) []Target) *harness {
	t.Helper()
	old := Quiet
	Quiet = 50 * time.Millisecond
	t.Cleanup(func() { Quiet = old })
	dir := t.TempDir()
	if dockerignore != "" {
		os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte(dockerignore), 0o644)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{batches: make(chan []string, 10), cancel: cancel, dir: dir}
	ready := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		run(ctx, targets(dir), func(_ context.Context, keys []string) error {
			h.batches <- keys
			return nil
		}, ready)
	}()
	t.Cleanup(func() { cancel(); wg.Wait() })
	<-ready
	return h
}

func (h *harness) write(t *testing.T, rel string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, rel), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) expect(t *testing.T, want ...string) {
	t.Helper()
	select {
	case got := <-h.batches:
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("batch %v, want %v", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no batch, want %v", want)
	}
}

func (h *harness) expectNone(t *testing.T) {
	t.Helper()
	select {
	case got := <-h.batches:
		t.Fatalf("unexpected batch %v", got)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestTwoTargetsOneBatch(t *testing.T) {
	h := start(t, "", func(dir string) []Target {
		os.MkdirAll(filepath.Join(dir, "a"), 0o755)
		os.MkdirAll(filepath.Join(dir, "b"), 0o755)
		return []Target{
			{Key: "a", Dir: filepath.Join(dir, "a"), Dockerfile: filepath.Join(dir, "a", "Dockerfile")},
			{Key: "b", Dir: filepath.Join(dir, "b"), Dockerfile: filepath.Join(dir, "b", "Dockerfile")},
			{Key: "root", Dir: dir, Dockerfile: filepath.Join(dir, "Dockerfile")}, // nests both
		}
	})
	h.write(t, "a/f")
	h.write(t, "b/f")
	h.expect(t, "a", "b", "root")
	h.expectNone(t)
}

func single(dir string) []Target {
	return []Target{{Key: "app", Dir: dir, Dockerfile: filepath.Join(dir, "Dockerfile")}}
}

func TestIgnore(t *testing.T) {
	h := start(t, "*.log\n!keep.log\nbuild/\n", single)
	os.Mkdir(filepath.Join(h.dir, "build"), 0o755)
	h.write(t, "a.log")
	h.write(t, "x.swp")
	h.write(t, "file~")
	h.write(t, "build/out")
	h.expectNone(t)
	h.write(t, "keep.log")
	h.expect(t, "app")
}

func TestExtraIgnore(t *testing.T) {
	h := start(t, "", func(dir string) []Target {
		os.MkdirAll(filepath.Join(dir, "dist"), 0o755)
		return []Target{{Key: "app", Dir: dir, Dockerfile: filepath.Join(dir, "Dockerfile"), Ignore: []string{"dist"}}}
	})
	h.write(t, "dist/main.js")
	h.expectNone(t)
	h.write(t, "src.ts")
	h.expect(t, "app")
}

func TestNestedDirCreatedAfterStart(t *testing.T) {
	h := start(t, "", single)
	os.MkdirAll(filepath.Join(h.dir, "sub", "deep"), 0o755)
	h.expect(t, "app")
	h.write(t, "sub/deep/f")
	h.expect(t, "app")
}

func TestCoalesceWhileInFlight(t *testing.T) {
	old := Quiet
	Quiet = 50 * time.Millisecond
	defer func() { Quiet = old }()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	batches := make(chan []string, 10)
	ready := make(chan struct{})
	go run(ctx, single(dir), func(_ context.Context, keys []string) error {
		batches <- keys
		<-release
		return nil
	}, ready)
	<-ready
	f := filepath.Join(dir, "f")
	os.WriteFile(f, []byte("1"), 0o644)
	<-batches
	os.WriteFile(f, []byte("2"), 0o644) // mid-flight
	time.Sleep(300 * time.Millisecond)
	select {
	case <-batches:
		t.Fatal("second rebuild started while first in flight")
	default:
	}
	release <- struct{}{}
	select {
	case got := <-batches:
		if !reflect.DeepEqual(got, []string{"app"}) {
			t.Fatal(got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no follow-up batch")
	}
	release <- struct{}{}
	select {
	case got := <-batches:
		t.Fatalf("extra batch %v", got)
	case <-time.After(300 * time.Millisecond):
	}
}
