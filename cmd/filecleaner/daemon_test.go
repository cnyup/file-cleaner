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

// writeDaemonCfg 生成带 schedule + purge_schedule 的最小配置。
func writeDaemonCfg(t *testing.T, dir string, schedule, purgeSchedule string) string {
	t.Helper()
	root := filepath.Join(dir, "data")
	os.MkdirAll(root, 0o755)
	state := filepath.Join(dir, "state")
	p := filepath.Join(dir, "config.yaml")
	content := `
state_dir: ` + state + `
audit_log: {rotation: off}
policies:
  - id: p1
    root: ` + root + `
    schedule: "` + schedule + `"
    retention:
      conditions:
        mtime_older_than: 1d
    trash:
      retention: 1h
      purge_schedule: "` + purgeSchedule + `"
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBuildJobsPurgeWired 验证 purge_schedule 生成独立 purge job：
// 每个配置了 purge_schedule 的策略恰好一个 purge job + 一个 run job。
func TestBuildJobsPurgeWired(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeDaemonCfg(t, dir, "0 2 * * *", "15 4 * * *")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if errs := config.Validate(cfg); len(errs) > 0 {
		t.Fatalf("validate: %v", errs)
	}
	jobs, err := buildJobs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var runN, purgeN int
	for _, j := range jobs {
		if j.kind == jobPurge {
			purgeN++
			if j.pol.Trash.PurgeSchedule != "15 4 * * *" {
				t.Fatalf("purge job schedule = %q", j.pol.Trash.PurgeSchedule)
			}
		} else {
			runN++
		}
	}
	if runN != 1 || purgeN != 1 {
		t.Fatalf("jobs: run=%d purge=%d, want 1/1", runN, purgeN)
	}
}

// TestBuildJobsNoPurgeSchedule purge_schedule 未配置 → 只有 run job。
func TestBuildJobsNoPurgeSchedule(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeDaemonCfg(t, dir, "0 2 * * *", "")
	cfg, _ := config.Load(cfgPath)
	jobs, err := buildJobs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].kind != jobRun {
		t.Fatalf("jobs = %+v, want single run job", jobs)
	}
}

// TestDaemonLoopFiresPurge 集成：极短间隔 purge_schedule（每分钟）+
// 已过期的桶 → daemon 触发 retire（桶 rename 进 .retired，毫秒级）。
// 物理删除由 sweep 承担（SweepOnce 单测覆盖），此处断言 retire 语义。
func TestDaemonLoopFiresPurge(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeDaemonCfg(t, dir, "0 3 31 2 *", "* * * * *") // 主清理永不触发（2月31日）；purge 每分钟
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	pol := &cfg.Policies[0]

	// 构造一个"已过期"的日期桶（昨天）：桶内一个文件
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	bucket := yesterday + ".r0"
	trashDir := filepath.Join(pol.Root, ".filecleaner-trash")
	bucketDir := filepath.Join(trashDir, bucket)
	os.MkdirAll(filepath.Join(bucketDir, "sub"), 0o755)
	os.WriteFile(filepath.Join(bucketDir, "sub", "old.txt"), []byte("expired"), 0o644)

	// 审计：桶内文件的 moved 记录（trash 路径含桶前缀）
	auditPath := filepath.Join(cfg.StateDir, "audit.jsonl")
	os.MkdirAll(cfg.StateDir, 0o755)
	old := time.Now().Add(-2 * time.Hour).UnixNano()
	trashPath := filepath.Join(bucketDir, "sub", "old.txt")
	line := `{"ts":` + itoa64(old) + `,"run_id":"r0","policy_id":"p1","type":"intent","src":"/x","trash":"` + trashPath + `"}` + "\n" +
		`{"ts":` + itoa64(old) + `,"run_id":"r0","policy_id":"p1","type":"result","src":"/x","trash":"` + trashPath + `","ok":true}` + "\n"
	os.WriteFile(auditPath, []byte(line), 0o640)

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	var out strings.Builder
	daemonLoop(ctx, cfg, &out)

	// retire 语义：桶离开活跃区、进入 .retired（文件保留等 sweep）
	if _, err := os.Lstat(bucketDir); !os.IsNotExist(err) {
		t.Fatalf("expired bucket must leave active area: %v", err)
	}
	retired := filepath.Join(trashDir, ".retired", bucket)
	if _, err := os.Lstat(filepath.Join(retired, "sub", "old.txt")); err != nil {
		t.Fatalf("retired bucket must keep files until sweep: %v", err)
	}
	if !strings.Contains(out.String(), "purge policy p1") {
		t.Fatalf("daemon output missing purge trigger:\n%s", out.String())
	}
}

func itoa64(n int64) string {
	return strings.TrimSpace(strings.Replace(fmtInt(n), " ", "", -1))
}

func fmtInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
