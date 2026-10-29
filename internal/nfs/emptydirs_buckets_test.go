package nfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// skipBuckets 辅助：跑一轮 SweepEmptyDirs，返回分桶统计。
func skipBuckets(t *testing.T, root string, ageGate time.Duration) *EmptyDirsStats {
	t.Helper()
	st, err := SweepEmptyDirs(root, EmptyDirsOptions{AgeGate: ageGate})
	if err != nil {
		t.Fatalf("SweepEmptyDirs: %v", err)
	}
	return st
}

// TestEmptyDirsSkipReasonBuckets skip 原因分桶（2026-10-29 计划 T2）：
// 复查非空与 mtime 未超龄必须分开计数——两者混在一个 Skipped 里导致
// 优化方向（mtime 前移 vs 只查 moved 祖先）无法用生产数据裁决。
func TestEmptyDirsSkipReasonBuckets(t *testing.T) {
	dir := t.TempDir()
	// 1) 超龄空目录 → Removed。
	old := filepath.Join(dir, "old-empty")
	os.MkdirAll(old, 0o755)
	backdate(t, old, 10*time.Hour)
	// 2) 未超龄空目录 → SkippedYoung。
	yng := filepath.Join(dir, "young-empty")
	os.MkdirAll(yng, 0o755) // mtime = now
	// 3) 复查时被回填的目录：收集后、复查前写入文件 → SkippedNonEmpty。
	refilled := filepath.Join(dir, "refilled")
	os.MkdirAll(refilled, 0o755)
	backdate(t, refilled, 10*time.Hour)

	st, err := SweepEmptyDirs(dir, EmptyDirsOptions{
		AgeGate: 5 * time.Hour,
		OnRmdir: func(path string) error {
			// 在删除回调（复查 readdir 之后、rmdir 之前不适用——本回调
			// 在 readdir 空与超龄判定后触发）里往 refilled 写文件，
			// 模拟"复查窗口内业务回填"。rmdir 将 ENOTEMPTY。
			if filepath.Base(path) == "refilled" {
				os.WriteFile(filepath.Join(path, "f.txt"), []byte("x"), 0o644)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("SweepEmptyDirs: %v", err)
	}
	if st.Removed != 1 {
		t.Fatalf("Removed = %d, want 1 (old-empty only)", st.Removed)
	}
	if st.SkippedYoung != 1 {
		t.Fatalf("SkippedYoung = %d, want 1 (young-empty)", st.SkippedYoung)
	}
	if st.SkippedNonEmpty != 1 {
		t.Fatalf("SkippedNonEmpty = %d, want 1 (refilled → ENOTEMPTY 兜底)", st.SkippedNonEmpty)
	}
	// 向后兼容：Skipped 合计 = 分桶之和（ENOENT 类 0）。
	if st.Skipped != st.SkippedNonEmpty+st.SkippedYoung {
		t.Fatalf("Skipped(%d) != NonEmpty(%d)+Young(%d)", st.Skipped, st.SkippedNonEmpty, st.SkippedYoung)
	}
}

// TestEmptyDirsSkipGoneBucket 复查时目录已被他人删除（ENOENT）：
// 计入 SkippedGone，不混入 NonEmpty/Young。
func TestEmptyDirsSkipGoneBucket(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	os.MkdirAll(gone, 0o755)
	backdate(t, gone, 10*time.Hour)

	st, err := SweepEmptyDirs(dir, EmptyDirsOptions{
		AgeGate: 5 * time.Hour,
		OnRmdir: func(path string) error {
			if filepath.Base(path) == "gone" {
				os.Remove(path) // 复查通过后、rmdir 前被并发删除
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("SweepEmptyDirs: %v", err)
	}
	if st.SkippedGone != 1 {
		t.Fatalf("SkippedGone = %d, want 1", st.SkippedGone)
	}
	if st.SkippedNonEmpty != 0 || st.SkippedYoung != 0 {
		t.Fatalf("nonempty/young = %d/%d, want 0/0", st.SkippedNonEmpty, st.SkippedYoung)
	}
}
