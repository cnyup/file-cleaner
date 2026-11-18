package main

// runEmptyDirs 实现 `filecleaner empty-dirs --config <yaml> --policy <id> [--dry-run]`：
// 只跑空目录清理，跳过全树文件扫描（2026-11-16）。
//
// 使用场景：超大 NAS（如 basic-serice，94 万顶层目录）的全树 scan 跑不完，
// 但空目录清理本身可以在合理时间内完成。此命令让 empty_dirs 独立运行。
import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/bcyup/filecleaner/internal/config"
	"github.com/bcyup/filecleaner/internal/runner"
)

func runEmptyDirs(args []string) int {
	fs := flag.NewFlagSet("empty-dirs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "config file")
	policyID := fs.String("policy", "", "policy id (empty = all policies with empty_dirs enabled)")
	dryRun := fs.Bool("dry-run", false, "count candidates without deleting")
	ignoreWindow := fs.Bool("ignore-window", false, "override streaming window guard (debug only; daytime readdir on mega-dirs stalls the NAS)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "empty-dirs: %v\n", err)
		return 1
	}
	if errs := config.Validate(cfg); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "empty-dirs: invalid config: %v\n", e)
		}
		return 1
	}

	var targets []*config.Policy
	for i := range cfg.Policies {
		p := &cfg.Policies[i]
		if *policyID != "" && p.ID != *policyID {
			continue
		}
		if !p.EmptyDirs.Enabled {
			continue
		}
		targets = append(targets, p)
	}
	if len(targets) == 0 {
		if *policyID != "" {
			fmt.Fprintf(os.Stderr, "empty-dirs: policy %q not found or empty_dirs not enabled\n", *policyID)
		} else {
			fmt.Fprintln(os.Stderr, "empty-dirs: no policy has empty_dirs enabled")
		}
		return 1
	}

	exit := 0
	for _, p := range targets {
		// 窗口熔断（mode=streaming 必配窗口）：窗外拒绝启动。白天对巨目录
		// readdir 是实测过的生产风险（探针①：首 READDIR 8 分钟不返回，
		// 进程 D 态；探针⑥：夜间同目录 ~3.2k/s——60 倍差距），不是提示，
		// 是硬拒绝。--ignore-window 显式覆盖（仅限排障）。
		if p.EmptyDirs.Mode == "streaming" && p.EmptyDirs.Window != "" && !*ignoreWindow {
			if in, err := config.InWindow(p.EmptyDirs.Window, time.Now()); err != nil {
				fmt.Fprintf(os.Stderr, "empty-dirs: policy %q: %v\n", p.ID, err)
				exit = 1
				continue
			} else if !in {
				fmt.Fprintf(os.Stderr, "empty-dirs: policy %q: now %s is outside window %q — daytime readdir on mega-dirs stalls the NAS (measured 60x slower than night). Use --ignore-window to override.\n",
					p.ID, time.Now().Format("15:04"), p.EmptyDirs.Window)
				exit = 1
				continue
			}
		}
		res, err := runner.RunEmptyDirs(cfg, p, *dryRun)
		if err != nil {
			fmt.Fprintf(os.Stderr, "empty-dirs: %v\n", err)
			exit = 1
			continue
		}
		out, _ := json.Marshal(res)
		fmt.Println(string(out))
	}
	return exit
}
