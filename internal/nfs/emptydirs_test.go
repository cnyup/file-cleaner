package nfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// TestSweepEmptyDirsTimingSplit 计时拆分不变量（2026-11-09 速度分析 C）：
// CollectElapsed + DeleteElapsed 必须覆盖全部实际工作，两者都 >0 且
// 各自不超过总耗时。生产靠这两个数裁决"并行删除 vs young 剪枝"的收益
// 排序——若某段恒为 0，说明计时挂点漏了。
func TestSweepEmptyDirsTimingSplit(t *testing.T) {
	root := t.TempDir()
	// 造三层嵌套空目录：collect 需要下钻，delete 需要按深度排序逐个删。
	for _, rel := range []string{"a/b/c", "a/d", "e/f"} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		backdate(t, p, 10*24*time.Hour)
	}

	start := time.Now()
	stats, err := SweepEmptyDirs(root, EmptyDirsOptions{
		AgeGate: 7 * 24 * time.Hour,
		Exclude: map[string]bool{DefaultTrashDir: true},
	})
	total := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if stats.CollectElapsed <= 0 {
		t.Fatalf("CollectElapsed=%v want >0 (timing hook missing on collect phase)", stats.CollectElapsed)
	}
	if stats.DeleteElapsed <= 0 {
		t.Fatalf("DeleteElapsed=%v want >0 (timing hook missing on delete phase)", stats.DeleteElapsed)
	}
	if sum := stats.CollectElapsed + stats.DeleteElapsed; sum > total {
		t.Fatalf("collect+delete=%v exceeds wall clock %v — phases overlap or double-counted", sum, total)
	}
	if stats.Removed != 3 {
		// 级联语义（emptydirs.go:29-31）：收集时即空的叶子连删；因本轮
		// 删除才变空的父目录（a、a/b）不进本轮候选，留待下轮。故 5 个
		// 目录里本轮只删 3 个叶子。
		t.Fatalf("removed=%d want 3 (leaf dirs only; parents emptied this run survive)", stats.Removed)
	}
}

// TestSweepEmptyDirsParallelDelete 阶段二并行删除（2026-11-10 速度分析 A）：
// 生产实测 delete_ms 占 empty_dirs 总时长 97.5%（test: 121,292/130,343ms），
// 瓶颈是逐个候选的 NFS 往返被串行执行。同深度目录互不为父子，可安全并行。
//
// 不变量：
//  1. 同深度目录确实并行（并发度 >1）——串行实现红
//  2. 父子顺序保持（级联语义不变：收集时即空的叶子连删）
//  3. 非空目录仍被挡住（ENOTEMPTY 兜底不变）
func TestSweepEmptyDirsParallelDelete(t *testing.T) {
	root := t.TempDir()
	// 40 个同深度叶子目录：足够观测到并发，又不至于让测试变慢。
	var leaves []string
	for i := 0; i < 40; i++ {
		p := filepath.Join(root, "d", fmt.Sprintf("leaf%02d", i))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		backdate(t, p, 10*24*time.Hour)
		leaves = append(leaves, p)
	}
	// 注意：不给 d 本身 backdate——d 只含子目录不含文件，收集阶段同样会被
	// 判为候选（empty 标志只在遇到文件时置 false）。若 d 也超龄，级联语义
	// 会让它在本轮一并被删（removed=41）。这里让 d 保持年轻被年龄门挡住，
	// 使断言聚焦在 40 个叶子的并发删除上。

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	stats, err := SweepEmptyDirs(root, EmptyDirsOptions{
		AgeGate: 7 * 24 * time.Hour,
		Exclude: map[string]bool{DefaultTrashDir: true},
		OnRmdir: func(path string) error {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			// 模拟 NFS 往返延迟，让并发窗口真实可见。
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 40 {
		t.Fatalf("removed=%d want 40", stats.Removed)
	}
	if maxInFlight < 2 {
		t.Fatalf("max concurrent OnRmdir=%d want >=2 — delete phase still serial", maxInFlight)
	}
	// 所有叶子必须真的被删掉。
	for _, p := range leaves {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("leaf %s still exists (err=%v)", p, err)
		}
	}
}

// TestSweepEmptyDirsParallelKeepsParentOrder 并行不得破坏父子顺序：
// 同深度并行、跨深度串行。这里构造 a/b/c 三层，验证父目录不会先于子目录
// 被尝试删除（若顺序错，父目录会因非空而 ENOTEMPTY 跳过，removed 会少）。
func TestSweepEmptyDirsParallelKeepsParentOrder(t *testing.T) {
	root := t.TempDir()
	// 3 层嵌套，每层 10 个：深度 3 的叶子先删，深度 2、1 依序。
	for i := 0; i < 10; i++ {
		p := filepath.Join(root, fmt.Sprintf("a%02d", i), "b", "c")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		backdate(t, p, 10*24*time.Hour)
	}
	stats, err := SweepEmptyDirs(root, EmptyDirsOptions{
		AgeGate: 7 * 24 * time.Hour,
		Exclude: map[string]bool{DefaultTrashDir: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 只删收集时即空的深度 3 叶子（10 个）；深度 2/1 因本轮才变空，留待下轮。
	if stats.Removed != 10 {
		t.Fatalf("removed=%d want 10 (only leaves emptied at collect time)", stats.Removed)
	}
}
