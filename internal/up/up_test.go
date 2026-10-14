package up

import (
	"path/filepath"
	"testing"

	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/infra"
)

func TestAssignPortsStable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ports.json")
	a, _ := assignPorts(p, []string{"b", "c"})
	if a["b"] != contract.IngressBasePort || a["c"] != contract.IngressBasePort+1 {
		t.Fatalf("first run: %v", a)
	}
	// a new key sorting first must not shift existing ports, and a subset keeps them too
	b, _ := assignPorts(p, []string{"a", "b", "c"})
	if b["b"] != a["b"] || b["c"] != a["c"] || b["a"] != contract.IngressBasePort+2 {
		t.Fatalf("second run: %v", b)
	}
	c, _ := assignPorts(p, []string{"c"})
	if c["c"] != a["c"] {
		t.Fatalf("subset: %v", c)
	}
	if lp := c[contract.LogsPortKey]; lp == 0 || lp != a[contract.LogsPortKey] || lp == c["c"] {
		t.Fatalf("logs port must be stable and distinct: %v", c)
	}
}

func TestProxyConfigRoutes(t *testing.T) {
	cfg := &config.Config{Apps: map[string]*config.App{
		"web": {Resource: map[string]string{"dev": "app-web"}, Port: 8000},
		"fn":  {Resource: map[string]string{"dev": "func-x"}},
	}}
	m := &infra.Model{
		Apps:            []infra.App{{Name: "app-web", EasyAuth: true}, {Name: "func-x", HostStorage: "stfn"}},
		StorageAccounts: []infra.StorageAccount{{Name: "stdata"}},
	}
	pc := proxyConfig(cfg, m, "dev", map[string]int{"web": 7002, "fn": 7001}, "h", contract.Principal{Name: "a@b.c"}, containerUpstream(cfg), contract.AzuriteService)
	want := map[string]string{
		"app-web.azurewebsites.net":    "http://web:8000",
		"func-x.azurewebsites.net":     "http://fn:80",
		"stdata.blob.core.windows.net": "https://azurite:10000",
		"stfn.queue.core.windows.net":  "https://azurite:10001",
		"stfn.table.core.windows.net":  "https://azurite:10002",
	}
	for h, u := range want {
		if pc.Routes[h] != u {
			t.Errorf("route %s = %q, want %q", h, pc.Routes[h], u)
		}
	}
	if len(pc.Routes) != 2+6 {
		t.Errorf("got %d routes: %v", len(pc.Routes), pc.Routes)
	}
	if len(pc.Ingress) != 2 || pc.Ingress[1].Port != 7002 || !pc.Ingress[1].EasyAuth || pc.Ingress[0].EasyAuth {
		t.Errorf("ingress: %+v", pc.Ingress)
	}
}

func TestUUIDFor(t *testing.T) {
	a := uuidFor("dev@example.com")
	if a != uuidFor("dev@example.com") || a == uuidFor("x@example.com") || len(a) != 36 || a[14] != '5' {
		t.Fatalf("uuid %s", a)
	}
}
