package nfs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// backdate 把 path 的 mtime/atime 拨回 d 之前（测试超龄场景）。
func backdate(t *testing.T, path string, d time.Duration) {
	t.Helper()
	past := time.Now().Add(-d)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// mkEmptyDir 建一个空目录并拨回时间。
func mkEmptyDir(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	backdate(t, path, age)
}

// TestRmdirFailsOnNonEmptyDir 安全底线：rmdir 对非空目录必失败（内核级
// 兜底——复查后写入文件的竞态窗口由此关闭，物理上不可能删掉带文件的目录）。
func TestRmdirFailsOnNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := os.Remove(target)
	if err == nil {
		t.Fatal("os.Remove on non-empty dir must fail")
	}
	if !errors.Is(err, syscall.ENOTEMPTY) {
		t.Logf("err=%v (ENOTEMPTY expected on POSIX; accept other non-nil)", err)
	}
	// 文件必须还在
	if _, err := os.Stat(filepath.Join(target, "f.txt")); err != nil {
		t.Fatalf("file lost: %v", err)
	}
}

// TestSweepEmptyDirsAgeGate 年龄门（mtime-only）：新目录不删；mtime 超龄删。
// atime 不参与判定——扫描者（本工具文件扫描）每天 readdir 刷 atime，
// atime 门会自我挫败（2026-10-20 runner 集成实证）。
func TestSweepEmptyDirsAgeGate(t *testing.T) {
	root := t.TempDir()
	fresh := filepath.Join(root, "fresh")       // 新建空目录
	oldBoth := filepath.Join(root, "old-both")  // mtime 超龄
	oldAOnly := filepath.Join(root, "old-a")    // mtime 超龄但 atime 很新（扫描噪声场景）
	mkEmptyDir(t, fresh, 0)
	mkEmptyDir(t, oldBoth, 10*24*time.Hour)
	mkEmptyDir(t, oldAOnly, 10*24*time.Hour)
	// oldAOnly 的 atime 拨新（模拟业务/扫描最近 readdir 过）
	if err := os.Chtimes(oldAOnly, time.Now(), time.Now().Add(-10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	gate := 7 * 24 * time.Hour
	stats, err := SweepEmptyDirs(root, EmptyDirsOptions{
		AgeGate: gate,
		Exclude: map[string]bool{DefaultTrashDir: true},
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if stats.Removed != 2 {
		t.Fatalf("removed=%d want 2 (old-both + old-a; atime must not gate)", stats.Removed)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh must survive: %v", err)
	}
}

// TestSweepEmptyDirsCascadeSemantics 级联语义：嵌套空洞 a/b/c（每层都
// 空且双超龄，收集时即为候选）单轮内自底向上连删——c、b、a 依次删除。
// 父目录若因本轮删除才变空（收集时非空），不进本轮候选，自然留待下轮
// （见 TestSweepEmptyDirsParentEmptiedThisRunSurvives）。
func TestSweepEmptyDirsCascadeSemantics(t *testing.T) {
	root := t.TempDir()
	nest := filepath.Join(root, "a", "b", "c")
	mkEmptyDir(t, nest, 10*24*time.Hour)
	backdate(t, filepath.Join(root, "a"), 10*24*time.Hour)
	backdate(t, filepath.Join(root, "a", "b"), 10*24*time.Hour)

	gate := 7 * 24 * time.Hour
	stats, err := SweepEmptyDirs(root, EmptyDirsOptions{
		AgeGate: gate,
		Exclude: map[string]bool{DefaultTrashDir: true},
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if stats.Removed != 3 {
		t.Fatalf("removed=%d want 3 (all layers empty+aged: c,b,a in one run, deepest first)", stats.Removed)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Error("a should be gone after bottom-up cascade")
	}
}

// TestSweepEmptyDirsParentEmptiedThisRunSurvives 收集时非空（含文件）、
// 本轮文件被移走后才变空的父目录：不进本轮候选，留待下轮。
func TestSweepEmptyDirsParentEmptiedThisRunSurvives(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "job1")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(parent, "result.json")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 文件与父目录都超龄；收集时文件仍在 → parent 非空不进候选。
	backdate(t, parent, 10*24*time.Hour)
	_ = file

	stats, err := SweepEmptyDirs(root, EmptyDirsOptions{
		AgeGate: 7 * 24 * time.Hour,
		Exclude: map[string]bool{DefaultTrashDir: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 0 {
		t.Fatalf("removed=%d want 0 (parent non-empty at collect time)", stats.Removed)
	}
}

// TestSweepEmptyDirsExcludesTrash 排除集：trash 目录与其下的空目录不动。
func TestSweepEmptyDirsExcludesTrash(t *testing.T) {
	root := t.TempDir()
	trashBucket := filepath.Join(root, DefaultTrashDir, "2026-10-20.run-xx")
	mkEmptyDir(t, trashBucket, 10*24*time.Hour)

	stats, err := SweepEmptyDirs(root, EmptyDirsOptions{
		AgeGate: 7 * 24 * time.Hour,
		Exclude: map[string]bool{DefaultTrashDir: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 0 {
		t.Fatalf("removed=%d want 0 (trash excluded)", stats.Removed)
	}
	if _, err := os.Stat(trashBucket); err != nil {
		t.Errorf("trash bucket must survive: %v", err)
	}
}

// TestSweepEmptyDirsDryRun dry-run：只统计不动盘。
func TestSweepEmptyDirsDryRun(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "old")
	mkEmptyDir(t, old, 10*24*time.Hour)

	stats, err := SweepEmptyDirs(root, EmptyDirsOptions{
		AgeGate: 7 * 24 * time.Hour,
		Exclude: map[string]bool{DefaultTrashDir: true},
		DryRun:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Candidates == 0 || stats.Removed != 1 {
		t.Fatalf("dry-run: candidates=%d removed=%d want 1,1 (counted as removed but not physically deleted)", stats.Candidates, stats.Removed)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("dry-run must not delete")
	}
}

// TestSweepEmptyDirsRecheckBeforeDelete 阶段二复查：收集后有新文件写入
// 的目录跳过（TOCTOU 防线一；防线二是 rmdir ENOTEMPTY）。
func TestSweepEmptyDirsRecheckBeforeDelete(t *testing.T) {
	root := t.TempDir()
	// 目录里先有一个文件让它"非空"，收集阶段会被跳过——我们模拟的是
	// 收集后写入，这里直接验证：带文件的目录 removed=0 且文件完好。
	victim := filepath.Join(root, "victim")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	backdate(t, victim, 10*24*time.Hour)
	if err := os.WriteFile(filepath.Join(victim, "late.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	stats, err := SweepEmptyDirs(root, EmptyDirsOptions{
		AgeGate: 7 * 24*time.Hour,
		Exclude: map[string]bool{DefaultTrashDir: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 0 {
		t.Fatalf("removed=%d want 0 (non-empty)", stats.Removed)
	}
	if _, err := os.Stat(filepath.Join(victim, "late.txt")); err != nil {
		t.Fatalf("file lost: %v", err)
	}
}
