package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Wave 1 轮转核心测试：rotation 分文件、Prune 窗口、多文件聚合、幂等导入。

func mkDate(y int, mo time.Month, d int) time.Time {
	return time.Date(y, mo, d, 12, 0, 0, 0, time.Local)
}

// TestOpenJSONLDailyRotationSeparatesDays T1：daily 模式下不同日期打开
// 产生不同分片；off 模式回 legacy 单文件。
func TestOpenJSONLDailyRotationSeparatesDays(t *testing.T) {
	dir := t.TempDir()

	d1 := mkDate(2026, 10, 20)
	d2 := mkDate(2026, 10, 21)

	j1, err := OpenJSONLRotating(dir, RotationDaily, d1)
	if err != nil {
		t.Fatalf("open d1: %v", err)
	}
	j1.RunStarted("r1", "p1", "hash1")
	j1.Close()

	j2, err := OpenJSONLRotating(dir, RotationDaily, d2)
	if err != nil {
		t.Fatalf("open d2: %v", err)
	}
	j2.RunStarted("r2", "p1", "hash1")
	j2.Close()

	for name, want := range map[string]bool{
		"audit-20261020.jsonl": true,
		"audit-20261021.jsonl": true,
		"audit.jsonl":          false,
	} {
		_, err := os.Stat(filepath.Join(dir, name))
		if got := err == nil; got != want {
			t.Errorf("file %s exists=%v want %v (err=%v)", name, got, want, err)
		}
	}

	// off 模式：写 legacy audit.jsonl
	j3, err := OpenJSONLRotating(dir, RotationOff, d2)
	if err != nil {
		t.Fatalf("open off: %v", err)
	}
	j3.Close()
	if _, err := os.Stat(filepath.Join(dir, "audit.jsonl")); err != nil {
		t.Errorf("off mode should write legacy audit.jsonl: %v", err)
	}
}

// TestAuditFilesAggregates T2：AuditFiles 聚合分片 + legacy，legacy 在前、
// 分片按日期序。
func TestAuditFilesAggregates(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"audit-20261020.jsonl", "audit-20261019.jsonl", "audit.jsonl", "audit.db"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	files, err := AuditFiles(dir)
	if err != nil {
		t.Fatalf("AuditFiles: %v", err)
	}
	want := []string{
		filepath.Join(dir, "audit.jsonl"),
		filepath.Join(dir, "audit-20261019.jsonl"),
		filepath.Join(dir, "audit-20261020.jsonl"),
	}
	if len(files) != len(want) {
		t.Fatalf("got %v want %v", files, want)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Fatalf("order: got %v want %v", files, want)
		}
	}
}

// TestPruneOldFilesKeepsWindow T1/T3：窗口外分片删除、窗口内保留、
// legacy 与不可解析名永不删、keep=0 no-op。
func TestPruneOldFilesKeepsWindow(t *testing.T) {
	dir := t.TempDir()
	now := mkDate(2026, 10, 22)
	keep := 15 * 24 * time.Hour

	names := map[string]bool{
		"audit-20261006.jsonl": true,  // 16 天前 → 删
		"audit-20261007.jsonl": true,  // 15 天前（边界：早于窗口起点）→ 删
		"audit-20261008.jsonl": false, // 14 天前 → 留
		"audit-20261021.jsonl": false,
		"audit-20261022.jsonl": false, // 当天 → 留
		"audit.jsonl":          false, // legacy → 永不删
		"audit-weird.jsonl":    false, // 不可解析 → 跳过
		"audit.db":             false, // 非审计文件 → 不碰
	}
	for n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := PruneOldFiles(dir, keep, now)
	if err != nil {
		t.Fatalf("PruneOldFiles: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed=%v want 2 files", removed)
	}
	for n, wantGone := range names {
		_, statErr := os.Stat(filepath.Join(dir, n))
		exists := statErr == nil
		if exists == wantGone {
			t.Errorf("file %s exists=%v wantGone=%v mismatch", n, exists, wantGone)
		}
	}

	// keep=0：no-op（永久保留语义）
	if _, err := PruneOldFiles(dir, 0, now); err != nil {
		t.Fatalf("PruneOldFiles keep=0: %v", err)
	}
}

// TestFindOpenIntentsSpansFiles T4：崩溃残留的裸 intent 在文件 A、
// 跨天 result 在文件 B 闭环；聚合判定逐条正确。
// 悬案的正确产生方式：intent 落盘后进程崩溃（正常协议下 result 总在
// action 后落盘）——故用手动 append 模拟。
func TestFindOpenIntentsSpansFiles(t *testing.T) {
	dir := t.TempDir()

	// A 文件 = b 的完整事务 + a 的裸 intent（append 模拟崩溃）
	ja, err := OpenJSONL(filepath.Join(dir, "audit-20261020.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	_ = ja.CommitMoves([]MoveIntent{
		{RunID: "r1", PolicyID: "p", Src: dir + "/b", Trash: dir + "/t/b"},
	}, func() []error { return []error{nil} })
	ja.Close()
	fa, err := os.OpenFile(filepath.Join(dir, "audit-20261020.jsonl"), os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(fa, `{"ts":1,"run_id":"r1","policy_id":"p","type":"intent","src":%q,"trash":%q}`+"\n", dir+"/a", dir+"/t/a")
	fa.Close()

	open, err := FindOpenIntentsAll(dir)
	if err != nil {
		t.Fatalf("FindOpenIntentsAll: %v", err)
	}
	if len(open) != 1 || open[0].Src != dir+"/a" {
		t.Fatalf("open=%v want exactly [a]", open)
	}

	// 文件 B（次日）：跨天闭环 a → 悬案消失
	jb2, err := OpenJSONL(filepath.Join(dir, "audit-20261021.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	_ = jb2.CommitMoves([]MoveIntent{
		{RunID: "r1", PolicyID: "p", Src: dir + "/a", Trash: dir + "/t/a"},
	}, func() []error { return []error{nil} })
	jb2.Close()

	open2, err := FindOpenIntentsAll(dir)
	if err != nil {
		t.Fatalf("FindOpenIntentsAll after close: %v", err)
	}
	if len(open2) != 0 {
		t.Fatalf("open=%v want empty after cross-day result", open2)
	}
}

// TestImportJSONLMultiFileIdempotent T5：两个分片含逐字节相同的完整事务
// （真实场景：轮转边界复制/重放），聚合导入条数不翻倍——唯一键
// (ts,run_id,type,src) 决定幂等边界：ts 不同的事件本就是不同事件。
func TestImportJSONLMultiFileIdempotent(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJSONL(filepath.Join(dir, "audit-20261020.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	_ = j.CommitMoves([]MoveIntent{
		{RunID: "r1", PolicyID: "p", Src: dir + "/a", Trash: dir + "/t/a"},
	}, func() []error { return []error{nil} })
	j.Close()

	// 分片 B = 分片 A 的逐字节副本（同 ts 同 run 同 type 同 src）
	b, err := os.ReadFile(filepath.Join(dir, "audit-20261020.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audit-20261021.jsonl"), b, 0o640); err != nil {
		t.Fatal(err)
	}

	st, err := OpenStore(filepath.Join(dir, "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var total int64
	files, err := AuditFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		n, err := st.ImportJSONL(f)
		if err != nil {
			t.Fatalf("import %s: %v", f, err)
		}
		total += n
	}
	if total != 2 { // intent + result 各一条（B 的重复被唯一键吞掉）
		t.Fatalf("imported=%d want 2 (idempotent across identical files)", total)
	}
}
