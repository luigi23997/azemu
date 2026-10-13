package build

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/luigi23997/azemu/internal/config"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", "golden", name)
	if *update {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, got, 0o644)
	}
	want, err := os.ReadFile(p)
	if err != nil || string(want) != string(got) {
		t.Errorf("%s differs from golden (run with -update):\n%s", name, got)
	}
}

func TestGenerate(t *testing.T) {
	for _, c := range []struct{ dir, runtime string }{
		{"py-uv", "python:3.12"}, {"py-req", "python:3.11"}, {"node-build", "node:20"}, {"node-plain", "node:22"},
	} {
		df, ign, err := generate(filepath.Join("testdata", c.dir), c.runtime)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, c.dir+".Dockerfile", df)
		golden(t, c.dir+".Dockerfile.dockerignore", ign)
	}
	if _, _, err := generate("testdata/py-uv", "dotnet-isolated:8"); err == nil {
		t.Error("want unsupported runtime error")
	}
}

func TestPrepare(t *testing.T) {
	root, abs := t.TempDir(), t.TempDir()
	cfg := &config.Config{Root: root, Apps: map[string]*config.App{
		"z": {Path: "x", Kind: "zip", Runtime: "python:3.12"},
		"d": {Path: abs, Kind: "docker"},
	}}
	ctx, df, err := Prepare(cfg, "d")
	if err != nil || ctx != abs || df != filepath.Join(abs, "Dockerfile") {
		t.Fatalf("docker: %q %q %v", ctx, df, err)
	}
	cfg.Apps["z"].Path = mustAbs(t, "testdata/py-req")
	_, df, err = Prepare(cfg, "z")
	if err != nil || df != filepath.Join(root, ".azemu/build/z/Dockerfile") {
		t.Fatalf("zip: %q %v", df, err)
	}
	st1, _ := os.Stat(df)
	if _, _, err = Prepare(cfg, "z"); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(df)
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Error("unchanged Dockerfile was rewritten")
	}
	if _, err := os.Stat(df + ".dockerignore"); err != nil {
		t.Error(err)
	}
}

func mustAbs(t *testing.T, p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
