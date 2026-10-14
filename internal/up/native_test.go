package up

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/host"
)

func TestNativeEnv(t *testing.T) {
	ports := map[string]int{proxyPortKey: 7100, identityPortKey: 7101, listenKey("fn"): 7102}
	got := nativeEnv(
		map[string]string{"AzureWebJobsScriptRoot": "/home/site/wwwroot", "IDENTITY_ENDPOINT": "http://azemu-proxy:8081/msi/token", "MY": "v"},
		map[string]string{"VIRTUAL_ENV": "/v"}, "/c", ports, "fn")
	want := []string{
		"HTTPS_PROXY=http://127.0.0.1:7100", "NO_PROXY=localhost,127.0.0.1", "PORT=7102", "MY=v", "VIRTUAL_ENV=/v",
		"IDENTITY_ENDPOINT=http://127.0.0.1:7101/msi/token", "SSL_CERT_FILE=" + filepath.Join("/c", "bundle.pem"),
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing %s in %v", w, got)
		}
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, "AzureWebJobsScriptRoot=") {
			t.Errorf("container script root kept: %s", kv)
		}
	}
}

func TestBuildOutputs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"build":"tsc"}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte("{\n // comment\n \"compilerOptions\": {\"outDir\": \"./out\",}\n}"), 0o644)
	if got := buildOutputs(&config.App{Kind: "zip", Runtime: "node:20"}, dir); !slices.Equal(got, []string{"dist", "out"}) {
		t.Errorf("got %v", got)
	}
	if got := buildOutputs(&config.App{Kind: "zip", Runtime: "python:3.12"}, dir); got != nil {
		t.Errorf("python: %v", got)
	}
}

// stop must end the whole tree: a grandchild still holding the output pipe would hold Wait for WaitDelay (5s).
func TestProcStopEndsTree(t *testing.T) {
	shell := "sleep 30 & sleep 30"
	if host.GOOS() == "windows" {
		shell = "start /b ping -n 30 127.0.0.1 & ping -n 30 127.0.0.1"
	}
	var out bytes.Buffer
	p, err := startProc(t.TempDir(), os.Environ(), &prefixer{mu: &sync.Mutex{}, w: &out, name: "t"}, shell)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	p.stop()
	if !p.exited() {
		t.Fatal("still running after stop")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("stop took %v: part of the tree survived", d)
	}
}
