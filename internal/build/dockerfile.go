// Package build produces the image build inputs for each app.
package build

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/luigi23997/azemu/internal/config"
)

const uvImage = "ghcr.io/astral-sh/uv:0.11.0"

// Prepare returns the (context dir, dockerfile path) to build app key.
// Zip apps get a generated Dockerfile + Dockerfile.dockerignore under <StateDir>/build/<key>/
// (BuildKit reads <dockerfile>.dockerignore when -f points outside the context).
func Prepare(cfg *config.Config, key string) (contextDir, dockerfile string, err error) {
	app := cfg.Apps[key]
	if app == nil {
		return "", "", fmt.Errorf("unknown app %q", key)
	}
	contextDir = cfg.Abs(app.Path)
	switch app.Kind {
	case "docker":
		df := app.Dockerfile
		if df == "" {
			df = "Dockerfile"
		}
		return contextDir, filepath.Join(contextDir, df), nil
	case "zip":
		df, ign, err := generate(contextDir, app.Runtime)
		if err != nil {
			return "", "", fmt.Errorf("app %s: %w", key, err)
		}
		dockerfile = cfg.StatePath("build", key, "Dockerfile")
		if err := os.MkdirAll(filepath.Dir(dockerfile), 0o755); err != nil {
			return "", "", err
		}
		if err := writeIfChanged(dockerfile, df); err != nil {
			return "", "", err
		}
		return contextDir, dockerfile, writeIfChanged(dockerfile+".dockerignore", ign)
	}
	return "", "", fmt.Errorf("app %s: unknown kind %q (want zip or docker)", key, app.Kind)
}

// generate renders the Dockerfile and its dockerignore for a zip app in dir.
func generate(dir, runtime string) (dockerfile, dockerignore []byte, err error) {
	name, ver, _ := strings.Cut(runtime, ":")
	if name != "python" && name != "node" {
		return nil, nil, fmt.Errorf("unsupported runtime %s (supported: python, node)", runtime)
	}
	if ver == "" {
		return nil, nil, fmt.Errorf("runtime %q needs a version, e.g. %s:3.12", runtime, name)
	}
	var b bytes.Buffer
	b.WriteString("# syntax=docker/dockerfile:1\n")
	if name == "python" {
		pythonDockerfile(&b, dir, ver)
	} else {
		nodeDockerfile(&b, dir, ver)
	}
	return b.Bytes(), dockerignoreFor(dir), nil
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

const funcEnv = "ENV AzureWebJobsScriptRoot=/home/site/wwwroot \\\n    AzureFunctionsJobHost__Logging__Console__IsEnabled=true\n"

func pythonDockerfile(b *bytes.Buffer, dir, ver string) {
	reqs := "COPY requirements.txt /requirements.txt\n"
	if exists(dir, "uv.lock") && !exists(dir, "requirements.txt") {
		fmt.Fprintf(b, "FROM %s AS uv\nFROM python:%s-slim AS deps\nCOPY --from=uv /uv /usr/local/bin/uv\nWORKDIR /app\n"+
			"COPY pyproject.toml uv.lock ./\n"+
			"RUN uv export --frozen --no-dev --no-emit-project --no-hashes -o requirements.txt\n\n", uvImage, ver)
		reqs = "COPY --from=deps /app/requirements.txt /requirements.txt\n"
	}
	fmt.Fprintf(b, "FROM mcr.microsoft.com/azure-functions/python:4-python%s\n%s%s", ver, funcEnv, reqs)
	b.WriteString("RUN --mount=type=cache,target=/root/.cache/pip pip install -r /requirements.txt\n")
	b.WriteString("COPY . /home/site/wwwroot\n")
}

func nodeDockerfile(b *bytes.Buffer, dir, ver string) {
	install := "npm install"
	if exists(dir, "package-lock.json") {
		install = "npm ci"
	}
	build := HasBuildScript(dir)
	if !build {
		install += " --omit=dev"
	}
	fmt.Fprintf(b, "FROM mcr.microsoft.com/azure-functions/node:4-node%s\n%sWORKDIR /home/site/wwwroot\n", ver, funcEnv)
	fmt.Fprintf(b, "COPY package*.json ./\nRUN --mount=type=cache,target=/root/.npm %s\nCOPY . .\n", install)
	if build {
		b.WriteString("RUN npm run build && npm prune --omit=dev\n")
	}
}

// HasBuildScript reports whether package.json in dir has a build script.
func HasBuildScript(dir string) bool {
	// ponytail: substring check on package.json instead of parsing; a "build" key anywhere counts.
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	return err == nil && bytes.Contains(b, []byte(`"build"`))
}

var defaultIgnore = []string{
	".git", ".venv", "venv", "**/__pycache__", "**/*.pyc", "**/node_modules", ".pytest_cache", ".ruff_cache",
	".mypy_cache", "coverage*", ".coverage", "junit.xml", "tests", "local.settings.json", ".env*", ".azemu", "**/.DS_Store",
}

func dockerignoreFor(dir string) []byte {
	var b bytes.Buffer
	b.WriteString(strings.Join(defaultIgnore, "\n") + "\n")
	if f, err := os.ReadFile(filepath.Join(dir, ".funcignore")); err == nil {
		// ponytail: .funcignore lines are copied verbatim; its gitignore-only syntax (e.g. trailing-slash globs) is not translated.
		b.WriteString("# from .funcignore\n")
		for _, l := range strings.Split(string(f), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				b.WriteString(l + "\n")
			}
		}
	}
	return b.Bytes()
}

// writeIfChanged keeps mtimes stable so BuildKit caches and file watchers stay quiet.
func writeIfChanged(path string, content []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		return nil
	}
	return os.WriteFile(path, content, 0o644)
}
