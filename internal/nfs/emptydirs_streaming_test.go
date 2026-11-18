package nfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// SweepEmptyDirsStreaming 契约（RED 先行）：
//
//  1. 流式：读一批 → 判龄 → 删一批，不等全树扫完（与 SweepEmptyDirs 的
//     两阶段相区别；面向单目录百万级直接子目录的巨目录，探针⑥实测
//     夜间 readdir ~3.2k/s，26M 子目录单窗口可清）。
//  2. checkpoint：每批落 cookie+计数，中断后重启从 checkpoint 续扫，
//     已删目录不重复处理（empty_dirs_resumed 指标 > 0）。
//  3. 窗口：window 配置（"01:00-07:00"，支持跨天）外运行到窗口尾时
//     优雅停止并落最后 checkpoint（不硬中断）。
//  4. 安全语义与 SweepEmptyDirs 对齐：mtime-only 年龄门 + rmdir 内核
//     ENOTEMPTY 兜底；不做删前全量 readdir 复查（reader 刚读过，间隔
//     <1 个流水线周期，三重保险留二重——探针结论：26M 目录各一次
//     复查 readdir 本身就是另一场 2.3h 全量遍历）。

// TestStreamingSweepBasics 基本流：超龄空目录删、young 保留、非目录条目
// 使父目录处理仍安全（streaming 只处理 root 的直接子目录，不递归）。
func TestStreamingSweepBasics(t *testing.T) {
	root := t.TempDir()
	old1 := filepath.Join(root, "old1")
	old2 := filepath.Join(root, "old2")
	young := filepath.Join(root, "young")
	fileEntry := filepath.Join(root, "file.txt")
	mkEmptyDir(t, old1, 10*24*time.Hour)
	mkEmptyDir(t, old2, 10*24*time.Hour)
	mkEmptyDir(t, young, 0)
	if err := os.WriteFile(fileEntry, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	stats, err := SweepEmptyDirsStreaming(root, EmptyDirsStreamingOptions{
		AgeGate:       7 * 24 * time.Hour,
		CheckpointDir: t.TempDir(),
		Now:           time.Now,
	})
	if err != nil {
		t.Fatalf("streaming sweep: %v", err)
	}
	if stats.Removed != 2 {
		t.Fatalf("Removed = %d, want 2 (old1, old2)", stats.Removed)
	}
	if stats.SkippedYoung != 1 {
		t.Fatalf("SkippedYoung = %d, want 1", stats.SkippedYoung)
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatalf("young dir must survive: %v", err)
	}
	if _, err := os.Stat(fileEntry); err != nil {
		t.Fatalf("file entry must survive: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root must survive: %v", err)
	}
	if stats.Checkpointed <= 0 {
		t.Fatalf("Checkpointed = %d, want > 0 (progress persisted)", stats.Checkpointed)
	}
}

// TestStreamingSweepCheckpointResume 中断续扫：第一轮 window 截断后，
// 第二轮带 checkpoint 重启，跳过已删部分（resumed 计数）且不重扫全量。
func TestStreamingSweepCheckpointResume(t *testing.T) {
	root := t.TempDir()
	// 建 10 个超龄空目录，名字保证 readdir 顺序（d1..d10）。
	for i := 0; i < 10; i++ {
		mkEmptyDir(t, filepath.Join(root, fname(i)), 10*24*time.Hour)
	}
	ckDir := t.TempDir()

	// 第一轮：max_entries=4 模拟窗口截断（真实场景是时间窗）。
	st1, err := SweepEmptyDirsStreaming(root, EmptyDirsStreamingOptions{
		AgeGate:       7 * 24 * time.Hour,
		CheckpointDir: ckDir,
		MaxEntries:    4,
		Now:           time.Now,
	})
	if err != nil {
		t.Fatalf("round1: %v", err)
	}
	if st1.Stopped {
		t.Fatalf("round1 should report Stopped=false (limit reached), got %+v", st1)
	}
	if st1.EntriesScanned != 4 {
		t.Fatalf("round1 scanned = %d, want 4", st1.EntriesScanned)
	}

	// 第二轮：同 checkpoint 目录续跑，应跳过已删的 4 个。
	st2, err := SweepEmptyDirsStreaming(root, EmptyDirsStreamingOptions{
		AgeGate:       7 * 24 * time.Hour,
		CheckpointDir: ckDir,
		Now:           time.Now,
	})
	if err != nil {
		t.Fatalf("round2: %v", err)
	}
	if st2.Resumed == 0 {
		t.Fatalf("round2 Resumed = 0, want > 0 (checkpoint must be reused)")
	}
	if st2.EntriesScanned != 6 {
		t.Fatalf("round2 scanned = %d, want 6 (10 - 4 already handled)", st2.EntriesScanned)
	}
	if st1.Removed+st2.Removed != 10 {
		t.Fatalf("total removed = %d, want 10", st1.Removed+st2.Removed)
	}
	// 全部删完后 checkpoint 应被清理（任务完成自愈）。
	ckFile := filepath.Join(ckDir, checkpointName(root))
	if _, err := os.Stat(ckFile); !os.IsNotExist(err) {
		t.Fatalf("checkpoint file should be removed after completion, stat err=%v", err)
	}
}

// TestStreamingSweepWindowStop 窗口熔断：stop_at 已过时立即优雅停止，
// 已处理部分计数正确，checkpoint 落盘。
func TestStreamingSweepWindowStop(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		mkEmptyDir(t, filepath.Join(root, fname(i)), 10*24*time.Hour)
	}
	now := time.Now()
	st, err := SweepEmptyDirsStreaming(root, EmptyDirsStreamingOptions{
		AgeGate:       7 * 24 * time.Hour,
		CheckpointDir: t.TempDir(),
		StopAt:        now.Add(-time.Minute), // 窗口已过
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("window sweep: %v", err)
	}
	if !st.Stopped {
		t.Fatal("Stopped must be true when StopAt is already past")
	}
	if st.EntriesScanned != 0 {
		t.Fatalf("scanned = %d, want 0 when window already closed", st.EntriesScanned)
	}
}

// TestStreamingSweepOnRmdir 审计回调：每个删除调用一次 OnRmdir。
func TestStreamingSweepOnRmdir(t *testing.T) {
	root := t.TempDir()
	mkEmptyDir(t, filepath.Join(root, "a"), 10*24*time.Hour)
	mkEmptyDir(t, filepath.Join(root, "b"), 10*24*time.Hour)
	var seen []string
	st, err := SweepEmptyDirsStreaming(root, EmptyDirsStreamingOptions{
		AgeGate: 7 * 24 * time.Hour,
		OnRmdir: func(path string) error {
			seen = append(seen, path)
			return nil
		},
		Now: time.Now,
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if st.Removed != 2 || len(seen) != 2 {
		t.Fatalf("Removed=%d OnRmdir calls=%d, want 2/2", st.Removed, len(seen))
	}
}

// TestStreamingSweepCheckpointFormat checkpoint 文件格式：单行 JSON，
// 字段 cookie/scanned/removed（人可读、可人工审计、崩溃可删）。
func TestStreamingSweepCheckpointFormat(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		mkEmptyDir(t, filepath.Join(root, fname(i)), 10*24*time.Hour)
	}
	ckDir := t.TempDir()
	st, err := SweepEmptyDirsStreaming(root, EmptyDirsStreamingOptions{
		AgeGate:       7 * 24 * time.Hour,
		CheckpointDir: ckDir,
		MaxEntries:    2,
		Now:           time.Now,
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if st.Stopped {
		t.Fatal("unexpected stop")
	}
	raw, err := os.ReadFile(filepath.Join(ckDir, checkpointName(root)))
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}
	var ck struct {
		Cookie   int64 `json:"cookie"`
		Scanned  int64 `json:"scanned"`
		Removed  int64 `json:"removed"`
		Root     string `json:"root"`
	}
	if err := json.Unmarshal(raw, &ck); err != nil {
		t.Fatalf("checkpoint not single-line JSON: %v\nraw: %s", err, raw)
	}
	if ck.Scanned != 2 || ck.Removed != 2 {
		t.Fatalf("checkpoint scanned/removed = %d/%d, want 2/2", ck.Scanned, ck.Removed)
	}
	if ck.Root != root {
		t.Fatalf("checkpoint root = %q, want %q", ck.Root, root)
	}
}

func fname(i int) string {
	const digits = "0123456789"
	return "d" + string(rune('0'+i%10))
}

func checkpointName(root string) string {
	// 稳定派生：root 路径做 sanitize，跨重启可复现。
	b := []byte(root)
	for i := range b {
		if b[i] == '/' || b[i] == ':' || b[i] == ' ' {
			b[i] = '_'
		}
	}
	return "emptydirs-" + string(b) + ".json"
}
