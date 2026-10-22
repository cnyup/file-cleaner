package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntentMustPersistBeforeAction 验证批量事务协议：
// CommitMoves 回调执行前，intent 必须已 append + fsync 到 JSONL。
func TestIntentMustPersistBeforeAction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	jl, err := OpenJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	defer jl.Close()

	var intentSeen []string
	err = jl.CommitMoves([]MoveIntent{
		{RunID: "r1", PolicyID: "p1", Src: "/a/f1", Trash: "/t/f1", Inode: 1},
		{RunID: "r1", PolicyID: "p1", Src: "/a/f2", Trash: "/t/f2", Inode: 2},
	}, func() []error {
		// 执行动作的瞬间检查 intent 是否已在盘上
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("action before intent persisted: %v", err)
		}
		if len(data) == 0 {
			t.Fatal("action executed but intent not yet on disk")
		}
		intentSeen = strings.Split(strings.TrimSpace(string(data)), "\n")
		return nil
	})
	if err != nil {
		t.Fatalf("CommitMoves: %v", err)
	}
	if len(intentSeen) != 2 {
		t.Fatalf("intents on disk at action time = %d, want 2", len(intentSeen))
	}
}

// TestCommitMovesWritesResults 动作完成后 result 事件必须落盘。
func TestCommitMovesWritesResults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	jl, _ := OpenJSONL(path)
	defer jl.Close()

	err := jl.CommitMoves([]MoveIntent{
		{RunID: "r1", PolicyID: "p1", Src: "/a/f1", Trash: "/t/f1", Inode: 1},
	}, func() []error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	jl.Sync()

	f, _ := os.Open(path)
	defer f.Close()
	var events []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			events = append(events, e)
		}
	}
	if len(events) != 2 {
		t.Fatalf("events=%d want 2 (intent+result)", len(events))
	}
	if events[0].Type != EventIntent || events[1].Type != EventResult {
		t.Fatalf("order wrong: %v then %v", events[0].Type, events[1].Type)
	}
}

// TestAuditUnavailableBlocksMoves 审计不可写时 CommitMoves 必须拒绝执行动作。
func TestAuditUnavailableBlocksMoves(t *testing.T) {
	dir := t.TempDir()
	// 用一个只读文件模拟磁盘满/不可写
	path := filepath.Join(dir, "audit.jsonl")
	os.WriteFile(path, []byte(""), 0o444)
	jl, err := OpenJSONL(path)
	if err == nil {
		called := false
		_ = jl.CommitMoves([]MoveIntent{{RunID: "r", PolicyID: "p", Src: "/a", Trash: "/t"}}, func() []error {
			called = true
			return nil
		})
		if called {
			t.Fatal("action must NOT run when audit is unwritable")
		}
		jl.Close()
	}
	// OpenJSONL 直接失败同样可接受（fail-closed 的另一形态）
}

// TestRecoverOpenIntents 启动恢复：intent 无 result → 需人工/核对处理，
// Recovery 必须列出未闭环条目。
func TestRecoverOpenIntents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	// 手工构造：一条闭环（intent+result），一条开环（只有 intent）
	lines := []string{
		`{"ts":1,"run_id":"r1","policy_id":"p1","type":"intent","src":"/a/f1","trash":"/t/f1"}`,
		`{"ts":2,"run_id":"r1","policy_id":"p1","type":"result","src":"/a/f1","trash":"/t/f1","ok":true}`,
		`{"ts":3,"run_id":"r1","policy_id":"p1","type":"intent","src":"/a/f2","trash":"/t/f2"}`,
	}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)

	open, err := FindOpenIntents(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Src != "/a/f2" {
		t.Fatalf("open intents = %+v, want [f2]", open)
	}
}

// TestAppendSkippedAgg 跳过聚合与 run 边界事件批量写入。
func TestAppendSkippedAgg(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	jl, _ := OpenJSONL(path)
	defer jl.Close()

	jl.RunStarted("r1", "p1", "hash1")
	jl.AppendSkippedAgg("r1", "p1", map[string]int64{"exclude_pattern": 10, "not_matched": 5})
	jl.RunFinished("r1", "p1", map[string]int64{"moved": 3})
	jl.Sync()

	data, _ := os.ReadFile(path)
	nl := strings.Count(string(data), "\n")
	if nl != 3 {
		t.Fatalf("lines=%d want 3", nl)
	}
	if !strings.Contains(string(data), "run_started") || !strings.Contains(string(data), "run_finished") {
		t.Fatal("run boundary events missing")
	}
}
