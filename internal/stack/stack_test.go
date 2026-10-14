package stack

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/host"
	"github.com/luigi23997/azemu/internal/infra"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestCompose(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("USERPROFILE", "/home/u") // os.UserHomeDir on Windows
	root := filepath.Join(t.TempDir(), "Demo.App")
	zipDir, _ := filepath.Abs("../build/testdata/py-uv")
	cfg := &config.Config{Root: root, Apps: map[string]*config.App{
		"func-a": {Path: zipDir, Kind: "zip", Runtime: "python:3.12"},
		"web":    {Path: "apps/web", Kind: "docker", Dockerfile: "Dockerfile", Platform: "linux/arm64"},
	}}
	m := &infra.Model{
		StorageAccounts: []infra.StorageAccount{{Name: "stb"}, {Name: "sta"}},
		Apps:            []infra.App{{Name: "eund-func-a", HostStorage: "sta"}, {Name: "w", HostStorage: "stc"}},
	}
	pc := &contract.ProxyConfig{
		Routes:  map[string]string{"sta.blob.core.windows.net": "https://azurite:10000", "eund-func-a.azurewebsites.net": "http://func-a:80"},
		Ingress: []contract.Ingress{{Port: 7001, Upstream: "http://func-a:80"}, {Port: 7002, Upstream: "http://web:80"}},
	}
	got, err := Compose(cfg, m, "dev", pc, "azemu-proxy:dev", 7000)
	if err != nil {
		t.Fatal(err)
	}
	got = []byte(filepath.ToSlash(string(got))) // Windows host paths, so one golden file fits every OS
	got = bytes.ReplaceAll(got, []byte(filepath.ToSlash(root)), []byte("<ROOT>"))
	got = bytes.ReplaceAll(got, []byte(filepath.ToSlash(filepath.Dir(zipDir))), []byte("<BUILD_TESTDATA>"))
	p := "testdata/compose.golden.yaml"
	if *update {
		os.MkdirAll("testdata", 0o755)
		os.WriteFile(p, got, 0o644)
	}
	if want, err := os.ReadFile(p); err != nil || !bytes.Equal(want, got) {
		t.Errorf("compose differs from %s (run with -update):\n%s", p, got)
	}
}

func TestComposeGraph(t *testing.T) {
	cfg := &config.Config{Root: "/p/demo", Graph: &config.Graph{}}
	pc := &contract.ProxyConfig{Graph: &contract.Graph{ReadOnly: true}}
	got, err := Compose(cfg, &infra.Model{}, "dev", pc, "azemu-proxy:dev", 7000)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- graph.microsoft.com", "- login.microsoftonline.com", "/p/demo/.azemu/graph:" + contract.GraphDir} {
		if !strings.Contains(filepath.ToSlash(string(got)), want) {
			t.Errorf("compose lacks %q:\n%s", want, got)
		}
	}
}

func TestProjectName(t *testing.T) {
	for in, want := range map[string]string{"/x/contoso.local": "azemu-contoso-local", "/x/My App_2": "azemu-my-app_2", "/": "azemu-project"} {
		if got := ProjectName(&config.Config{Root: in}); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestWriteEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env", "a.env")
	if err := WriteEnvFile(p, map[string]string{"B": "x=y z", "A": `"q" $HOME`}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "A=\"q\" $HOME\nB=x=y z\n" {
		t.Errorf("got %q", b)
	}
	if st, _ := os.Stat(p); host.GOOS() != "windows" && st.Mode().Perm() != 0o600 { // Windows has no Unix modes
		t.Errorf("mode %v", st.Mode())
	}
	if err := WriteEnvFile(p, map[string]string{"K": "a\nb"}); err == nil || !strings.Contains(err.Error(), "K") {
		t.Errorf("want newline error naming K, got %v", err)
	}
}

func TestParsePS(t *testing.T) {
	for _, in := range []string{
		`[{"Service":"a","State":"running","Health":"healthy"},{"Service":"b","State":"exited","ExitCode":1}]`,
		"{\"Service\":\"a\",\"State\":\"running\",\"Health\":\"healthy\"}\n{\"Service\":\"b\",\"State\":\"exited\",\"ExitCode\":1}\n",
	} {
		cs, err := parsePS([]byte(in))
		if err != nil || len(cs) != 2 || cs[0].Health != "healthy" || cs[1].ExitCode != 1 {
			t.Errorf("%q: %+v %v", in, cs, err)
		}
	}
}

func TestPrefixWriter(t *testing.T) {
	var b bytes.Buffer
	w := prefixed(&b)
	w.Write([]byte("one\ntw"))
	w.Write([]byte("o\n"))
	if b.String() != "[compose] one\n[compose] two\n" {
		t.Errorf("got %q", b.String())
	}
}
