package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/host"
	"github.com/luigi23997/azemu/internal/pull"
)

const storageUsage = `usage: azemu storage pull <account> [-f dir] [--env E] [--subscription S]
                          [--containers a,b] [--prefix p] [--tables t1,t2] [--all] [--concurrency 8]

Copies the named containers/tables (or --all of them) of the real storage account into the emulated
account of the same name. Needs 'azemu up' running and an az login whose role can list account keys.
Queues are not copied (ephemeral). Real data ends up on this machine; azemu down --volumes wipes it.`

func runStorage(args []string) error {
	if len(args) == 0 || args[0] != "pull" {
		return errors.New(storageUsage)
	}
	return storagePull(args[1:])
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func storagePull(args []string) error {
	fs := flag.NewFlagSet("storage pull", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(fs.Output(), storageUsage); fs.PrintDefaults() }
	file := fs.String("f", ".", "azemu.yaml or its directory")
	envFlag := fs.String("env", "", "environment (default: env in azemu.yaml)")
	sub := fs.String("subscription", "", "Azure subscription for az (default: the az default)")
	conts := fs.String("containers", "", "comma-separated containers to copy")
	tabs := fs.String("tables", "", "comma-separated tables to copy")
	var o pull.Options
	fs.StringVar(&o.Prefix, "prefix", "", "only blobs whose name starts with this")
	fs.BoolVar(&o.All, "all", false, "copy all containers and all tables")
	fs.IntVar(&o.Concurrency, "concurrency", 8, "parallel blob/entity transfers")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New(storageUsage)
	}
	acct := pos[0]
	o.Containers, o.Tables = splitList(*conts), splitList(*tabs)
	if err := o.Validate(); err != nil {
		return err
	}

	_, m, _, err := loadModel(*file, *envFlag, false)
	if err != nil {
		return err
	}
	var names []string
	found := false
	for _, a := range m.StorageAccounts {
		names = append(names, a.Name)
		found = found || a.Name == acct
	}
	if !found {
		return fmt.Errorf("%q is not an emulated storage account; emulated: %s", acct, strings.Join(names, ", "))
	}

	caPEM, err := os.ReadFile(filepath.Join(contract.HostCADir(), "ca.pem"))
	if err != nil {
		return errors.New("local emulator not found: run azemu up first")
	}
	for _, p := range []int{contract.AzuriteBlobPort, contract.AzuriteTablePort} {
		c, err := host.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), 2*time.Second)
		if err != nil {
			return errors.New("local Azurite is not reachable on 127.0.0.1: run azemu up first")
		}
		c.Close()
	}

	ctx, stop := ctrlC()
	defer stop()
	conn := os.Getenv("AZEMU_PULL_SOURCE_CONNECTION_STRING") // tests: any Azurite/account connection string, skips az
	if conn == "" {
		key, err := azAccountKey(ctx, acct, *sub)
		if err != nil {
			return err
		}
		o.Secret = key
		conn = fmt.Sprintf("DefaultEndpointsProtocol=https;AccountName=%s;AccountKey=%s;EndpointSuffix=core.windows.net", acct, key)
	}
	src, err := pull.SourceClients(conn)
	if err != nil {
		return scrub(err, o.Secret)
	}
	dst, err := pull.LocalClients(acct, caPEM, "127.0.0.1", contract.AzuriteBlobPort, contract.AzuriteTablePort)
	if err != nil {
		return err
	}
	fmt.Printf("Copying real data from %s into local emulator (stays on this machine)\n", acct)
	_, err = pull.Run(ctx, src, dst, o, os.Stdout)
	return scrub(err, o.Secret)
}

func scrub(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "***"))
}

// azAccountKey asks az (the user's login) for the account's first key. It lives in memory only.
func azAccountKey(ctx context.Context, acct, sub string) (string, error) {
	args := []string{"storage", "account", "keys", "list", "--account-name", acct, "--query", "[0].value", "-o", "tsv"}
	if sub != "" {
		args = append(args, "--subscription", sub)
	}
	var out, errb bytes.Buffer
	cmd := host.CommandContext(ctx, "az", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := host.Run(cmd); err != nil {
		if strings.Contains(errb.String(), "AuthorizationFailed") {
			return "", fmt.Errorf("az cannot list the keys of %s (AuthorizationFailed): this needs the listKeys permission "+
				"(Contributor or Storage Account Key Operator); activate your PIM role or use data-plane access instead", acct)
		}
		return "", fmt.Errorf("az storage account keys list failed: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	key := strings.TrimSpace(out.String())
	if key == "" {
		return "", errors.New("az returned no key for " + acct)
	}
	return key, nil
}
