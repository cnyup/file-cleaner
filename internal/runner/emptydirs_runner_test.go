package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bcyup/filecleaner/internal/config"
)

// emptyDirsPolicy 构造带 empty_dirs 段的最小策略（root 指向 t.TempDir）。
func emptyDirsPolicy(t *testing.T, root string, dryRunY bool) (*config.Config, *config.Policy) {
	t.Helper()
	cfg := &config.Config{StateDir: filepath.Join(t.TempDir(), "state"), AuditLog: config.AuditLog{Rotation: config.AuditRotationOff}}
	p := &config.Policy{
		ID:    "p-emptydirs",
		Root:  root,
		Trash: config.Trash{Dir: ".filecleaner-trash"},
		Retention: config.Retention{
			Match:      "AND",
			Conditions: map[string]string{"mtime_older_than": "1d"},
		},
		EmptyDirs: config.EmptyDirs{Enabled: true, Retention: "1h"},
		Concurrency: config.Concurrency{
			DirReadWorkers: 4, StatWorkers: 2, MoveWorkers: 2,
		},
	}
	_ = dryRunY
	return cfg, p
}

// backdateDir 拨回目录时间。
func backdateDir(t *testing.T, path string, d time.Duration) {
	t.Helper()
	past := time.Now().Add(-d)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// TestRunOnceEmptyDirsSweepAfterMoves 端到端：文件清理移走文件后留下
// 空父目录，同一轮 RunOnce 的 empty_dirs 尾扫将其删除（下轮删父级）。
func TestRunOnceEmptyDirsSweepAfterMoves(t *testing.T) {
	root := t.TempDir()
	cfg, p := emptyDirsPolicy(t, root, false)
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// job/ 子目录里放一个超龄文件；目录本身超龄。
	job := filepath.Join(root, "job")
	if err := os.MkdirAll(job, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(job, "result.json")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(f, old, old)
	backdateDir(t, job, 48*time.Hour)

	stats, err := RunOnce(context.Background(), cfg, p, false)
	if err != nil {
		t.Fatalf("runonce: %v", err)
	}
	// 文件被移走（进 trash）
	if stats.MovedOK != 1 {
		t.Fatalf("moved=%d want 1", stats.MovedOK)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatal("file should be in trash, not at source")
	}
	// 收集时 job 非空（文件还在）→ 不进本轮候选，本轮不删；
	// 但 trash 里的镜像目录树不该导致误删。
	if stats.EmptyDirsRemoved != 0 {
		t.Fatalf("empty_dirs removed=%d want 0 this run (parent non-empty at collect)", stats.EmptyDirsRemoved)
	}
	if _, err := os.Stat(job); err != nil {
		t.Fatalf("job dir must survive this run: %v", err)
	}

	// 第二轮：job 已空。注意 POSIX 语义——第一轮移走文件时父目录 mtime
	// 被刷新为 now（目录项删除也是 mtime 更新事件），这正是安全语义：
	// 刚清空的目录必须静置满 retention 才可删（业务可能还要用）。
	// 模拟"又过了 7 天"：backdate 后第二轮应删除。
	backdateDir(t, job, 48*time.Hour)
	stats2, err := RunOnce(context.Background(), cfg, p, false)
	if err != nil {
		t.Fatalf("runonce2: %v", err)
	}
	if stats2.EmptyDirsRemoved < 1 {
		t.Fatalf("run2 empty_dirs removed=%d want >=1", stats2.EmptyDirsRemoved)
	}
	if _, err := os.Stat(job); !os.IsNotExist(err) {
		t.Error("job dir should be removed in run 2")
	}
}

// TestRunOnceEmptyDirsDryRunNoDelete dry-run：统计有值但目录不动。
func TestRunOnceEmptyDirsDryRunNoDelete(t *testing.T) {
	root := t.TempDir()
	cfg, p := emptyDirsPolicy(t, root, true)
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hole := filepath.Join(root, "hole")
	if err := os.Mkdir(hole, 0o755); err != nil {
		t.Fatal(err)
	}
	backdateDir(t, hole, 48*time.Hour)

	stats, err := RunOnce(context.Background(), cfg, p, true)
	if err != nil {
		t.Fatalf("runonce dry: %v", err)
	}
	if stats.EmptyDirsCandidates == 0 {
		t.Fatal("dry-run should count candidates")
	}
	if _, err := os.Stat(hole); err != nil {
		t.Fatal("dry-run must not delete dirs")
	}
}

// TestRunOnceEmptyDirsAuditTrail 审计：rmdir 事件带 run_id/policy_id，
// totals 里含 empty_dirs 计数。
func TestRunOnceEmptyDirsAuditTrail(t *testing.T) {
	root := t.TempDir()
	cfg, p := emptyDirsPolicy(t, root, false)
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hole := filepath.Join(root, "hole")
	if err := os.Mkdir(hole, 0o755); err != nil {
		t.Fatal(err)
	}

	// 第一轮：hole 的 mtime 是 now（不 backdate）→ 年龄门挡住，不删。
	if _, err := RunOnce(context.Background(), cfg, p, false); err != nil {
		t.Fatal(err)
	}
	// 静置超龄后第二轮：hole 被删，产生 rmdir 审计事件。
	backdateDir(t, hole, 48*time.Hour)
	if _, err := RunOnce(context.Background(), cfg, p, false); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(cfg.StateDir, "audit.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rmdirs, finishedWithEmpty int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e map[string]any
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e["type"] == "rmdir_result" {
			rmdirs++
			if e["policy_id"] != "p-emptydirs" || e["run_id"] == "" {
				t.Fatalf("rmdir event missing ids: %v", e)
			}
		}
		if e["type"] == "run_finished" {
			tot, _ := e["extra"].(map[string]any)
			if tot != nil {
				if _, ok := tot["totals"].(map[string]any)["empty_dirs_removed"]; ok {
					finishedWithEmpty++
				}
				// 时长可观测性（2026-10-23）：分阶段时长必须落盘，否则
				// 生产 4h46m 轮无法归因（scan/drain/total/empty_dirs_ms）。
				if _, ok := tot["totals"].(map[string]any)["scan_ms"]; !ok {
					t.Fatal("run_finished totals missing scan_ms")
				}
				if _, ok := tot["totals"].(map[string]any)["total_ms"]; !ok {
					t.Fatal("run_finished totals missing total_ms")
				}
				if _, ok := tot["totals"].(map[string]any)["empty_dirs_ms"]; !ok {
					t.Fatal("run_finished totals missing empty_dirs_ms")
				}
			}
		}
	}
	if rmdirs == 0 {
		t.Fatal("no rmdir_result events in audit trail")
	}
	if finishedWithEmpty == 0 {
		t.Fatal("run_finished totals missing empty_dirs_removed")
	}
	_ = fmt.Sprint()
}
