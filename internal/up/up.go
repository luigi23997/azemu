// Package up orchestrates `azemu up`: plan, builds, platform, seeds and apps run as one parallel graph.
package up

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/luigi23997/azemu/internal/build"
	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/graph"
	"github.com/luigi23997/azemu/internal/host"
	"github.com/luigi23997/azemu/internal/infra"
	"github.com/luigi23997/azemu/internal/proxy"
	"github.com/luigi23997/azemu/internal/settings"
	"github.com/luigi23997/azemu/internal/stack"
	"github.com/luigi23997/azemu/internal/watch"
)

type Options struct {
	File          string // azemu.yaml or its directory
	Env           string // default: env in azemu.yaml
	As            string // Easy Auth principal; default identity.as, then git config user.email
	Refresh       bool
	NoWatch       bool
	KeepTelemetry bool
	Apps          []string // default: all
	Version       string
}

type buildResult struct {
	err error
	dur time.Duration
}

type phases struct {
	mu  sync.Mutex
	rec []string
}

func (p *phases) add(name string, since time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rec = append(p.rec, fmt.Sprintf("%s %.1fs", name, time.Since(since).Seconds()))
}

// load reads azemu.yaml and returns it with the effective env and (when apps are named) only those apps.
func load(file, envFlag string, apps []string) (*config.Config, string, error) {
	cfg, err := config.Load(file)
	if err != nil {
		return nil, "", err
	}
	env := envFlag
	if env == "" {
		env = cfg.Env
	}
	return subset(cfg, apps), env, validate(cfg, apps)
}

func validate(cfg *config.Config, apps []string) error {
	for _, a := range apps {
		if cfg.Apps[a] == nil {
			return fmt.Errorf("unknown app %q (have: %s)", a, strings.Join(slices.Sorted(maps.Keys(cfg.Apps)), ", "))
		}
	}
	return nil
}

// subset returns a copy of cfg restricted to keys (all apps when none given).
// ponytail: unselected apps are absent from the generated compose and proxy routes; up all apps to wire them together.
func subset(cfg *config.Config, keys []string) *config.Config {
	if len(keys) == 0 {
		return cfg
	}
	c := *cfg
	c.Apps = map[string]*config.App{}
	for _, k := range keys {
		if a := cfg.Apps[k]; a != nil {
			c.Apps[k] = a
		}
	}
	return &c
}

func yamlPath(file string) string {
	if strings.HasSuffix(file, ".yaml") {
		return file
	}
	return filepath.Join(file, config.FileName)
}

// Run is `azemu up`.
func Run(ctx context.Context, o Options) error {
	t0 := time.Now()
	cfg, env, err := load(o.File, o.Env, o.Apps)
	if err != nil {
		return err
	}
	if len(cfg.Apps) == 0 {
		return errors.New("no apps in azemu.yaml")
	}
	ph := &phases{}
	proxyImage := os.Getenv("AZEMU_PROXY_IMAGE")
	if proxyImage == "" {
		proxyImage = "azemu/proxy:dev"
	}
	if err := preflight(ctx, o.Version, proxyImage); err != nil {
		return err
	}
	ph.add("preflight", t0)
	header, err := identityHeader(cfg)
	if err != nil {
		return err
	}
	ports, err := assignPorts(cfg.StatePath("ports.json"), slices.Sorted(maps.Keys(cfg.Apps)))
	if err != nil {
		return err
	}
	if err := stack.WriteBuildCompose(cfg, env, proxyImage, ports[contract.LogsPortKey]); err != nil {
		return err
	}
	keys := slices.Sorted(maps.Keys(cfg.Apps))

	var (
		model   *infra.Model
		planned = make(chan struct{})
		mu      sync.Mutex
		results = map[string]*buildResult{}
		envs    map[string]map[string]string
	)
	g, gctx := errgroup.WithContext(ctx)

	// [A] plan, then everything that needs the model.
	g.Go(func() error {
		t := time.Now()
		m, err := infra.Load(gctx, cfg, env, o.Refresh)
		if err != nil {
			return err
		}
		ph.add("plan", t)
		for _, w := range m.Warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		warnMismatch(cfg, m, env)
		pc := proxyConfig(cfg, m, env, ports, header, principalFor(cfg, o.As), containerUpstream(cfg), contract.AzuriteService)
		if err := prepareGraph(cfg); err != nil {
			return err
		}
		if err := writeJSON(cfg.StatePath("proxy.json"), pc); err != nil {
			return err
		}
		if err := stack.WriteCompose(cfg, m, env, pc, proxyImage, ports[contract.LogsPortKey]); err != nil {
			return err
		}
		t = time.Now()
		e, err := resolveAll(cfg, m, env, header, o.KeepTelemetry)
		if err != nil {
			return err
		}
		if err := writeEnvFiles(cfg, e); err != nil {
			return err
		}
		ph.add("settings", t)
		mu.Lock()
		model, envs = m, e
		mu.Unlock()
		close(planned)
		return nil
	})

	// [B] one compose build per app, concurrently; a failing app does not cancel the others.
	g.Go(func() error {
		t := time.Now()
		for k, r := range buildApps(gctx, cfg, true, keys) {
			mu.Lock()
			results[k] = r
			mu.Unlock()
		}
		ph.add("build", t)
		return nil
	})

	// [D] log dashboard: independent of everything else, so it shows startup logs; a failure only warns.
	// [C] waits for it, so two `compose up` never create the project network at once.
	logsUp := make(chan struct{})
	g.Go(func() error {
		defer close(logsUp)
		if err := stack.UpLogs(gctx, cfg); err != nil && gctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "warning: logs dashboard did not start:", err)
		}
		return nil
	})

	// [C] CA + proxy + azurite, then storage objects and seeds.
	g.Go(func() error {
		select {
		case <-planned:
		case <-gctx.Done():
			return gctx.Err()
		}
		<-logsUp
		t := time.Now()
		caPEM, err := proxy.EnsureCA(contract.HostCADir())
		if err != nil {
			return err
		}
		if err := stack.Up(gctx, cfg, contract.ProxyService, contract.AzuriteService); err != nil {
			return err
		}
		ph.add("platform", t)
		t = time.Now()
		if err := stack.CreateObjects(gctx, model, caPEM); err != nil {
			return err
		}
		if err := runSeedsIfNeeded(gctx, cfg, header, false); err != nil {
			return err
		}
		ph.add("storage+seeds", t)
		return nil
	})
	if err := g.Wait(); err != nil {
		return err
	}

	// A+B+C+seeds -> start every app that built.
	var built []string
	for _, k := range keys {
		if results[k] != nil && results[k].err == nil {
			built = append(built, k)
		}
	}
	t := time.Now()
	var upErr error
	if len(built) > 0 {
		upErr = stack.Up(ctx, cfg, built...)
	}
	ph.add("start", t)
	if upErr != nil {
		return upErr
	}
	printStatus(ctx, cfg, env, ports, results)
	ph.add("total", t0)
	fmt.Println("timings:", strings.Join(ph.rec, " | "))

	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
		}
	}
	if o.NoWatch {
		if failed > 0 {
			return fmt.Errorf("%d app(s) failed to build", failed)
		}
		return nil
	}
	return watchLoop(ctx, o, cfg, model, env, header, envs, built)
}

// ---- builds ----

// buildApps runs one `docker compose build <svc>` per app concurrently and prints each outcome.
func buildApps(ctx context.Context, cfg *config.Config, preplan bool, keys []string) map[string]*buildResult {
	res := map[string]*buildResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			fmt.Printf("[build] %s %s start\n", start.Format("15:04:05.000"), k)
			var out bytes.Buffer
			err := stack.BuildService(ctx, cfg, preplan, &out, k)
			r := &buildResult{err: err, dur: time.Since(start)}
			end := time.Now().Format("15:04:05.000")
			if err != nil && ctx.Err() != nil {
				fmt.Printf("[build] %s %s cancelled\n", end, k)
			} else if err != nil {
				fmt.Printf("[build] %s %s FAILED after %.1fs: %v\n%s", end, k, r.dur.Seconds(), err, tail(out.String(), 25))
			} else {
				fmt.Printf("[build] %s %s done in %.1fs\n", end, k, r.dur.Seconds())
			}
			mu.Lock()
			res[k] = r
			mu.Unlock()
		}()
	}
	wg.Wait()
	return res
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "    | " + strings.Join(lines, "\n    | ") + "\n"
}

// ---- preflight ----

func preflight(ctx context.Context, version, image string) error {
	if err := host.Run(host.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")); err != nil {
		return errors.New("docker is not reachable: start Docker Desktop (or dockerd) and retry (azemu doctor)")
	}
	if host.Run(host.CommandContext(ctx, "docker", "image", "inspect", image)) == nil {
		return nil
	}
	if version != "dev" {
		return nil // compose pulls it
	}
	src := findSource()
	if src == "" {
		return fmt.Errorf("proxy image %s not found: run `docker build -t %s <azemu repo>`", image, image)
	}
	fmt.Printf("building proxy image %s from %s\n", image, src)
	cmd := host.CommandContext(ctx, "docker", "build", "-q", "-t", image, src)
	cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
	return host.Run(cmd)
}

// findSource looks upward from the working dir and the executable for the azemu repo (a dev build).
func findSource() string {
	var starts []string
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	for _, d := range starts {
		for ; d != filepath.Dir(d); d = filepath.Dir(d) {
			b, _ := os.ReadFile(filepath.Join(d, "go.mod"))
			if _, err := os.Stat(filepath.Join(d, "Dockerfile")); err == nil && bytes.Contains(b, []byte("module github.com/luigi23997/azemu")) {
				return d
			}
		}
	}
	return ""
}

// ---- proxy config, ports, identity ----

// identityHeader is random but persisted so the proxy and app containers are not recreated on every run.
func identityHeader(cfg *config.Config) (string, error) {
	p := cfg.StatePath("identity-header")
	if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	r := make([]byte, 16)
	if _, err := rand.Read(r); err != nil {
		return "", err
	}
	h := hex.EncodeToString(r)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return h, os.WriteFile(p, []byte(h), 0o600)
}

// assignPorts returns a host ingress port per key, persisted in path: known keys keep theirs, new ones get the next free.
// The logs dashboard gets the reserved key contract.LogsPortKey: 7000 when free, else a free port picked by the OS.
// ponytail: app ports are assumed free on the host (a busy one surfaces as a compose error, azemu doctor checks);
// a persisted logs port taken later by another process only warns at `up`.
func assignPorts(path string, keys []string) (map[string]int, error) {
	ports := map[string]int{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &ports); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	used := map[int]bool{}
	for _, p := range ports {
		used[p] = true
	}
	changed := false
	if _, ok := ports[contract.LogsPortKey]; !ok {
		p := freePort(contract.LogsPort)
		ports[contract.LogsPortKey], used[p], changed = p, true, true
	}
	for _, k := range keys {
		if _, ok := ports[k]; ok {
			continue
		}
		p := contract.IngressBasePort
		for used[p] {
			p++
		}
		ports[k], used[p], changed = p, true, true
	}
	if changed {
		return ports, writeJSON(path, ports)
	}
	return ports, nil
}

// freePort returns want when it can be bound on localhost, else any free port.
func freePort(want int) int {
	for _, a := range []string{fmt.Sprintf("127.0.0.1:%d", want), "127.0.0.1:0"} {
		if l, err := host.Listen("tcp", a); err == nil {
			defer l.Close()
			return l.Addr().(*net.TCPAddr).Port
		}
	}
	return want
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func principalFor(cfg *config.Config, as string) contract.Principal {
	name := as
	if name == "" {
		name = cfg.Identity.As
	}
	if name == "" {
		b, _ := host.Output(host.Command("git", "config", "user.email"))
		name = strings.TrimSpace(string(b))
	}
	if name == "" {
		name = "dev@azemu.local"
		fmt.Fprintf(os.Stderr, "warning: no Easy Auth identity (--as, identity.as, git config user.email); using %s\n", name)
	}
	return contract.Principal{Name: name, ID: uuidFor(name)}
}

// uuidFor derives a stable UUID (v5-style: sha1, version and variant bits set) from name.
func uuidFor(name string) string {
	h := sha1.Sum([]byte("azemu:" + name))
	h[6] = h[6]&0x0f | 0x50
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func appPort(a *config.App) int {
	if a.Port > 0 {
		return a.Port
	}
	return 80
}

// containerUpstream is where an app listens inside the compose network.
func containerUpstream(cfg *config.Config) func(string) string {
	return func(k string) string { return fmt.Sprintf("http://%s:%d", k, appPort(cfg.Apps[k])) }
}

// proxyConfig builds the routing table: app hostnames to their upstream, storage hostnames to azurite, one ingress per app.
func proxyConfig(cfg *config.Config, m *infra.Model, env string, ports map[string]int, header string, pr contract.Principal,
	upstream func(key string) string, azuriteHost string) *contract.ProxyConfig {
	pc := &contract.ProxyConfig{IdentityHeader: header, Routes: map[string]string{}, Principal: pr}
	for _, k := range slices.Sorted(maps.Keys(cfg.Apps)) {
		res := cfg.ResourceName(k, env)
		if res == "" {
			continue
		}
		up := upstream(k)
		host := res + ".azurewebsites.net"
		pc.Routes[host] = up
		easy := false
		if a := m.App(res); a != nil {
			easy = a.EasyAuth
		}
		pc.Ingress = append(pc.Ingress, contract.Ingress{Port: ports[k], Upstream: up, Host: host, EasyAuth: easy})
	}
	accts := map[string]bool{}
	for _, a := range m.StorageAccounts {
		accts[a.Name] = true
	}
	for _, a := range m.Apps {
		if a.HostStorage != "" {
			accts[a.HostStorage] = true
		}
	}
	for a := range accts {
		for svc, port := range map[string]int{"blob": contract.AzuriteBlobPort, "queue": contract.AzuriteQueuePort, "table": contract.AzuriteTablePort} {
			pc.Routes[fmt.Sprintf("%s.%s.core.windows.net", a, svc)] = fmt.Sprintf("https://%s:%d", azuriteHost, port)
		}
	}
	if cfg.Graph != nil {
		pc.Graph = &contract.Graph{ReadOnly: !cfg.Graph.Writable}
	}
	return pc
}

// prepareGraph creates the token directory (so docker does not create it as root) and warns when nobody has logged in yet.
func prepareGraph(cfg *config.Config) error {
	if cfg.Graph == nil {
		return nil
	}
	if err := os.MkdirAll(cfg.StatePath("graph"), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(cfg.StatePath("graph", graph.TokenFile)); err != nil {
		fmt.Fprintln(os.Stderr, "warning: graph is configured but not signed in: run `azemu graph login`")
	}
	return nil
}

func warnMismatch(cfg *config.Config, m *infra.Model, env string) {
	for k, a := range cfg.Apps {
		p := m.App(cfg.ResourceName(k, env))
		if p == nil {
			continue
		}
		rt := p.Runtime
		if p.RuntimeVersion != "" {
			rt += ":" + p.RuntimeVersion
		}
		if p.Kind != a.Kind || (a.Kind == "zip" && rt != a.Runtime) {
			fmt.Fprintf(os.Stderr, "warning: %s: azemu.yaml says %s %s, terraform says %s %s; run `azemu init --update`\n", k, a.Kind, a.Runtime, p.Kind, rt)
		}
	}
}

// ---- settings ----

// resolveAll resolves every app's env concurrently; failures are merged (deduplicated) into one error.
func resolveAll(cfg *config.Config, m *infra.Model, env, header string, keepTelemetry bool) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for k := range cfg.Apps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := settings.Resolve(cfg, m, k, env, settings.Options{IdentityHeader: header, KeepTelemetry: keepTelemetry})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else {
				out[k] = e
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		seen := map[string]bool{}
		var msgs []string
		for _, e := range errs {
			if !seen[e.Error()] {
				seen[e.Error()] = true
				msgs = append(msgs, e.Error())
			}
		}
		slices.Sort(msgs)
		return nil, errors.New(strings.Join(msgs, "\n"))
	}
	return out, nil
}

func writeEnvFiles(cfg *config.Config, envs map[string]map[string]string) error {
	for k, e := range envs {
		if err := stack.WriteEnvFile(cfg.StatePath("env", k+".env"), e); err != nil {
			return fmt.Errorf("app %s: %w", k, err)
		}
	}
	return nil
}

// ---- seeds ----

func seedHash(cfg *config.Config) string {
	b, _ := json.Marshal(cfg.Seed)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// runSeedsIfNeeded runs the seed steps on the first up or when the seed list changed (force: always).
func runSeedsIfNeeded(ctx context.Context, cfg *config.Config, header string, force bool) error {
	if len(cfg.Seed) == 0 {
		return nil
	}
	marker := cfg.StatePath("seeded")
	h := seedHash(cfg)
	if old, _ := os.ReadFile(marker); !force && string(old) == h {
		return nil
	}
	proj := stack.ProjectName(cfg)
	env := map[string]string{
		"IDENTITY_ENDPOINT": contract.IdentityEndpoint(), "IDENTITY_HEADER": header,
		"REQUESTS_CA_BUNDLE": contract.CABundleFile, "SSL_CERT_FILE": contract.CABundleFile, "CURL_CA_BUNDLE": contract.CABundleFile,
		"NODE_EXTRA_CA_CERTS": contract.CACertFile,
		"AZURE_STORAGE_KEY":   contract.AzuriteKey, "AZURE_STORAGE_AUTH_MODE": "key",
	}
	g, ctx := errgroup.WithContext(ctx)
	for i, s := range cfg.Seed {
		g.Go(func() error {
			args := []string{"run", "--rm", "--network", proj + "_default",
				"-v", proj + "_" + contract.CertsVolume + ":" + contract.CertsDir + ":ro",
				"-v", cfg.Root + ":/src", "-w", "/src"}
			for _, k := range slices.Sorted(maps.Keys(env)) {
				args = append(args, "-e", k+"="+env[k])
			}
			args = append(args, s.Image, "sh", "-c", s.Run)
			var out bytes.Buffer
			cmd := host.CommandContext(ctx, "docker", args...)
			cmd.Stdout, cmd.Stderr = &out, &out
			start := time.Now()
			if err := host.Run(cmd); err != nil {
				return fmt.Errorf("seed %d (%s): %w\n%s", i+1, s.Image, err, tail(out.String(), 25))
			}
			fmt.Printf("[seed] %d done in %.1fs\n", i+1, time.Since(start).Seconds())
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	return os.WriteFile(marker, []byte(h), 0o644)
}

// Seed is `azemu seed`: run the seed steps again.
func Seed(ctx context.Context, file, envFlag string) error {
	cfg, _, err := load(file, envFlag, nil)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(cfg.StatePath("identity-header"))
	if err != nil {
		return errors.New("stack has never been started: run `azemu up` first")
	}
	return runSeedsIfNeeded(ctx, cfg, strings.TrimSpace(string(b)), true)
}

// ---- status / logs / down ----

func printStatus(ctx context.Context, cfg *config.Config, env string, ports map[string]int, results map[string]*buildResult) {
	states := map[string]string{}
	if cs, err := stack.PS(ctx, cfg); err == nil {
		for _, c := range cs {
			s := c.State
			if c.Health != "" {
				s += " (" + c.Health + ")"
			}
			states[c.Service] = s
		}
	}
	printTable(cfg, env, ports, states, results)
	if p := ports[contract.LogsPortKey]; p > 0 {
		fmt.Printf("\nLogs dashboard: http://127.0.0.1:%d\n", p)
	}
}

func printTable(cfg *config.Config, env string, ports map[string]int, states map[string]string, results map[string]*buildResult) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tAZURE HOST\tLOCAL URL\tSTATE\tBUILD")
	for _, k := range slices.Sorted(maps.Keys(cfg.Apps)) {
		state, bt := states[k], "-"
		if state == "" {
			state = "not running"
		}
		if r := results[k]; r != nil {
			bt = fmt.Sprintf("%.1fs", r.dur.Seconds())
			if r.err != nil {
				state, bt = "BUILD FAILED", bt+" (failed)"
			}
		}
		url := "-"
		if p, ok := ports[k]; ok {
			url = fmt.Sprintf("http://127.0.0.1:%d", p)
		}
		fmt.Fprintf(tw, "%s\t%s.azurewebsites.net\t%s\t%s\t%s\n", k, cfg.ResourceName(k, env), url, state, bt)
	}
	tw.Flush()
}

// Status is `azemu status`.
func Status(ctx context.Context, file, envFlag string) error {
	cfg, env, err := load(file, envFlag, nil)
	if err != nil {
		return err
	}
	ports := map[string]int{}
	if b, err := os.ReadFile(cfg.StatePath("ports.json")); err == nil {
		json.Unmarshal(b, &ports)
	}
	printStatus(ctx, cfg, env, ports, nil)
	return nil
}

// Logs is `azemu logs`: follow, all services when none are named.
func Logs(ctx context.Context, file string, services []string) error {
	cfg, _, err := load(file, "", nil)
	if err != nil {
		return err
	}
	return stack.Logs(ctx, cfg, true, services...)
}

// Down is `azemu down`.
func Down(ctx context.Context, file string, volumes bool) error {
	cfg, _, err := load(file, "", nil)
	if err != nil {
		return err
	}
	if err := stack.Down(ctx, cfg, volumes); err != nil {
		return err
	}
	if volumes {
		os.Remove(cfg.StatePath("seeded")) // azurite data is gone: seeds must run again
	}
	return nil
}

// ---- watch ----

func watchLoop(ctx context.Context, o Options, cfg *config.Config, m *infra.Model, env, header string,
	envs map[string]map[string]string, built []string) error {
	var mu sync.Mutex // serialises compose up calls and guards built/envs
	builtSet := map[string]bool{}
	for _, k := range built {
		builtSet[k] = true
	}
	var targets []watch.Target
	for _, k := range slices.Sorted(maps.Keys(cfg.Apps)) {
		dir, df, err := build.Prepare(cfg, k)
		if err != nil {
			return err
		}
		targets = append(targets, watch.Target{Key: k, Dir: dir, Dockerfile: df})
	}

	rebuild := func(ctx context.Context, keys []string) error {
		fmt.Printf("[watch] rebuilding %s\n", strings.Join(keys, ", "))
		results := buildApps(ctx, cfg, false, keys)
		mu.Lock()
		defer mu.Unlock()
		var ok []string
		for _, k := range keys {
			if results[k].err == nil {
				ok = append(ok, k)
				builtSet[k] = true
			} else {
				fmt.Printf("[watch] %s: build failed, the previous container keeps running\n", k)
			}
		}
		if len(ok) == 0 {
			return nil
		}
		if err := stack.UpNoDeps(ctx, cfg, ok...); err != nil {
			return err
		}
		fmt.Printf("[watch] %s updated\n", strings.Join(ok, ", "))
		return nil
	}

	go watchSettings(ctx, o, cfg, m, env, header, envs, &mu, func(ctx context.Context, changed map[string]map[string]string) error {
		var restart []string
		for _, k := range slices.Sorted(maps.Keys(changed)) {
			if err := stack.WriteEnvFile(cfg.StatePath("env", k+".env"), changed[k]); err != nil {
				return err
			}
			envs[k] = changed[k]
			if builtSet[k] {
				restart = append(restart, k)
			}
		}
		if len(restart) == 0 {
			return nil
		}
		fmt.Printf("[watch] settings changed, restarting %s\n", strings.Join(restart, ", "))
		return stack.UpNoDeps(ctx, cfg, restart...)
	})

	fmt.Println("watching for changes (Ctrl-C stops watching; containers keep running, `azemu down` stops them)")
	err := watch.Run(ctx, targets, rebuild)
	fmt.Println("\nstopped watching; containers are still running. `azemu down` stops them.")
	return err
}

// watchSettings polls the secrets file and azemu.yaml; when apps' resolved settings change it calls apply with the
// changed ones, holding mu. apply records them in envs and restarts what runs. Returns when ctx is done.
// ponytail: poll mtimes every 2s instead of a second fsnotify watcher on single files.
func watchSettings(ctx context.Context, o Options, cfg *config.Config, m *infra.Model, env, header string,
	envs map[string]map[string]string, mu *sync.Mutex, apply func(ctx context.Context, changed map[string]map[string]string) error) {
	files := []string{cfg.Abs(config.Expand(cfg.Secrets, env)), yamlPath(o.File)}
	stamp := func() (s []time.Time) {
		for _, f := range files {
			fi, err := os.Stat(f)
			if err == nil {
				s = append(s, fi.ModTime())
			} else {
				s = append(s, time.Time{})
			}
		}
		return
	}
	last := stamp()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		cur := stamp()
		if slices.Equal(cur, last) {
			continue
		}
		last = cur
		newCfg, _, err := load(o.File, o.Env, o.Apps)
		if err == nil {
			var e map[string]map[string]string
			if e, err = resolveAll(newCfg, m, env, header, o.KeepTelemetry); err == nil {
				mu.Lock()
				changed := map[string]map[string]string{}
				for k, v := range e {
					if !maps.Equal(v, envs[k]) {
						changed[k] = v
					}
				}
				if len(changed) > 0 {
					err = apply(ctx, changed)
				}
				mu.Unlock()
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "[watch] settings: %v\n", err)
		}
	}
}
