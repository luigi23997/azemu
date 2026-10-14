// Command azemu runs Azure Function Apps and App Services locally, wired like Azure.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

const usage = `azemu — run your Azure apps locally, wired like Azure.

Usage: azemu <command> [flags]

Commands:
  init      discover apps from pipelines + terraform, write azemu.yaml
  up        build and start everything in parallel, then watch and rebuild (--native: no containers)
  down      stop the stack (--volumes wipes emulated storage)
  status    show apps, local URLs, state
  env       print the resolved settings of an app
  logs      follow logs
  seed      re-run seed steps
  secrets   'secrets init' writes missing Key Vault secret names into .env
  doctor    check prerequisites (--native: for up --native)
  trust     add the azemu CA to your user's trust store (needed by up --native on Windows and macOS)
  storage   'storage pull <account>' copies selected real containers/tables into the emulator
  graph     'graph login' signs you in so apps using a Graph app registration run with your own access
  infra     'infra show' prints the infra model; 'infra verify' diffs static vs terraform plan
  proxy     (internal) run the in-network proxy
  version   print version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"init": runInit, "up": runUp, "down": runDown, "status": runStatus,
		"env": runEnv, "logs": runLogs, "seed": runSeed, "secrets": runSecrets, "storage": runStorage,
		"doctor": runDoctor, "trust": runTrust, "graph": runGraph, "proxy": runProxy, "infra": runInfra,
		"version": func([]string) error { fmt.Println(version); return nil },
	}
	cmd, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Print(usage)
		os.Exit(2)
	}
	if err := cmd(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "azemu:", err)
		os.Exit(1)
	}
}
