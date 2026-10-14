package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/luigi23997/azemu/internal/up"
)

func ctrlC() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

func runUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	var o up.Options
	fs.StringVar(&o.File, "f", ".", "azemu.yaml or its directory")
	fs.StringVar(&o.Env, "env", "", "environment (default: env in azemu.yaml)")
	fs.StringVar(&o.As, "as", "", "Easy Auth identity (default: identity.as, then git config user.email)")
	fs.BoolVar(&o.Refresh, "refresh-infra", false, "force a new terraform plan")
	fs.BoolVar(&o.NoWatch, "no-watch", false, "start, print the status table and exit")
	fs.BoolVar(&o.KeepTelemetry, "keep-telemetry", false, "keep App Insights settings")
	native := fs.Bool("native", false, "run without containers: apps as local processes (func start / run:), Azurite via npx")
	apps, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	o.Apps, o.Version = apps, version
	ctx, stop := ctrlC()
	defer stop()
	if *native {
		return up.RunNative(ctx, o)
	}
	return up.Run(ctx, o)
}

func runTrust(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: azemu trust")
	}
	return up.Trust()
}

func runDown(args []string) error {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	file := fs.String("f", ".", "azemu.yaml or its directory")
	vol := fs.Bool("volumes", false, "also wipe emulated storage")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	ctx, stop := ctrlC()
	defer stop()
	if err := up.Down(ctx, *file, *vol); err != nil {
		return err
	}
	if *vol {
		fmt.Println("stopped; emulated storage wiped")
	} else {
		fmt.Println("stopped; emulated storage kept (use --volumes to wipe)")
	}
	return nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	file := fs.String("f", ".", "azemu.yaml or its directory")
	env := fs.String("env", "", "environment (default: env in azemu.yaml)")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	ctx, stop := ctrlC()
	defer stop()
	return up.Status(ctx, *file, *env)
}

func runLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	file := fs.String("f", ".", "azemu.yaml or its directory")
	svc, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	ctx, stop := ctrlC()
	defer stop()
	return up.Logs(ctx, *file, svc)
}

func runSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	file := fs.String("f", ".", "azemu.yaml or its directory")
	env := fs.String("env", "", "environment (default: env in azemu.yaml)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return errors.New("usage: azemu seed [-f azemu.yaml] [--env E]")
	}
	ctx, stop := ctrlC()
	defer stop()
	return up.Seed(ctx, *file, *env)
}
