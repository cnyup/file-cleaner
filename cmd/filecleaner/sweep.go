package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/bcyup/filecleaner/internal/config"
	"github.com/bcyup/filecleaner/internal/runner"
)

// runSweep 实现 `filecleaner sweep --config <yaml> [--policy <id>]`：
// 手动触发一轮回收站物理删除（.retired/.purging 桶）。
func runSweep(args []string) int {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "config file")
	policyID := fs.String("policy", "", "policy id (empty = all)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sweep: %v\n", err)
		return 1
	}
	total := 0
	for i := range cfg.Policies {
		if *policyID != "" && cfg.Policies[i].ID != *policyID {
			continue
		}
		n, err := runner.Sweep(cfg, &cfg.Policies[i])
		if err != nil {
			fmt.Fprintf(os.Stderr, "sweep: policy %s: %v\n", cfg.Policies[i].ID, err)
			return 1
		}
		total += n
	}
	fmt.Printf("sweep: %d files physically deleted\n", total)
	return 0
}
