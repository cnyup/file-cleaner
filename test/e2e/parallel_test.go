//go:build linux || darwin

package e2e

// 并行调度 e2e（Wave 3 T5）：两策略两 root、同分钟 cron、真 daemon
// 子进程 + run_on_start——验证多策略并行执行下：
//  1. 两个策略各自完成 run_finished（on-start 并行补欠账）
//  2. 审计 JSONL 全部行可解析（投影 rebuild 成功——并发写无坏行）
//  3. 两策略的文件互不干扰（各自进各自的回收站桶）
import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestParallelPoliciesTwoRoots 两策略并行 on-start：各自清理各自的树。
func TestParallelPoliciesTwoRoots(t *testing.T) {
	bin := buildBin(t)
	dir := t.TempDir()
	rootA := filepath.Join(dir, "nasa")
	rootB := filepath.Join(dir, "nasb")
	state := filepath.Join(dir, "state")
	for _, r := range []string{rootA, rootB} {
		makeTree(t, r)
		os.MkdirAll(state, 0o755)
	}

	cfg := `
state_dir: ` + state + `
audit_log: {rotation: off}
max_concurrent_runs: 2
run_on_start: true
policies:
  - id: pol-a
    root: ` + rootA + `
    schedule: "0 3 31 2 *"
    retention:
      conditions:
        mtime_older_than: 1d
    exclusions:
      patterns: ["*.log"]
      dirs: ["excl"]
    trash:
      retention: 10d
  - id: pol-b
    root: ` + rootB + `
    schedule: "0 3 31 2 *"
    retention:
      conditions:
        mtime_older_than: 1d
    exclusions:
      patterns: ["*.log"]
      dirs: ["excl"]
    trash:
      retention: 10d
`
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// daemon：on-start 并行两轮后，因 schedule 永不触发（2月31日），
	// 主循环进入 idling——给足时间跑完两轮后杀进程。
	ctxDone := make(chan struct{})
	cmd := exec.Command(bin, "daemon", "--config", cfgPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	go func() {
		time.Sleep(6 * time.Second)
		cmd.Process.Kill()
		close(ctxDone)
	}()
	cmd.Wait()
	<-ctxDone

	// 1) 两个策略各自 run_finished
	auditPath := filepath.Join(state, "audit.jsonl")
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	finished := map[string]bool{}
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var ev map[string]any
		if err := dec.Decode(&ev); err != nil {
			break
		}
		if ev["type"] == "run_finished" {
			finished[ev["policy_id"].(string)] = true
		}
	}
	if !finished["pol-a"] || !finished["pol-b"] {
		t.Fatalf("run_finished policies = %v, want both pol-a and pol-b", finished)
	}

	// 2) 投影 rebuild 成功（audit 全行可解析 + SQLite 重建）
	out, err := exec.Command(bin, "audit", "rebuild", "--config", cfgPath).CombinedOutput()
	if err != nil {
		t.Fatalf("audit rebuild: %v\n%s", err, out)
	}

	// 3) 各自的旧文件进了各自的回收站，新文件与排除项原位
	for _, root := range []string{rootA, rootB} {
		if _, err := os.Stat(filepath.Join(root, "a", "old1.txt")); !os.IsNotExist(err) {
			t.Fatalf("%s: a/old1.txt should be moved to trash", root)
		}
		if _, err := os.Stat(filepath.Join(root, "b", "new1.txt")); err != nil {
			t.Fatalf("%s: b/new1.txt must stay", root)
		}
		if _, err := os.Stat(filepath.Join(root, "keep.log")); err != nil {
			t.Fatalf("%s: keep.log (excluded) must stay", root)
		}
		trashA := filepath.Join(root, ".filecleaner-trash")
		entries, _ := os.ReadDir(trashA)
		if len(entries) == 0 {
			t.Fatalf("%s: trash should contain buckets", root)
		}
	}
}
