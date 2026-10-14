package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/host"
	"github.com/luigi23997/azemu/internal/up"
)

type check struct {
	name     string
	required bool // false = failure only warns
	hint     string
	run      func(ctx context.Context) (string, error)
}

// out runs a command and returns trimmed stdout (stderr in the error).
func out(ctx context.Context, name string, args ...string) (string, error) {
	b, err := host.CombinedOutput(host.CommandContext(ctx, name, args...))
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	native := fs.Bool("native", false, "check the prerequisites of up --native instead of docker")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	docker := []check{
		{"docker daemon", true, "start Docker Desktop / dockerd", func(ctx context.Context) (string, error) {
			return out(ctx, "docker", "version", "--format", "{{.Server.Version}}")
		}},
		{"docker compose >= 2.24", true, "install the Docker Compose v2 plugin (2.24+)", func(ctx context.Context) (string, error) {
			v, err := out(ctx, "docker", "compose", "version", "--short")
			if err != nil {
				return "", err
			}
			p := strings.Split(strings.TrimPrefix(v, "v"), ".")
			maj, _ := strconv.Atoi(p[0])
			min := 0
			if len(p) > 1 {
				min, _ = strconv.Atoi(p[1])
			}
			if maj < 2 || (maj == 2 && min < 24) {
				return v, errors.New("too old")
			}
			return v, nil
		}},
		{"docker buildx", true, "install the docker buildx plugin", func(ctx context.Context) (string, error) {
			return out(ctx, "docker", "buildx", "version")
		}},
	}
	nativeChecks := []check{
		{"func (Functions Core Tools)", true, "npm i -g azure-functions-core-tools@4 (no admin needed)", func(ctx context.Context) (string, error) {
			return out(ctx, "func", "--version")
		}},
		{"npx (Node.js, for Azurite)", true, "install Node.js 22+", func(ctx context.Context) (string, error) {
			return out(ctx, "npx", "--version")
		}},
		{"python (python apps only)", false, "install the Python version of your apps", func(ctx context.Context) (string, error) {
			if v, err := out(ctx, "python", "--version"); err == nil {
				return v, nil
			}
			return out(ctx, "py", "--version")
		}},
		{"azemu CA trusted", host.GOOS() != "linux", "run: azemu trust", func(ctx context.Context) (string, error) {
			if ok, hint := up.Trusted(); !ok {
				return "", errors.New(hint)
			}
			return "yes", nil
		}},
	}
	checks := docker
	if *native {
		checks = nativeChecks
	}
	checks = append(checks, []check{
		{"terraform", true, "install terraform (https://developer.hashicorp.com/terraform/install)", func(ctx context.Context) (string, error) {
			j, err := out(ctx, "terraform", "version", "-json")
			var v struct {
				V string `json:"terraform_version"`
			}
			if err == nil {
				err = json.Unmarshal([]byte(j), &v)
			}
			return v.V, err
		}},
		{"az login", true, "run: az login", func(ctx context.Context) (string, error) {
			return out(ctx, "az", "account", "show", "--query", "name", "-o", "tsv")
		}},
		{"ports 10000-10002, 7001-7010", false, "stop whatever holds the port (or `azemu down`)", checkPorts},
		{"host CA dir writable", true, "fix permissions on " + contract.HostCADir(), func(ctx context.Context) (string, error) {
			d := contract.HostCADir()
			if err := os.MkdirAll(d, 0o755); err != nil {
				return "", err
			}
			f, err := os.CreateTemp(d, ".doctor-*")
			if err != nil {
				return "", err
			}
			f.Close()
			return d, os.Remove(f.Name())
		}},
	}...)

	errs := make([]error, len(checks))
	details := make([]string, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			details[i], errs[i] = c.run(ctx)
		}()
	}
	wg.Wait()

	failed := 0
	for i, c := range checks {
		switch {
		case errs[i] == nil:
			fmt.Printf("✓ %-30s %s\n", c.name, firstLine(details[i]))
		case !c.required:
			fmt.Printf("! %-30s %v\n    fix: %s\n", c.name, errs[i], c.hint)
		default:
			failed++
			fmt.Printf("✗ %-30s %v\n    fix: %s\n", c.name, errs[i], c.hint)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d required check(s) failed", failed)
	}
	return nil
}

func firstLine(s string) string { l, _, _ := strings.Cut(s, "\n"); return l }

// checkPorts: a port held by an azemu container is fine (already up); anything else is a conflict.
func checkPorts(ctx context.Context) (string, error) {
	var busy []string
	ports := []int{contract.AzuriteBlobPort, contract.AzuriteQueuePort, contract.AzuriteTablePort}
	for p := contract.IngressBasePort; p < contract.IngressBasePort+10; p++ {
		ports = append(ports, p)
	}
	for _, p := range ports {
		l, err := host.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			l.Close()
			continue
		}
		// ponytail: container ownership = name contains "azemu"; a project-specific compose name would need a label filter.
		if names, _ := out(ctx, "docker", "ps", "--filter", "publish="+strconv.Itoa(p), "--format", "{{.Names}}"); strings.Contains(names, "azemu") {
			continue
		}
		busy = append(busy, strconv.Itoa(p))
	}
	if len(busy) > 0 {
		return "", errors.New("in use: " + strings.Join(busy, ", "))
	}
	return "all free", nil
}
