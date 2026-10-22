package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/bcyup/filecleaner/internal/config"
	"github.com/bcyup/filecleaner/internal/runner"
)

// runOnce 实现 `filecleaner once --config <yaml> --policy <id> [--dry-run]`。
func runOnce(args []string) int {
	fs := flag.NewFlagSet("once", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "config file")
	policyID := fs.String("policy", "", "policy id")
	dryRun := fs.Bool("dry-run", false, "scan and evaluate without moving")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "once: %v\n", err)
		return 1
	}
	if errs := config.Validate(cfg); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "once: invalid config: %v\n", e)
		}
		return 1
	}
	var pol *config.Policy
	for i := range cfg.Policies {
		if cfg.Policies[i].ID == *policyID {
			pol = &cfg.Policies[i]
			break
		}
	}
	if pol == nil {
		fmt.Fprintf(os.Stderr, "once: policy %q not found\n", *policyID)
		return 1
	}
	stats, err := runner.RunOnce(context.Background(), cfg, pol, *dryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "once: %v\n", err)
		return 1
	}
	out, _ := json.Marshal(stats)
	fmt.Println(string(out))
	return 0
}

// runRestore 实现 `filecleaner restore --config <yaml> --policy <id> --path <rel>`。
func runRestore(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "config file")
	policyID := fs.String("policy", "", "policy id")
	path := fs.String("path", "", "relative path to restore")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		return 1
	}
	var pol *config.Policy
	for i := range cfg.Policies {
		if cfg.Policies[i].ID == *policyID {
			pol = &cfg.Policies[i]
		}
	}
	if pol == nil {
		fmt.Fprintf(os.Stderr, "restore: policy %q not found\n", *policyID)
		return 1
	}
	if err := runner.Restore(cfg, pol, *path); err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		return 1
	}
	fmt.Printf("restore: ok %s\n", *path)
	return 0
}

// runPurge 实现 `filecleaner purge --config <yaml> --policy <id>`。
func runPurge(args []string) int {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "config file")
	policyID := fs.String("policy", "", "policy id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purge: %v\n", err)
		return 1
	}
	var pol *config.Policy
	for i := range cfg.Policies {
		if cfg.Policies[i].ID == *policyID {
			pol = &cfg.Policies[i]
		}
	}
	if pol == nil {
		fmt.Fprintf(os.Stderr, "purge: policy %q not found\n", *policyID)
		return 1
	}
	purged, err := runner.Purge(cfg, pol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purge: %v\n", err)
		return 1
	}
	out, _ := json.Marshal(purged)
	fmt.Println(string(out))
	return 0
}

// runAuditQuery 实现 `filecleaner audit query --config <yaml> [--policy <id>]`。
func runAuditQuery(args []string) int {
	fs := flag.NewFlagSet("audit query", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "config file")
	policyID := fs.String("policy", "", "policy id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit query: %v\n", err)
		return 1
	}
	for i := range cfg.Policies {
		if *policyID != "" && cfg.Policies[i].ID != *policyID {
			continue
		}
		rows, err := runner.AuditQuery(cfg, &cfg.Policies[i])
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit query: %v\n", err)
			return 1
		}
		for _, r := range rows {
			out, _ := json.Marshal(r)
			fmt.Println(string(out))
		}
	}
	return 0
}
