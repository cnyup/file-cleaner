package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestCommitRmdirsTransaction rmdir 事务协议：intent 批落盘 → action →
// result per-dir 按物理结果落盘（部分失败不误标整批）。
func TestCommitRmdirsTransaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	jl, err := OpenJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	defer jl.Close()

	intents := []RmdirIntent{
		{RunID: "r1", PolicyID: "p1", Path: "/nas/a/b/c"},
		{RunID: "r1", PolicyID: "p1", Path: "/nas/a/b"},
	}
	var physical []string
	err = jl.CommitRmdirs(intents, func() []error {
		physical = append(physical, "c-deleted", "b-failed")
		return []error{nil, os.ErrPermission}
	})
	if err == nil {
		t.Fatal("first non-nil error must be returned as aggregate")
	}

	f, _ := os.Open(path)
	defer f.Close()
	sc := bufio.NewScanner(f)
	var evs []Event
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
	}
	if len(evs) != 4 {
		t.Fatalf("events=%d want 4 (2 intent + 2 result)", len(evs))
	}
	if evs[0].Type != "rmdir_intent" || evs[1].Type != "rmdir_intent" {
		t.Fatalf("intent types: %s %s", evs[0].Type, evs[1].Type)
	}
	if evs[2].Type != "rmdir_result" || *evs[2].OK != true {
		t.Fatalf("c result: %s ok=%v want rmdir_result/true", evs[2].Type, evs[2].OK)
	}
	if evs[3].Type != "rmdir_result" || *evs[3].OK != false {
		t.Fatalf("b result: %s ok=%v want rmdir_result/false (partial failure not masked)", evs[3].Type, evs[3].OK)
	}
	if evs[3].Err == "" {
		t.Error("failed result must carry err")
	}
}

// TestCommitRmdirsEmptyBatch 空批无操作、无落盘。
func TestCommitRmdirsEmptyBatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	jl, _ := OpenJSONL(path)
	defer jl.Close()
	if err := jl.CommitRmdirs(nil, func() []error { return nil }); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		// 文件可能被 Open 创建；检查内容为空即可
		fi, _ := os.Stat(path)
		if fi != nil && fi.Size() != 0 {
			t.Fatalf("file size=%d want 0", fi.Size())
		}
	}
}
