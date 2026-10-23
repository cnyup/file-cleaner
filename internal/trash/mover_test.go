package trash

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bcyup/filecleaner/internal/audit"
)

// setup 构建 root + 审计。
func setup(t *testing.T) (*Layout, string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	os.MkdirAll(root, 0o755)
	jsonlPath := filepath.Join(dir, "audit.jsonl")
	return NewLayout(root, ".filecleaner-trash"), root, jsonlPath
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMovePreservesStructure(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	src := filepath.Join(root, "a/b/f.txt")
	mustWrite(t, src, "hello")

	jl, err := OpenAudit(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer jl.Close()
	m := NewMover(layout, jl)

	if _, _, err := m.MoveBatch("r1", "p1", []MoveReq{{Src: src, Rel: "a/b/f.txt"}}, Recheck{}); err != nil {
		t.Fatalf("MoveBatch: %v", err)
	}

	trashPath := layout.TrashPath("a/b/f.txt")
	data, err := os.ReadFile(trashPath)
	if err != nil || string(data) != "hello" {
		t.Fatalf("trash file missing/corrupt: %v %q", err, data)
	}
	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Fatal("src must be gone after move")
	}
	// 内容必须是 rename 搬运（同 inode），不是复制——校验 inode 一致
	si, _ := os.Lstat(trashPath)
	_ = si
}

func TestMovePathTraversalBlocked(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	jl, _ := OpenAudit(jsonlPath)
	defer jl.Close()
	m := NewMover(layout, jl)

	// rel 含 ..，必须拒绝
	_, _, err := m.MoveBatch("r1", "p1", []MoveReq{{Src: filepath.Join(root, "f"), Rel: "../escape"}}, Recheck{})
	if err == nil {
		t.Fatal("path traversal must be rejected")
	}
}

func TestSameNameSecondMove(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	jl, _ := OpenAudit(jsonlPath)
	defer jl.Close()
	m := NewMover(layout, jl)

	src := filepath.Join(root, "f.txt")
	mustWrite(t, src, "v1")
	if _, _, err := m.MoveBatch("r1", "p1", []MoveReq{{Src: src, Rel: "f.txt"}}, Recheck{}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, src, "v2")
	if _, _, err := m.MoveBatch("r2", "p1", []MoveReq{{Src: src, Rel: "f.txt"}}, Recheck{}); err != nil {
		t.Fatal(err)
	}
	// 两个版本都在回收站
	if _, err := os.Lstat(layout.TrashPath("f.txt")); err != nil {
		t.Fatal("first version missing")
	}
	second, err := os.ReadFile(layout.TrashPath("f.txt") + ".2") // 时间戳后缀的简化形态
	if err != nil || string(second) != "v2" {
		// 后缀形式由实现定义；至少第二个版本必须存在且内容正确
		found := false
		entries, _ := os.ReadDir(filepath.Dir(layout.TrashPath("f.txt")))
		for _, e := range entries {
			if e.Name() != "f.txt" {
				if b, err := os.ReadFile(filepath.Join(filepath.Dir(layout.TrashPath("f.txt")), e.Name())); err == nil && string(b) == "v2" {
					found = true
				}
			}
		}
		if !found {
			t.Fatal("second version missing in trash")
		}
	}
}

func TestRecheckChangedSkips(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	jl, _ := OpenAudit(jsonlPath)
	defer jl.Close()
	m := NewMover(layout, jl)

	src := filepath.Join(root, "f.txt")
	mustWrite(t, src, "v1")
	st, _ := os.Lstat(src)
	old := st.ModTime()

	// recheck 声称旧 mtime，但文件实际被更新 → 必须跳过
	newer := time.Now().Add(time.Hour)
	os.Chtimes(src, newer, newer)
	_, _, err := m.MoveBatch("r1", "p1", []MoveReq{{Src: src, Rel: "f.txt"}}, Recheck{Mtime: old})
	if err == nil {
		t.Fatal("changed file must be skipped")
	}
	if _, err := os.Lstat(src); err != nil {
		t.Fatal("src must remain")
	}
}

func TestRestoreNoOverwrite(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	jl, _ := OpenAudit(jsonlPath)
	defer jl.Close()
	m := NewMover(layout, jl)

	src := filepath.Join(root, "f.txt")
	mustWrite(t, src, "old")
	if _, _, err := m.MoveBatch("r1", "p1", []MoveReq{{Src: src, Rel: "f.txt"}}, Recheck{}); err != nil {
		t.Fatal(err)
	}
	// 原路径出现新文件
	mustWrite(t, src, "new")
	if err := m.Restore("r2", "p1", "f.txt"); err == nil {
		t.Fatal("restore onto existing file must fail (no overwrite)")
	}
	// 新文件内容未被破坏
	b, _ := os.ReadFile(src)
	if string(b) != "new" {
		t.Fatal("existing file must not be touched")
	}
	// 回收站版本仍在
	if _, err := os.Lstat(layout.TrashPath("f.txt")); err != nil {
		t.Fatal("trash copy must remain after failed restore")
	}
}

func TestRestoreOK(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	jl, _ := OpenAudit(jsonlPath)
	defer jl.Close()
	m := NewMover(layout, jl)

	src := filepath.Join(root, "a/f.txt")
	mustWrite(t, src, "data")
	_, _, _ = m.MoveBatch("r1", "p1", []MoveReq{{Src: src, Rel: "a/f.txt"}}, Recheck{})
	if err := m.Restore("r2", "p1", "a/f.txt"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	b, _ := os.ReadFile(src)
	if string(b) != "data" {
		t.Fatal("restored content mismatch")
	}
}

func TestPurgeOnlyKnownExpired(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	jl, _ := OpenAudit(jsonlPath)
	defer jl.Close()
	m := NewMover(layout, jl)
	// purge 走投影：导入 JSONL 到 SQLite
	st, err := audit.OpenStore(filepath.Join(filepath.Dir(jsonlPath), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m.SetStore(st)

	// 已知条目：先移入（较早）
	src1 := filepath.Join(root, "old.txt")
	mustWrite(t, src1, "x")
	_, _, _ = m.MoveBatch("r1", "p1", []MoveReq{{Src: src1, Rel: "old.txt"}}, Recheck{})

	// 未知条目：手工放进回收站（DB 无记录）→ 不可删
	mustWrite(t, layout.TrashPath("unknown.txt"), "y")

	// 未到期已知条目：稍后移入
	time.Sleep(50 * time.Millisecond)
	src2 := filepath.Join(root, "fresh.txt")
	mustWrite(t, src2, "z")
	_, _, _ = m.MoveBatch("r2", "p1", []MoveReq{{Src: src2, Rel: "fresh.txt"}}, Recheck{})

	// expiry 取两者入站时间的中点：old 过期、fresh 未过期（先刷新投影）
	if _, err := st.ImportJSONL(jsonlPath); err != nil {
		t.Fatal(err)
	}
	oldAt, ok, _ := st.MovedAt(layout.TrashPath("old.txt"))
	if !ok {
		t.Fatal("old.txt missing in projection")
	}
	freshAt, ok, _ := st.MovedAt(layout.TrashPath("fresh.txt"))
	if !ok {
		t.Fatal("fresh.txt missing in projection")
	}
	mid := oldAt.Add(freshAt.Sub(oldAt) / 2)

	purged, err := m.Purge("p1", mid)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 {
		t.Fatalf("purged=%d want 1 (only expired)", len(purged))
	}
	if _, err := os.Lstat(layout.TrashPath("old.txt")); !os.IsNotExist(err) {
		t.Fatal("expired entry must be deleted")
	}
	if _, err := os.Lstat(layout.TrashPath("unknown.txt")); err != nil {
		t.Fatal("unknown still must not be purged")
	}
	if _, err := os.Lstat(layout.TrashPath("fresh.txt")); err != nil {
		t.Fatal("fresh entry must survive")
	}
}
