package infra

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/host"
)

const cacheTTL = 24 * time.Hour

// Plan returns the infra model for env, from cache unless refresh is set or the cache is stale.
func Plan(ctx context.Context, cfg *config.Config, env string, refresh bool) (*Model, error) {
	dir := cfg.Abs(cfg.Infra.Dir)
	tfData := cfg.StatePath("tf", env)
	modelFile := cfg.StatePath("infra", env+".json")
	keyFile := cfg.StatePath("infra", env+".key")

	var initArgs, planArgs []string
	initArgs = []string{"init", "-input=false", "-no-color", "-reconfigure"}
	for _, b := range cfg.Infra.BackendConfig {
		initArgs = append(initArgs, "-backend-config="+config.Expand(b, env))
	}
	planArgs = []string{"plan", "-refresh=false", "-lock=false", "-input=false", "-no-color"}
	for _, v := range cfg.Infra.VarFiles {
		planArgs = append(planArgs, "-var-file="+config.Expand(v, env))
	}

	run := func(stdout io.Writer, args ...string) error {
		cmd := host.CommandContext(ctx, "terraform", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "TF_DATA_DIR="+tfData, "TF_IN_AUTOMATION=1")
		var errBuf bytes.Buffer
		cmd.Stdout, cmd.Stderr = stdout, &errBuf
		if err := host.Run(cmd); err != nil {
			for _, l := range strings.Split(strings.TrimRight(errBuf.String(), "\n"), "\n") {
				fmt.Fprintln(os.Stderr, "[terraform] "+l)
			}
			return fmt.Errorf("terraform %s: %w", args[0], err)
		}
		return nil
	}

	// ponytail: plan_file is never cached; a .json is instant and a binary only costs init + show.
	if cfg.Infra.PlanFile != "" {
		pf := cfg.Abs(config.Expand(cfg.Infra.PlanFile, env))
		if strings.HasSuffix(pf, ".json") {
			b, err := os.ReadFile(pf)
			if err != nil {
				return nil, err
			}
			return FromPlanJSON(b, env)
		}
		if err := run(io.Discard, "init", "-input=false", "-no-color", "-backend=false"); err != nil {
			return nil, err
		}
		var js bytes.Buffer
		if err := run(&js, "show", "-json", pf); err != nil {
			return nil, err
		}
		return FromPlanJSON(js.Bytes(), env)
	}

	var ver bytes.Buffer
	if err := run(&ver, "version"); err != nil {
		return nil, err
	}
	verLine, _, _ := strings.Cut(ver.String(), "\n")
	key, err := cacheKey(dir, tfData, strings.Join(initArgs, " ")+"\n"+strings.Join(planArgs, " ")+"\n"+verLine)
	if err != nil {
		return nil, err
	}

	// ponytail: remote state changes aren't in the key; upgrade: include the `terraform state pull` serial.
	if !refresh {
		if st, err := os.Stat(modelFile); err == nil && time.Since(st.ModTime()) < cacheTTL {
			if k, _ := os.ReadFile(keyFile); string(k) == key {
				if b, err := os.ReadFile(modelFile); err == nil {
					var m Model
					if json.Unmarshal(b, &m) == nil {
						return &m, nil
					}
				}
			}
		}
	}

	if err := os.MkdirAll(tfData, 0o700); err != nil {
		return nil, err
	}
	planBin := filepath.Join(tfData, "plan.bin")
	defer os.Remove(planBin)
	if err := run(io.Discard, initArgs...); err != nil {
		return nil, err
	}
	if err := run(io.Discard, append(planArgs, "-out="+planBin)...); err != nil {
		return nil, err
	}
	var js bytes.Buffer
	if err := run(&js, "show", "-json", planBin); err != nil {
		return nil, err
	}
	m, err := FromPlanJSON(js.Bytes(), env)
	if err != nil {
		return nil, err
	}

	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.MkdirAll(filepath.Dir(modelFile), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(modelFile, b, 0o600); err != nil {
		return nil, err
	}
	return m, os.WriteFile(keyFile, []byte(key), 0o600)
}

// cacheKey hashes every *.tf, *.tfvars and .terraform.lock.hcl under dir plus extra.
func cacheKey(dir, tfData, extra string) (string, error) {
	h := sha256.New()
	h.Write([]byte(extra))
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".terraform" || p == tfData {
				return filepath.SkipDir
			}
			return nil
		}
		n := d.Name()
		if !strings.HasSuffix(n, ".tf") && !strings.HasSuffix(n, ".tfvars") && n != ".terraform.lock.hcl" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(h, "\n%s\n", rel)
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}
