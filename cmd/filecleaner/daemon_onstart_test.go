package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bcyup/filecleaner/internal/config"
)

// P1 回归（run_on_start）：全局 run_on_start: true 时，daemon 启动即对
// 每个 run job 先跑一轮再进入定时循环——消除部署完成到首个 cron 触发
// 之间的清理空窗（生产实况：02:00 首调前欠账无人还）。
//
// 主清理 schedule 用 2 月 31 日（永不触发）隔离变量：审计文件里出现
// run 边界事件只可能来自 on-start 轮。
func TestDaemonLoopRunOnStart(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	os.MkdirAll(root, 0o755)
	// 一个过期文件：mtime 48h 前，条件 1d → on-start 轮应命中并移动
	old := time.Now().Add(-48 * time.Hour)
	p := filepath.Join(root, "stale.txt")
	os.WriteFile(p, []byte("x"), 0o644)
	os.Chtimes(p, old, old)

	state := filepath.Join(dir, "state")
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `
state_dir: ` + state + `
audit_log: {rotation: off}
run_on_start: true
policies:
  - id: p1
    root: ` + root + `
    schedule: "0 3 31 2 *"
    retention:
      conditions:
        mtime_older_than: 1d
    trash:
      retention: 1h
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v (run_on_start key must be accepted)", err)
	}
	if errs := config.Validate(cfg); len(errs) > 0 {
		t.Fatalf("validate: %v", errs)
	}
	if !cfg.RunOnStart {
		t.Fatal("cfg.RunOnStart must parse true")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out strings.Builder
	daemonLoop(ctx, cfg, &out)

	if !strings.Contains(out.String(), "on-start") {
		t.Fatalf("daemon output missing on-start run:\n%s", out.String())
	}
	// 物理 + 审计双重断言：stale.txt 进桶，run 边界事件落盘
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("on-start run must move stale file: %v", err)
	}
	auditData, err := os.ReadFile(filepath.Join(state, "audit.jsonl"))
	if err != nil || !strings.Contains(string(auditData), "run_started") {
		t.Fatalf("on-start run must write audit events: %v", err)
	}
}

// 反例：run_on_start 缺省（false）时启动不跑——永不触发的 schedule 下
// 审计文件不应出现（保持既有行为，防止 on-start 变成无条件）。
func TestDaemonLoopNoRunOnStartByDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeDaemonCfg(t, dir, "0 3 31 2 *", "")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunOnStart {
		t.Fatal("run_on_start must default false")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out strings.Builder
	daemonLoop(ctx, cfg, &out)

	auditPath := filepath.Join(cfg.StateDir, "audit.jsonl")
	if _, err := os.Stat(auditPath); !os.IsNotExist(err) {
		t.Fatalf("no on-start run expected, but audit exists")
	}
}
