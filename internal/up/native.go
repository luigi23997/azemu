package up

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luigi23997/azemu/internal/build"
	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/host"
	"github.com/luigi23997/azemu/internal/infra"
	"github.com/luigi23997/azemu/internal/proxy"
	"github.com/luigi23997/azemu/internal/stack"
	"github.com/luigi23997/azemu/internal/watch"
)

// azuritePkg is run with npx; pinned so every machine emulates the same storage.
const azuritePkg = "azurite@3.34.0"

// Extra keys in ports.json for native mode, next to the app keys (their ingress ports).
const (
	proxyPortKey    = "_proxy"    // HTTPS_PROXY of every app
	identityPortKey = "_identity" // IDENTITY_ENDPOINT
)

// listenKey is the ports.json key of the port app k itself listens on (its ingress port fronts it).
func listenKey(k string) string { return "_listen:" + k }

// RunNative is `azemu up --native`: everything as plain processes on this machine, no containers (e.g. an Azure
// Virtual Desktop without nested virtualization). The proxy runs inside azemu and apps reach it as their HTTPS_PROXY;
// Azurite runs through npx; zip apps run with `func start` (Azure Functions Core Tools), docker apps with their `run:`.
// Everything stops with azemu (Ctrl-C).
func RunNative(ctx context.Context, o Options) error {
	t0 := time.Now()
	cfg, env, err := load(o.File, o.Env, o.Apps)
	if err != nil {
		return err
	}
	if len(cfg.Apps) == 0 {
		return errors.New("no apps in azemu.yaml")
	}
	keys := slices.Sorted(maps.Keys(cfg.Apps))
	for _, k := range keys {
		if a := cfg.Apps[k]; a.Kind == "docker" && a.Run == "" {
			return fmt.Errorf("app %s: a docker app needs `run:` in azemu.yaml for --native: the command that starts it, listening on {port}", k)
		}
	}
	header, err := identityHeader(cfg)
	if err != nil {
		return err
	}
	portKeys := append(slices.Clone(keys), proxyPortKey, identityPortKey)
	for _, k := range keys {
		portKeys = append(portKeys, listenKey(k))
	}
	ports, err := assignPorts(cfg.StatePath("ports.json"), portKeys)
	if err != nil {
		return err
	}

	m, err := infra.Load(ctx, cfg, env, o.Refresh)
	if err != nil {
		return err
	}
	for _, w := range m.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	warnMismatch(cfg, m, env)
	if err := prepareGraph(cfg); err != nil {
		return err
	}
	local := func(k string) string { return fmt.Sprintf("http://127.0.0.1:%d", ports[listenKey(k)]) }
	pc := proxyConfig(cfg, m, env, ports, header, principalFor(cfg, o.As), local, "127.0.0.1")
	envs, err := resolveAll(cfg, m, env, header, o.KeepTelemetry)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var outMu sync.Mutex
	logTo := func(name string) io.Writer { return &prefixer{mu: &outMu, w: os.Stdout, name: name} }
	log.SetOutput(logTo("proxy"))
	log.SetFlags(0)

	certs := cfg.StatePath("certs")
	proxyErr, err := proxy.Native(ctx, *pc, proxy.NativeOptions{
		CADir: contract.HostCADir(), CertsDir: certs, GraphDir: cfg.StatePath("graph"),
		ProxyPort: ports[proxyPortKey], IdentityPort: ports[identityPortKey],
	})
	if err != nil {
		return err
	}
	go func() {
		if err := <-proxyErr; err != nil {
			fmt.Fprintln(os.Stderr, "azemu: proxy stopped:", err)
			cancel()
		}
	}()
	if ok, hint := Trusted(); !ok {
		fmt.Fprintln(os.Stderr, "warning: the azemu CA is not trusted on this machine, so the Functions host cannot reach emulated storage: "+hint)
	}
	caPEM, err := os.ReadFile(filepath.Join(certs, "ca.pem"))
	if err != nil {
		return err
	}
	az, err := startAzurite(ctx, cfg, m, certs, logTo("azurite"))
	if err != nil {
		return err
	}
	defer az.stop()
	if err := stack.CreateObjects(ctx, m, caPEM); err != nil {
		return err
	}
	if len(cfg.Seed) > 0 {
		fmt.Fprintln(os.Stderr, "warning: seed steps run in containers and are skipped with --native")
	}

	n := &native{cfg: cfg, envs: envs, ports: ports, certs: certs, logTo: logTo, procs: map[string]*proc{}, launches: map[string]*launch{}}
	defer n.stopAll()
	results := n.prepareAll(ctx, keys)
	n.mu.Lock()
	for _, k := range keys {
		if results[k].err == nil {
			if err := n.start(ctx, k, n.launches[k]); err != nil {
				results[k].err = err
				fmt.Printf("[%s] failed to start: %v\n", k, err)
			}
		}
	}
	n.mu.Unlock()
	states := map[string]string{}
	for _, k := range keys {
		if results[k].err == nil {
			states[k] = "running"
		}
	}
	printTable(cfg, env, ports, states, results)
	fmt.Printf("timings: total %.1fs\n", time.Since(t0).Seconds())

	if o.NoWatch {
		fmt.Println("running without watching (Ctrl-C stops everything)")
		<-ctx.Done()
		return nil
	}
	return n.watch(ctx, o, m, env, header)
}

// native holds the running app processes. mu guards procs, launches and envs.
type native struct {
	cfg      *config.Config
	ports    map[string]int
	certs    string
	logTo    func(name string) io.Writer
	mu       sync.Mutex
	envs     map[string]map[string]string
	procs    map[string]*proc
	launches map[string]*launch
}

// launch is how to start one app once its dependencies are in place.
type launch struct {
	dir   string
	argv  []string
	shell string            // run: command line, when set
	env   map[string]string // on top of the app settings (virtualenv)
}

// prepareAll installs every app's dependencies concurrently, recording each launch; one failure spares the others.
func (n *native) prepareAll(ctx context.Context, keys []string) map[string]*buildResult {
	res := map[string]*buildResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			l, err := n.prepare(ctx, k)
			r := &buildResult{err: err, dur: time.Since(start)}
			if err != nil {
				fmt.Printf("[%s] prepare FAILED after %.1fs: %v\n", k, r.dur.Seconds(), err)
			}
			mu.Lock()
			res[k] = r
			mu.Unlock()
			if err == nil {
				n.mu.Lock()
				n.launches[k] = l
				n.mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return res
}

// prepare installs what app k needs to run (when its inputs changed) and returns how to start it.
func (n *native) prepare(ctx context.Context, k string) (*launch, error) {
	app := n.cfg.Apps[k]
	port := strconv.Itoa(n.ports[listenKey(k)])
	l := &launch{dir: n.cfg.Abs(app.Path), argv: []string{"func", "start", "--port", port}}
	if app.Kind == "zip" {
		name, ver, _ := strings.Cut(app.Runtime, ":")
		switch name {
		case "python":
			env, err := pythonDeps(ctx, n.cfg.StatePath("venv", k), l.dir, ver)
			if err != nil {
				return nil, err
			}
			l.env = env
		case "node":
			if err := nodeDeps(ctx, n.cfg.StatePath("deps", k), l.dir); err != nil {
				return nil, err
			}
			if build.HasBuildScript(l.dir) {
				if err := step(ctx, l.dir, nil, "npm", "run", "build"); err != nil {
					return nil, err
				}
			}
		default:
			return nil, fmt.Errorf("runtime %q is not supported natively (python, node)", app.Runtime)
		}
	}
	if app.Run != "" {
		l.shell = strings.ReplaceAll(app.Run, "{port}", port)
	}
	return l, nil
}

// start runs app k (n.mu held), replacing nothing: stop the old process first.
func (n *native) start(ctx context.Context, k string, l *launch) error {
	env := nativeEnv(n.envs[k], l.env, n.certs, n.ports, k)
	p, err := startProc(l.dir, append(os.Environ(), env...), n.logTo(k), l.shell, l.argv...)
	if err != nil {
		return err
	}
	n.procs[k] = p
	go func() {
		<-p.done
		if !p.stopping.Load() && ctx.Err() == nil {
			fmt.Printf("[%s] exited (%s); it starts again on the next change\n", k, host.ExitState(p.cmd))
		}
	}()
	return nil
}

// restart stops app k's process and starts it again (n.mu held).
func (n *native) restart(ctx context.Context, k string) error {
	if p := n.procs[k]; p != nil {
		p.stop()
	}
	return n.start(ctx, k, n.launches[k])
}

func (n *native) stopAll() {
	n.mu.Lock()
	defer n.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range n.procs {
		wg.Add(1)
		go func() { defer wg.Done(); p.stop() }()
	}
	wg.Wait()
}

func (n *native) watch(ctx context.Context, o Options, m *infra.Model, env, header string) error {
	var targets []watch.Target
	for _, k := range slices.Sorted(maps.Keys(n.cfg.Apps)) {
		dir, df, err := build.Prepare(n.cfg, k) // only for its dockerignore, which filters events
		if err != nil {
			return err
		}
		targets = append(targets, watch.Target{Key: k, Dir: dir, Dockerfile: df, Ignore: buildOutputs(n.cfg.Apps[k], dir)})
	}
	rebuild := func(ctx context.Context, keys []string) error {
		fmt.Printf("[watch] rebuilding %s\n", strings.Join(keys, ", "))
		results := n.prepareAll(ctx, keys)
		n.mu.Lock()
		defer n.mu.Unlock()
		var ok []string
		for _, k := range keys {
			if results[k].err != nil {
				fmt.Printf("[watch] %s: prepare failed, the previous process keeps running\n", k)
				continue
			}
			if err := n.restart(ctx, k); err != nil {
				fmt.Printf("[watch] %s: %v\n", k, err)
				continue
			}
			ok = append(ok, k)
		}
		if len(ok) > 0 {
			fmt.Printf("[watch] %s restarted\n", strings.Join(ok, ", "))
		}
		return nil
	}
	go watchSettings(ctx, o, n.cfg, m, env, header, n.envs, &n.mu, func(ctx context.Context, changed map[string]map[string]string) error {
		var restarted []string
		for _, k := range slices.Sorted(maps.Keys(changed)) {
			n.envs[k] = changed[k]
			if n.procs[k] != nil {
				if err := n.restart(ctx, k); err != nil {
					return err
				}
				restarted = append(restarted, k)
			}
		}
		if len(restarted) > 0 {
			fmt.Printf("[watch] settings changed, restarted %s\n", strings.Join(restarted, ", "))
		}
		return nil
	})
	fmt.Println("watching for changes (Ctrl-C stops everything)")
	err := watch.Run(ctx, targets, rebuild)
	fmt.Println("\nstopping")
	return err
}

// nativeEnv is what app k gets on top of the user's own environment: its resolved settings, with the parts that
// assume a container pointed at this machine, plus the proxy variables that replace the network's DNS aliases.
func nativeEnv(settings, extra map[string]string, certs string, ports map[string]int, k string) []string {
	e := maps.Clone(settings)
	delete(e, "AzureWebJobsScriptRoot") // a container path; func start uses its working directory
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", ports[proxyPortKey])
	for key, v := range map[string]string{
		"IDENTITY_ENDPOINT":   fmt.Sprintf("http://127.0.0.1:%d%s", ports[identityPortKey], contract.IdentityPath),
		"REQUESTS_CA_BUNDLE":  filepath.Join(certs, "bundle.pem"),
		"SSL_CERT_FILE":       filepath.Join(certs, "bundle.pem"),
		"CURL_CA_BUNDLE":      filepath.Join(certs, "bundle.pem"),
		"NODE_EXTRA_CA_CERTS": filepath.Join(certs, "ca.pem"),
		"HTTPS_PROXY":         proxyURL, "https_proxy": proxyURL,
		"HTTP_PROXY": proxyURL, "http_proxy": proxyURL,
		"NO_PROXY": "localhost,127.0.0.1", "no_proxy": "localhost,127.0.0.1",
		"NODE_USE_ENV_PROXY": "1", // Node 24+: fetch and https honour HTTPS_PROXY
		"PORT":               strconv.Itoa(ports[listenKey(k)]),
	} {
		e[key] = v
	}
	for key, v := range extra {
		e[key] = v
	}
	out := make([]string, 0, len(e))
	for _, key := range slices.Sorted(maps.Keys(e)) {
		out = append(out, key+"="+e[key])
	}
	return out
}

// buildOutputs are dirs a native build writes into the app folder; watching them would rebuild forever.
func buildOutputs(app *config.App, dir string) []string {
	if app.Kind != "zip" || !strings.HasPrefix(app.Runtime, "node") || !build.HasBuildScript(dir) {
		return nil
	}
	out := []string{"dist"}
	if b, err := os.ReadFile(filepath.Join(dir, "tsconfig.json")); err == nil {
		if m := outDirRe.FindSubmatch(b); m != nil {
			out = append(out, strings.TrimPrefix(string(m[1]), "./"))
		}
	}
	return out
}

// ponytail: regex instead of a JSONC parser; tsconfig allows comments and trailing commas.
var outDirRe = regexp.MustCompile(`"outDir"\s*:\s*"([^"]+)"`)

// ---- dependencies ----

// step runs one setup command, showing its output only when it fails.
func step(ctx context.Context, dir string, env []string, argv ...string) error {
	cmd := host.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := host.Run(cmd); err != nil {
		return fmt.Errorf("%s: %w\n%s", strings.Join(argv, " "), err, tail(out.String(), 25))
	}
	return nil
}

// changed reports whether files differ from what marker recorded; save records them.
func changed(marker string, files ...string) (bool, func() error) {
	h := sha256.New()
	for _, f := range files {
		b, _ := os.ReadFile(f)
		fmt.Fprintf(h, "%s %d\n", filepath.Base(f), len(b))
		h.Write(b)
	}
	sum := fmt.Sprintf("%x", h.Sum(nil))
	old, _ := os.ReadFile(marker)
	return string(old) != sum, func() error {
		if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
			return err
		}
		return os.WriteFile(marker, []byte(sum), 0o644)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// pythonDeps keeps a virtualenv per app under .azemu/venv and returns the env that activates it.
func pythonDeps(ctx context.Context, venv, dir, ver string) (map[string]string, error) {
	bin := filepath.Join(venv, "bin")
	pathSep := ":"
	if host.GOOS() == "windows" {
		bin = filepath.Join(venv, "Scripts")
		pathSep = ";"
	}
	py := filepath.Join(bin, "python")
	if !exists(py) && !exists(py+".exe") {
		base, err := findPython(ctx, ver)
		if err != nil {
			return nil, err
		}
		if err := step(ctx, dir, nil, append(base, "-m", "venv", venv)...); err != nil {
			return nil, err
		}
	}
	env := map[string]string{"VIRTUAL_ENV": venv, "PATH": bin + pathSep + os.Getenv("PATH")}

	req, inputs := filepath.Join(dir, "requirements.txt"), []string{filepath.Join(dir, "requirements.txt")}
	uv := !exists(req) && exists(filepath.Join(dir, "uv.lock"))
	if uv {
		req, inputs = filepath.Join(venv, "requirements.txt"), []string{filepath.Join(dir, "pyproject.toml"), filepath.Join(dir, "uv.lock")}
	} else if !exists(req) {
		return env, nil
	}
	dirty, save := changed(filepath.Join(venv, ".azemu-deps"), inputs...)
	if !dirty {
		return env, nil
	}
	if uv {
		if err := step(ctx, dir, nil, "uv", "export", "--frozen", "--no-dev", "--no-emit-project", "--no-hashes", "-o", req); err != nil {
			return nil, fmt.Errorf("uv.lock project (install uv, or add a requirements.txt): %w", err)
		}
	}
	if err := step(ctx, dir, nil, py, "-m", "pip", "install", "--disable-pip-version-check", "-r", req); err != nil {
		return nil, err
	}
	return env, save()
}

// findPython returns the command of a Python for version ver (e.g. "3.12"); another version only warns.
func findPython(ctx context.Context, ver string) ([]string, error) {
	cands := [][]string{{"python" + ver}, {"python3"}, {"python"}}
	if host.GOOS() == "windows" {
		cands = [][]string{{"py", "-" + ver}, {"python"}, {"py"}}
	}
	for _, c := range cands {
		out, err := host.Output(host.CommandContext(ctx, c[0], append(c[1:], "-c", "import sys;print('%d.%d'%sys.version_info[:2])")...))
		if err != nil {
			continue
		}
		if got := strings.TrimSpace(string(out)); got != ver {
			fmt.Fprintf(os.Stderr, "warning: Python %s not found, using %s (%s)\n", ver, got, strings.Join(c, " "))
		}
		return c, nil
	}
	return nil, fmt.Errorf("no Python found (want %s): install it and retry", ver)
}

// nodeDeps runs npm ci (npm install without a lockfile) when package.json or the lockfile changed.
func nodeDeps(ctx context.Context, marker, dir string) error {
	lock := filepath.Join(dir, "package-lock.json")
	dirty, save := changed(marker, filepath.Join(dir, "package.json"), lock)
	if !dirty && exists(filepath.Join(dir, "node_modules")) {
		return nil
	}
	cmd := []string{"npm", "ci"}
	if !exists(lock) {
		cmd = []string{"npm", "install"}
	}
	if err := step(ctx, dir, nil, cmd...); err != nil {
		return err
	}
	return save()
}

// ---- azurite ----

func startAzurite(ctx context.Context, cfg *config.Config, m *infra.Model, certs string, out io.Writer) (*proc, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", contract.AzuriteBlobPort)
	if c, err := host.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		return nil, fmt.Errorf("%s is already in use: another Azurite, or the docker stack (`azemu down`)", addr)
	}
	p, err := startProc(cfg.Root, append(os.Environ(), "AZURITE_ACCOUNTS="+stack.AzuriteAccounts(m)), out, "",
		"npx", "--yes", azuritePkg,
		"--blobHost", "127.0.0.1", "--queueHost", "127.0.0.1", "--tableHost", "127.0.0.1",
		"--oauth", "basic", "--cert", filepath.Join(certs, "azurite.pem"), "--key", filepath.Join(certs, "azurite-key.pem"),
		"--skipApiVersionCheck", "--loose", "--location", cfg.StatePath("azurite"))
	if err != nil {
		return nil, fmt.Errorf("azurite (needs Node.js and npx): %w", err)
	}
	deadline := time.Now().Add(3 * time.Minute) // the first run downloads it
	for {
		if c, err := host.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return p, nil
		}
		select {
		case <-p.done:
			return nil, errors.New("azurite exited; see the [azurite] lines above")
		case <-ctx.Done():
			p.stop()
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			p.stop()
			return nil, errors.New("azurite did not start within 3 minutes")
		}
	}
}
