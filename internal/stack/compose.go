// Package stack generates the docker compose project and drives it.
package stack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/luigi23997/azemu/internal/build"
	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/host"
	"github.com/luigi23997/azemu/internal/infra"
)

var unsafeName = regexp.MustCompile(`[^a-z0-9_-]+`)

func slug(cfg *config.Config) string {
	s := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(filepath.Base(cfg.Root)), "-"), "-_")
	if s == "" {
		return "project"
	}
	return s
}

// ProjectName is the compose project name: "azemu-" + sanitized basename of the project root.
func ProjectName(cfg *config.Config) string { return "azemu-" + slug(cfg) }

type composeFile struct {
	Services map[string]*service `yaml:"services"`
	Volumes  map[string]struct{} `yaml:"volumes"`
	Configs  map[string]inline   `yaml:"configs,omitempty"`
}

type inline struct {
	Content string `yaml:"content"`
}

// dozzleImage is pinned: the newest stable release at least two weeks old, with linux/amd64 and linux/arm64 manifests.
const dozzleImage = "amir20/dozzle:v11.1.1"

// dozzleProfile is Dozzle's default UI profile: keep stopped containers listed so a crashed app's last logs stay visible.
const dozzleProfile = `{"settings":{"showAllContainers":true}}`

// name is the label Dozzle shows instead of the compose container name (<project>-<service>-1).
func name(n string) map[string]string { return map[string]string{"dev.dozzle.name": n} }

type service struct {
	Image       string              `yaml:"image"`
	Build       *buildSpec          `yaml:"build,omitempty"`
	Platform    string              `yaml:"platform,omitempty"`
	Command     []string            `yaml:"command,omitempty"`
	Environment map[string]string   `yaml:"environment,omitempty"`
	EnvFile     []envFile           `yaml:"env_file,omitempty"`
	Volumes     []string            `yaml:"volumes,omitempty"`
	Ports       []string            `yaml:"ports,omitempty"`
	Networks    map[string]netAlias `yaml:"networks,omitempty"`
	DependsOn   map[string]dep      `yaml:"depends_on,omitempty"`
	Healthcheck *healthcheck        `yaml:"healthcheck,omitempty"`
	Restart     string              `yaml:"restart,omitempty"`
	Labels      map[string]string   `yaml:"labels,omitempty"`
	Configs     []fileConfig        `yaml:"configs,omitempty"`
}

type fileConfig struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

type buildSpec struct {
	Context    string `yaml:"context"`
	Dockerfile string `yaml:"dockerfile"`
}
type envFile struct {
	Path   string `yaml:"path"`
	Format string `yaml:"format"`
}
type netAlias struct {
	Aliases []string `yaml:"aliases"`
}
type dep struct {
	Condition string `yaml:"condition"`
}
type healthcheck struct {
	Test     []string `yaml:"test"`
	Interval string   `yaml:"interval"`
	Timeout  string   `yaml:"timeout"`
	Retries  int      `yaml:"retries"`
}

// Compose renders the compose project: proxy, azurite, the Dozzle log dashboard and one service per app.
// It calls build.Prepare for every app, which may write generated Dockerfiles.
func Compose(cfg *config.Config, m *infra.Model, env string, pc *contract.ProxyConfig, proxyImage string, logsPort int) ([]byte, error) {
	certs := contract.CertsVolume + ":" + contract.CertsDir + ":ro"
	healthy := dep{"service_healthy"}

	proxy := &service{
		Image:   proxyImage,
		Command: []string{"proxy", "--config", contract.ProxyConfigPath},
		// proxy.json is bind-mounted and read once at startup; a hash in the env makes compose recreate the proxy when it changes.
		Environment: map[string]string{"AZEMU_CONFIG_HASH": configHash(pc)},
		Labels:      name("proxy"),
		Volumes: []string{
			cfg.StatePath("proxy.json") + ":" + contract.ProxyConfigPath + ":ro",
			contract.HostCADir() + ":" + contract.ProxyCADir,
			contract.CertsVolume + ":" + contract.CertsDir,
		},
		Healthcheck: &healthcheck{
			Test:     []string{"CMD", "wget", "-qO-", "http://localhost:" + strconv.Itoa(contract.IdentityPort) + contract.ProxyHealthPath},
			Interval: "2s", Timeout: "3s", Retries: 30,
		},
	}
	for _, in := range pc.Ingress {
		proxy.Ports = append(proxy.Ports, fmt.Sprintf("127.0.0.1:%d:%d", in.Port, in.Port))
	}
	aliases := make([]string, 0, len(pc.Routes)+2)
	for h := range pc.Routes {
		aliases = append(aliases, h)
	}
	if pc.Graph != nil {
		aliases = append(aliases, contract.GraphHost, contract.LoginHost)
		proxy.Volumes = append(proxy.Volumes, cfg.StatePath("graph")+":"+contract.GraphDir)
	}
	if len(aliases) > 0 {
		sort.Strings(aliases)
		proxy.Networks = map[string]netAlias{"default": {aliases}}
	}

	azurite := &service{
		Image: "mcr.microsoft.com/azure-storage/azurite",
		Command: []string{"azurite", "--blobHost", "0.0.0.0", "--queueHost", "0.0.0.0", "--tableHost", "0.0.0.0",
			"--oauth", "basic", "--cert", contract.AzuriteCert, "--key", contract.AzuriteKeyPEM,
			"--skipApiVersionCheck", "--loose", "--location", "/data"},
		Environment: map[string]string{"AZURITE_ACCOUNTS": AzuriteAccounts(m)},
		Volumes:     []string{certs, "azurite-data:/data"},
		Labels:      name("azurite"),
		Ports: []string{
			fmt.Sprintf("127.0.0.1:%d:%d", contract.AzuriteBlobPort, contract.AzuriteBlobPort),
			fmt.Sprintf("127.0.0.1:%d:%d", contract.AzuriteQueuePort, contract.AzuriteQueuePort),
			fmt.Sprintf("127.0.0.1:%d:%d", contract.AzuriteTablePort, contract.AzuriteTablePort),
		},
		DependsOn: map[string]dep{contract.ProxyService: healthy},
		Healthcheck: &healthcheck{ // node-based image: no wget/nc
			Test: []string{"CMD", "node", "-e", fmt.Sprintf(
				"require('net').connect(%d,'127.0.0.1').on('connect',()=>process.exit(0)).on('error',()=>process.exit(1))", contract.AzuriteBlobPort)},
			Interval: "2s", Timeout: "3s", Retries: 30,
		},
	}

	// Read-only log viewer for this project's containers only. No depends_on: it starts first and shows startup and crash logs.
	// Actions and shell stay off (Dozzle's default) and the socket is mounted read-only.
	logs := &service{
		Image: dozzleImage,
		Environment: map[string]string{
			"DOZZLE_FILTER":             "label=com.docker.compose.project=" + ProjectName(cfg),
			"DOZZLE_HOSTNAME":           ProjectName(cfg),
			"DOZZLE_NO_ANALYTICS":       "true",
			"DOZZLE_RELEASE_CHECK_MODE": "manual",
		},
		Volumes: []string{"/var/run/docker.sock:/var/run/docker.sock:ro"},
		Ports:   []string{fmt.Sprintf("127.0.0.1:%d:8080", logsPort)},
		Labels:  name("logs"),
		Configs: []fileConfig{{Source: "dozzle-profile", Target: "/data/__default__/profile.json"}},
	}

	cf := composeFile{
		Services: map[string]*service{contract.ProxyService: proxy, contract.AzuriteService: azurite, contract.LogsService: logs},
		Volumes:  map[string]struct{}{contract.CertsVolume: {}, "azurite-data": {}},
		Configs:  map[string]inline{"dozzle-profile": {dozzleProfile}},
	}
	for key, app := range cfg.Apps {
		ctxDir, dockerfile, err := build.Prepare(cfg, key)
		if err != nil {
			return nil, err
		}
		platform := app.Platform
		if platform == "" && app.Kind == "zip" {
			platform = "linux/amd64" // the Azure Functions base images have no arm64 manifest
		}
		cf.Services[key] = &service{
			Image:     "azemu/" + slug(cfg) + "-" + key + ":dev",
			Build:     &buildSpec{Context: ctxDir, Dockerfile: dockerfile},
			Platform:  platform,
			EnvFile:   []envFile{{Path: cfg.StatePath("env", key+".env"), Format: "raw"}},
			Volumes:   []string{certs},
			DependsOn: map[string]dep{contract.ProxyService: healthy, contract.AzuriteService: healthy},
			Restart:   "no",
			Labels:    name(key),
		}
	}
	return yaml.Marshal(cf) // yaml.v3 sorts map keys, so output is deterministic
}

func configHash(pc *contract.ProxyConfig) string {
	b, _ := json.Marshal(pc)
	return fmt.Sprintf("%x", sha256.Sum256(b))[:16]
}

// AzuriteAccounts builds AZURITE_ACCOUNTS for every storage account plus every app's host storage.
func AzuriteAccounts(m *infra.Model) string {
	set := map[string]bool{}
	for _, a := range m.StorageAccounts {
		set[a.Name] = true
	}
	for _, a := range m.Apps {
		if a.HostStorage != "" {
			set[a.HostStorage] = true
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n+":"+contract.AzuriteKey)
	}
	sort.Strings(names)
	return strings.Join(names, ";")
}

// WriteCompose renders the project and writes <StateDir>/compose.yaml, skipping the write when unchanged.
func WriteCompose(cfg *config.Config, m *infra.Model, env string, pc *contract.ProxyConfig, proxyImage string, logsPort int) error {
	b, err := Compose(cfg, m, env, pc, proxyImage, logsPort)
	if err != nil {
		return err
	}
	return writeIfChanged(cfg.StatePath("compose.yaml"), b)
}

// WriteBuildCompose writes <StateDir>/compose.build.yaml: the apps with an empty model and proxy config.
// Builds need neither, so they can start before the terraform plan finishes.
func WriteBuildCompose(cfg *config.Config, env, proxyImage string, logsPort int) error {
	b, err := Compose(cfg, &infra.Model{}, env, &contract.ProxyConfig{}, proxyImage, logsPort)
	if err != nil {
		return err
	}
	return writeIfChanged(cfg.StatePath("compose.build.yaml"), b)
}

func writeIfChanged(p string, b []byte) error {
	if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, b) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// BuildService builds one service, capturing compose output in out. preplan selects compose.build.yaml
// (usable before the plan) instead of compose.yaml. Concurrent calls for different services are safe.
func BuildService(ctx context.Context, cfg *config.Config, preplan bool, out io.Writer, service string) error {
	file := "compose.yaml"
	if preplan {
		file = "compose.build.yaml"
	}
	return composeIn(ctx, cfg, file, out, "build", service)
}

// compose runs `docker compose` for the project, writing stdout/stderr to out.
func compose(ctx context.Context, cfg *config.Config, out io.Writer, args ...string) error {
	return composeIn(ctx, cfg, "compose.yaml", out, args...)
}

func composeIn(ctx context.Context, cfg *config.Config, file string, out io.Writer, args ...string) error {
	full := append([]string{"compose", "--progress", "plain", "-p", ProjectName(cfg), "-f", cfg.StatePath(file)}, args...)
	cmd := host.CommandContext(ctx, "docker", full...)
	cmd.Dir = cfg.Root
	cmd.Stdout, cmd.Stderr = out, out
	if err := host.Run(cmd); err != nil {
		return fmt.Errorf("docker compose %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func prefixed(w io.Writer) io.Writer { return &prefixWriter{w: w} }

// prefixWriter prepends "[compose] " to every line; safe for concurrent writers.
type prefixWriter struct {
	mu   sync.Mutex
	w    io.Writer
	part []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.part = append(p.part, b...)
	for {
		i := bytes.IndexByte(p.part, '\n')
		if i < 0 {
			return len(b), nil
		}
		if _, err := fmt.Fprintf(p.w, "[compose] %s\n", p.part[:i]); err != nil {
			return 0, err
		}
		p.part = p.part[i+1:]
	}
}

// Build builds the given services (all when none given).
func Build(ctx context.Context, cfg *config.Config, services ...string) error {
	return compose(ctx, cfg, prefixed(os.Stderr), append([]string{"build"}, services...)...)
}

// Up starts services (all when none given), waiting for healthy. Images must already be built.
func Up(ctx context.Context, cfg *config.Config, services ...string) error {
	return quiet(ctx, cfg, append([]string{"up", "-d", "--wait", "--no-build"}, services...)...)
}

// UpLogs starts the log dashboard from compose.build.yaml (written before the plan), without waiting: it has no dependencies.
func UpLogs(ctx context.Context, cfg *config.Config) error {
	var buf bytes.Buffer
	err := composeIn(ctx, cfg, "compose.build.yaml", &buf, "up", "-d", "--no-build", contract.LogsService)
	if err != nil {
		prefixed(os.Stderr).Write(buf.Bytes())
	}
	return err
}

// UpNoDeps is Up for an app-only refresh: dependencies (proxy, azurite) are left alone.
func UpNoDeps(ctx context.Context, cfg *config.Config, services ...string) error {
	return quiet(ctx, cfg, append([]string{"up", "-d", "--wait", "--no-build", "--no-deps"}, services...)...)
}

// Down stops and removes the stack; volumes also wipes Azurite data and the certs volume.
func Down(ctx context.Context, cfg *config.Config, volumes bool) error {
	args := []string{"down", "--remove-orphans"}
	if volumes {
		args = append(args, "--volumes")
	}
	return quiet(ctx, cfg, args...)
}

// quiet runs compose and shows its output only when it fails (the per-container progress lines are noise).
func quiet(ctx context.Context, cfg *config.Config, args ...string) error {
	var buf bytes.Buffer
	err := compose(ctx, cfg, &buf, args...)
	if err != nil {
		prefixed(os.Stderr).Write(buf.Bytes())
	}
	return err
}

// Logs streams service logs to stdout unprefixed (compose already prefixes each line with the service name).
func Logs(ctx context.Context, cfg *config.Config, follow bool, services ...string) error {
	args := []string{"logs"}
	if follow {
		args = append(args, "--follow")
	}
	return compose(ctx, cfg, os.Stdout, append(args, services...)...)
}

// Container is the subset of `docker compose ps --format json` azemu shows.
type Container struct {
	Service  string `json:"Service"`
	State    string `json:"State"`  // running, exited, ...
	Health   string `json:"Health"` // healthy, unhealthy, starting, ""
	Status   string `json:"Status"`
	ExitCode int    `json:"ExitCode"`
}

// PS lists the project's containers, including stopped ones.
func PS(ctx context.Context, cfg *config.Config) ([]Container, error) {
	var out bytes.Buffer
	if err := compose(ctx, cfg, &out, "ps", "--all", "--format", "json"); err != nil {
		return nil, err
	}
	return parsePS(out.Bytes())
}

// parsePS accepts both compose output shapes: one JSON array (older) or one JSON object per line (newer).
func parsePS(b []byte) ([]Container, error) {
	b = bytes.TrimSpace(b)
	var cs []Container
	if len(b) > 0 && b[0] == '[' {
		return cs, json.Unmarshal(b, &cs)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	for dec.More() {
		var c Container
		if err := dec.Decode(&c); err != nil {
			return nil, err
		}
		cs = append(cs, c)
	}
	return cs, nil
}
