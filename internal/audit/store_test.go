package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func setupStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "audit.jsonl")
	dbPath := filepath.Join(dir, "audit.db")
	jl, err := OpenJSONL(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jl.Close() })
	st, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, jsonlPath, dbPath
}

func TestImportIdempotent(t *testing.T) {
	st, jsonlPath, _ := setupStore(t)
	jl, _ := OpenJSONL(jsonlPath)
	defer jl.Close()

	// 写 2 条闭环事件
	_ = jl.CommitMoves([]MoveIntent{
		{RunID: "r1", PolicyID: "p1", Src: "/a/f1", Trash: "/t/f1", Inode: 1},
	}, func() []error { return nil })

	// 导入两次，必须幂等
	for i := 0; i < 2; i++ {
		n, err := st.ImportJSONL(jsonlPath)
		if err != nil {
			t.Fatalf("import #%d: %v", i+1, err)
		}
		_ = n
	}
	rows, err := st.QueryMoved("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Src != "/a/f1" {
		t.Fatalf("moved rows = %+v", rows)
	}
}

func TestStateTransitions(t *testing.T) {
	st, jsonlPath, _ := setupStore(t)
	jl, _ := OpenJSONL(jsonlPath)
	defer jl.Close()

	// moved → restored → moved → purged 全生命周期
	_ = jl.CommitMoves([]MoveIntent{{RunID: "r1", PolicyID: "p1", Src: "/a/f1", Trash: "/t/f1"}}, func() []error { return nil })
	_ = jl.CommitRestore("r2", "p1", "/a/f1", "/t/f1")
	_ = jl.WriteRestoreResult("r2", "p1", "/t/f1", "/a/f1", true, "")
	_ = jl.CommitMoves([]MoveIntent{{RunID: "r3", PolicyID: "p1", Src: "/a/f1", Trash: "/t/f1"}}, func() []error { return nil })
	_ = jl.CommitPurge("r4", "p1", "/t/f1")
	_ = jl.WritePurgeResult("r4", "p1", "/t/f1", true, "")

	if _, err := st.ImportJSONL(jsonlPath); err != nil {
		t.Fatal(err)
	}
	state, err := st.FileState("/t/f1")
	if err != nil {
		t.Fatal(err)
	}
	if state != "purged" {
		t.Fatalf("final state = %q, want purged", state)
	}
}

func TestOpenIntentProjected(t *testing.T) {
	st, jsonlPath, _ := setupStore(t)
	// 只有 intent 无 result
	os.WriteFile(jsonlPath, []byte(`{"ts":1,"run_id":"r1","policy_id":"p1","type":"intent","src":"/a/f9","trash":"/t/f9"}
`), 0o640)
	if _, err := st.ImportJSONL(jsonlPath); err != nil {
		t.Fatal(err)
	}
	open, err := st.OpenIntents()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Src != "/a/f9" {
		t.Fatalf("open intents = %+v", open)
	}
}

func TestTruncatedTailLine(t *testing.T) {
	st, jsonlPath, _ := setupStore(t)
	os.WriteFile(jsonlPath, []byte(
		`{"ts":1,"run_id":"r1","policy_id":"p1","type":"intent","src":"/a/f1","trash":"/t/f1"}`+"\n"+
			`{"ts":2,"run_id":"r1","policy_id":"p1","type":"result","src":"/a/f1","trash":"/t"`), 0o640) // 截断
	n, err := st.ImportJSONL(jsonlPath)
	if err != nil {
		t.Fatalf("import with truncated tail must not fail: %v", err)
	}
	if n != 1 {
		t.Fatalf("imported = %d, want 1 (truncated line skipped)", n)
	}
}
