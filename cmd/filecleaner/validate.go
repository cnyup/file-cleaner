package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/bcyup/filecleaner/internal/config"
)

// runValidate 实现 `filecleaner validate --config <yaml>`。
// Wave 0 骨架：加载 + 解析；完整 fail-closed 校验矩阵在 Wave 1 接入。
func runValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	path := fs.String("config", "config.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate: %v\n", err)
		return 1
	}
	fmt.Printf("validate: ok (state_dir=%s policies=%d)\n", cfg.StateDir, len(cfg.Policies))
	return 0
}
