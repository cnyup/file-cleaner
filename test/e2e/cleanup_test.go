//go:build linux || darwin

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// buildBin 编译当前源码为测试二进制。
func buildBin(t *testing.T) string {
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

// makeTree 构建测试树。
func makeTree(t *testing.T, root string) {
	t.Helper()
	old := time.Now().Add(-48 * time.Hour)
	new := time.Now().Add(-30 * time.Minute)

	files := map[string]time.Time{
		"a/old1.txt":      old,
		"a/old2.txt":      old,
		"b/new1.txt":      new,
		"keep.log":        old,
		"excl/skipme.txt": old,
		"deep/x/old3.txt": old,
	}
	for rel, ts := range files {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("content-"+rel), 0o644)
		os.Chtimes(p, ts, ts)
	}
	// 符号链接
	os.Symlink(filepath.Join(root, "b"), filepath.Join(root, "blink"))
	// 刚被修改过的旧文件（mtime 老 + ctime 新 → 静默期应拦截）：
	// ctime 无法直接设置，通过 chmod 触发
	p := filepath.Join(root, "a/touched.txt")
	os.WriteFile(p, []byte("recently-touched"), 0o644)
	os.Chtimes(p, old, old)
	os.Chmod(p, 0o644) // 触发 ctime=now
}

func TestFullPipelineOnce(t *testing.T) {
	bin := buildBin(t)
	base := t.TempDir()
	root := filepath.Join(base, "data")
	state := filepath.Join(base, "state")
	os.MkdirAll(root, 0o755)
	makeTree(t, root)

	cfg := filepath.Join(base, "config.yaml")
	os.WriteFile(cfg, []byte(`
state_dir: `+state+`
policies:
  - id: e2e
    root: `+root+`
    retention:
      conditions:
        mtime_older_than: 24h
      # min_quiet_period 语义（mtime 老 + ctime 新 → 拦截）在 policy 单测覆盖；
      # E2E 树的文件 ctime 均为创建时刻，无法伪造过去 ctime，故此处不启用。
    exclusions:
      dirs: [excl]
      patterns: ["*.log"]
    trash:
      retention: 1h
    concurrency:
      stat_workers: 4
      move_workers: 2
`), 0o644)

	// 1) dry-run：不移动
	out, err := exec.Command(bin, "once", "--config", cfg, "--policy", "e2e", "--dry-run").CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(root, "a/old1.txt")); err != nil {
		t.Fatal("dry-run must not move files")
	}

	// 2) 实际执行
	out, err = exec.Command(bin, "once", "--config", cfg, "--policy", "e2e").CombinedOutput()
	if err != nil {
		t.Fatalf("once: %v\n%s", err, out)
	}

	// P0-2 回归：stats.moved 必须是物理口径——命中 4 个（old1/old2/
	// old3 + touched，无静默期时仅按 mtime 判定）全部物理进桶，
	// JSON 输出的 MovedOK 必须等于物理数与 Matched（全成功路径锚定）。
	var runStats struct {
		MovedOK    int64 `json:"MovedOK"`
		MoveErrors int64 `json:"MoveErrors"`
		Matched    int64 `json:"Matched"`
	}
	if err := json.Unmarshal(out, &runStats); err != nil {
		t.Fatalf("once output not stats JSON: %v\n%s", err, out)
	}
	if runStats.MovedOK != 4 || runStats.MoveErrors != 0 || runStats.MovedOK != runStats.Matched {
		t.Errorf("stats physical semantics: MovedOK=%d MoveErrors=%d Matched=%d, want 4/0/4\nraw: %s", runStats.MovedOK, runStats.MoveErrors, runStats.Matched, out)
	}

	// 断言：old1/old2/old3 移入回收站（桶语义：<trash>/<date>.run-*/<rel>）
	trash := filepath.Join(root, ".filecleaner-trash")
	findInBuckets := func(rel string) string {
		entries, _ := os.ReadDir(trash)
		for _, e := range entries {
			if !e.IsDir() || e.Name()[0] == '.' {
				continue
			}
			p := filepath.Join(trash, e.Name(), rel)
			if _, err := os.Lstat(p); err == nil {
				return p
			}
		}
		return ""
	}
	for _, rel := range []string{"a/old1.txt", "a/old2.txt", "deep/x/old3.txt"} {
		if findInBuckets(rel) == "" {
			t.Errorf("expected in trash bucket: %s", rel)
		}
		if _, err := os.Lstat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("src must be gone: %s", rel)
		}
	}
	// 断言：new/排除目录/排除 pattern/符号链接 保留原位
	// （a/touched.txt 会被正常清理：无静默期时仅按 mtime 判定）
	for _, rel := range []string{"b/new1.txt", "keep.log", "excl/skipme.txt", "blink"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			t.Errorf("must remain in place: %s (%v)", rel, err)
		}
	}

	// 3) 恢复
	out, err = exec.Command(bin, "restore", "--config", cfg, "--policy", "e2e", "--path", "a/old1.txt").CombinedOutput()
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(filepath.Join(root, "a/old1.txt")); err != nil || string(b) != "content-a/old1.txt" {
		t.Errorf("restored content mismatch: %q %v", b, err)
	}

	// 4) 恢复冲突：原路径已有新文件 → 拒绝
	os.WriteFile(filepath.Join(root, "a/old2.txt"), []byte("new-file"), 0o644)
	out, err = exec.Command(bin, "restore", "--config", cfg, "--policy", "e2e", "--path", "a/old2.txt").CombinedOutput()
	if err == nil {
		t.Fatal("restore onto existing must fail")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a/old2.txt")); string(b) != "new-file" {
		t.Fatal("existing file must not be overwritten")
	}

	// 5) purge（桶语义：retention=1h，今天的桶未到期 → 不 retire、不删）
	out, err = exec.Command(bin, "purge", "--config", cfg, "--policy", "e2e").CombinedOutput()
	if err != nil {
		t.Fatalf("purge: %v\n%s", err, out)
	}
	if findInBuckets("deep/x/old3.txt") == "" {
		t.Fatal("unexpired bucket must survive purge")
	}

	// 6) 审计可查
	out, err = exec.Command(bin, "audit", "query", "--config", cfg, "--policy", "e2e").CombinedOutput()
	if err != nil {
		t.Fatalf("audit query: %v\n%s", err, out)
	}
	if len(out) == 0 {
		t.Fatal("audit query returned nothing")
	}
}
