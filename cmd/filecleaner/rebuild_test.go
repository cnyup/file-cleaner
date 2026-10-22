package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bcyup/filecleaner/internal/config"
	"github.com/bcyup/filecleaner/internal/runner"
)

// buildBinMain 编译当前源码为测试二进制（main 包测试版）。
func buildBinMain(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "filecleaner")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/bcyup/filecleaner/cmd/filecleaner")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// TestAuditRebuildCLI E2E：删掉投影 → rebuild 重建 → query 恢复；
// --check-open 列出未闭环 intent，人工闭环后不再列出。
func TestAuditRebuildCLI(t *testing.T) {
	bin := buildBinMain(t)
	base := t.TempDir()
	root := filepath.Join(base, "data")
	state := filepath.Join(base, "state")
	os.MkdirAll(root, 0o755)
	os.MkdirAll(state, 0o755)

	cfgPath := filepath.Join(base, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
state_dir: `+state+`
audit_log: {rotation: off}
policies:
  - id: rb
    root: `+root+`
    retention:
      conditions:
        mtime_older_than: 1d
    trash:
      retention: 1h
`), 0o644)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	pol := &cfg.Policies[0]

	// 手工写一条已闭环 moved 记录 + 一条未闭环 intent
	trashPath := filepath.Join(root, ".filecleaner-trash", "2026-09-28.run-x", "f.txt")
	os.MkdirAll(filepath.Dir(trashPath), 0o755)
	os.WriteFile(trashPath, []byte("data"), 0o644)
	auditPath := filepath.Join(state, "audit.jsonl")
	openRel := "2026-09-28.run-y/g.txt"
	openTrash := filepath.Join(root, ".filecleaner-trash", openRel)
	os.MkdirAll(filepath.Dir(openTrash), 0o755)
	os.WriteFile(openTrash, []byte("pending"), 0o644)
	lines := []string{
		`{"ts":1,"run_id":"r1","policy_id":"rb","type":"intent","src":"/data/f.txt","trash":"` + trashPath + `"}`,
		`{"ts":2,"run_id":"r1","policy_id":"rb","type":"result","src":"/data/f.txt","trash":"` + trashPath + `","ok":true}`,
		`{"ts":3,"run_id":"r2","policy_id":"rb","type":"intent","src":"/data/g.txt","trash":"` + openTrash + `"}`,
	}
	os.WriteFile(auditPath, []byte(strings.Join(lines, "\n")+"\n"), 0o640)

	// 先用 query 建出投影，然后删掉投影模拟损坏
	out, err := exec.Command(bin, "audit", "query", "--config", cfgPath, "--policy", "rb").CombinedOutput()
	if err != nil {
		t.Fatalf("audit query: %v\n%s", err, out)
	}
	for _, suffix := range []string{"audit.db", "audit.db-wal", "audit.db-shm"} {
		os.Remove(filepath.Join(state, suffix))
	}

	// 1) --check-open：列出未闭环 intent（g.txt），不含已闭环的 f.txt
	out, err = exec.Command(bin, "audit", "rebuild", "--config", cfgPath, "--check-open").CombinedOutput()
	if err != nil {
		t.Fatalf("rebuild --check-open: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), openTrash) {
		t.Fatalf("--check-open must list open intent %s, got:\n%s", openTrash, out)
	}
	if strings.Contains(string(out), trashPath) {
		t.Fatalf("--check-open must NOT list closed intent %s", trashPath)
	}

	// 2) 重建投影
	out, err = exec.Command(bin, "audit", "rebuild", "--config", cfgPath).CombinedOutput()
	if err != nil {
		t.Fatalf("rebuild: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "rebuilt") {
		t.Fatalf("rebuild output should report rebuild result:\n%s", out)
	}

	// 3) query 恢复且能查到 moved 记录
	out, err = exec.Command(bin, "audit", "query", "--config", cfgPath, "--policy", "rb").CombinedOutput()
	if err != nil {
		t.Fatalf("audit query after rebuild: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), trashPath) {
		t.Fatalf("query after rebuild must find moved record, got:\n%s", out)
	}

	// 4) 人工闭环未闭环 intent（补写 result）后 --check-open 为空
	lines = append(lines, `{"ts":4,"run_id":"r2","policy_id":"rb","type":"result","src":"/data/g.txt","trash":"`+openTrash+`","ok":true}`)
	os.WriteFile(auditPath, []byte(strings.Join(lines, "\n")+"\n"), 0o640)
	out, err = exec.Command(bin, "audit", "rebuild", "--config", cfgPath, "--check-open").CombinedOutput()
	if err != nil {
		t.Fatalf("rebuild --check-open after close: %v\n%s", err, out)
	}
	if strings.Contains(string(out), openTrash) {
		t.Fatalf("--check-open must be empty after closure, got:\n%s", out)
	}
	_ = pol
	_ = runner.AuditQuery
}
