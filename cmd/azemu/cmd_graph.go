package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/graph"
	"github.com/luigi23997/azemu/internal/host"
)

const graphUsage = `usage: azemu graph login|status [-f dir]

With "graph: {}" in azemu.yaml, apps that call Microsoft Graph with an app registration (client id + secret) run
with YOUR delegated access instead: azemu hands them a fake token and swaps in your real one on graph.microsoft.com.
'graph login' signs you in (device code); the refresh token is kept in .azemu/graph/ (mode 0600, gitignored).
Writes are rejected unless graph.writable is true. Other Entra calls are passed through untouched.`

func runGraph(args []string) error {
	if len(args) == 0 || (args[0] != "login" && args[0] != "status") {
		return errors.New(graphUsage)
	}
	fs := flag.NewFlagSet("graph "+args[0], flag.ContinueOnError)
	file := fs.String("f", ".", "azemu.yaml or its directory")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*file)
	if err != nil {
		return err
	}
	if cfg.Graph == nil {
		return errors.New("add `graph: {}` to azemu.yaml first")
	}
	path := cfg.StatePath("graph", graph.TokenFile)
	if args[0] == "status" {
		return graphStatus(cfg, path)
	}
	g := *cfg.Graph
	if g.Tenant == "" {
		g.Tenant = graph.DefaultTenant
	}
	if g.ClientID == "" {
		g.ClientID = graph.DefaultClientID
	}
	if len(g.Scopes) == 0 {
		g.Scopes = graph.DefaultScopes
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: host.DialContext, ForceAttemptHTTP2: true}}
	t, err := graph.Login(ctx, hc, graph.Authority, g.Tenant, g.ClientID, g.Scopes, os.Stdout)
	if err != nil {
		return err
	}
	if err := graph.Save(path, t); err != nil {
		return err
	}
	fmt.Printf("signed in as %s; token saved to %s\n", t.Account, path)
	return nil
}

func graphStatus(cfg *config.Config, path string) error {
	t, err := graph.Load(path)
	if err != nil {
		fmt.Println("not signed in: run `azemu graph login`")
		return nil
	}
	mode := "read-only"
	if cfg.Graph.Writable {
		mode = "writable"
	}
	fmt.Printf("account: %s\ntenant:  %s\nscopes:  %s\nmode:    %s\naccess token valid until %s (refreshed automatically)\n",
		t.Account, t.Tenant, t.Scope, mode, t.Expires.Local().Format(time.RFC3339))
	return nil
}
