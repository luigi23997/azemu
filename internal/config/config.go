// Package config loads and saves azemu.yaml, the reviewed contract between code and infra.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const FileName = "azemu.yaml"

type Config struct {
	Env      string          `yaml:"env"`
	Infra    Infra           `yaml:"infra"`
	Secrets  string          `yaml:"secrets,omitempty"` // path to .env, may contain {env}; default ".env"
	Identity Identity        `yaml:"identity,omitempty"`
	Graph    *Graph          `yaml:"graph,omitempty"` // present = apps calling Microsoft Graph run with your own delegated login
	Apps     map[string]*App `yaml:"apps"`            // key = local app name (folder basename), stable across envs
	Seed     []Seed          `yaml:"seed,omitempty"`

	Root string `yaml:"-"` // directory containing azemu.yaml; all relative paths resolve against it
}

type Infra struct {
	Dir           string   `yaml:"dir"`                      // terraform working dir, relative to Root
	VarFiles      []string `yaml:"var_files,omitempty"`      // relative to Dir, may contain {env}
	BackendConfig []string `yaml:"backend_config,omitempty"` // relative to Dir, may contain {env}
	// PlanFile, when set, skips running plan: a `terraform show -json` output (.json) or a saved
	// plan binary (e.g. the infra pipeline's tfplan.bin artifact). Relative to Root, may contain {env}.
	PlanFile string `yaml:"plan_file,omitempty"`
	// Mode: "static" (default) evaluates the .tf/.tfvars files locally with no Azure access;
	// "plan" runs `terraform plan` (needs state + subscription read access). PlanFile implies plan.
	Mode string `yaml:"mode,omitempty"`
}

type Identity struct {
	As string `yaml:"as,omitempty"` // Easy Auth principal name; default: git config user.email
}

// Graph configures `azemu graph login`; every field has a default, so `graph: {}` is enough.
type Graph struct {
	Tenant   string   `yaml:"tenant,omitempty"`    // default "organizations"
	ClientID string   `yaml:"client_id,omitempty"` // public client used to sign in; default Microsoft Graph Command Line Tools
	Scopes   []string `yaml:"scopes,omitempty"`    // delegated scopes; default Sites.Read.All, Files.Read.All
	Writable bool     `yaml:"writable,omitempty"`  // default false: the proxy rejects writes to Graph
}

type App struct {
	Path       string            `yaml:"path"`                 // build context, relative to Root
	Resource   map[string]string `yaml:"resource"`             // env -> Azure resource name
	Kind       string            `yaml:"kind"`                 // "zip" | "docker"
	Runtime    string            `yaml:"runtime,omitempty"`    // "python:3.12", "node:20" (zip apps)
	Dockerfile string            `yaml:"dockerfile,omitempty"` // docker apps, relative to Path
	Port       int               `yaml:"port,omitempty"`       // container port; default 80
	Platform   string            `yaml:"platform,omitempty"`   // e.g. linux/amd64; default native
	Env        map[string]string `yaml:"env,omitempty"`        // final overrides
	// Run starts the app in native mode (`up --native`, no containers), from Path, through the shell; {port} is the
	// port to listen on (also in $PORT). Required for docker apps there; zip apps default to `func start --port {port}`.
	Run string `yaml:"run,omitempty"`
}

type Seed struct {
	Image string `yaml:"image"`
	Run   string `yaml:"run"`
}

// Load reads azemu.yaml from dir (or the file itself if path ends in .yaml).
func Load(path string) (*Config, error) {
	if !strings.HasSuffix(path, ".yaml") {
		path = filepath.Join(path, FileName)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	c.Root = abs
	if c.Secrets == "" {
		c.Secrets = ".env"
	}
	return &c, nil
}

func (c *Config) Save(path string) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// Expand replaces {env} in s.
func Expand(s, env string) string { return strings.ReplaceAll(s, "{env}", env) }

// Abs resolves p against Root.
func (c *Config) Abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Root, p)
}

// StatePath returns a path under <Root>/.azemu.
func (c *Config) StatePath(elem ...string) string {
	return filepath.Join(append([]string{c.Root, ".azemu"}, elem...)...)
}

// ResourceName returns the Azure resource name of app key for env, or "".
func (c *Config) ResourceName(key, env string) string {
	if a, ok := c.Apps[key]; ok {
		return a.Resource[env]
	}
	return ""
}
