package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadMinimalConfig 验证最小合法配置能被加载。
// Wave 0 骨架测试：完整字段与校验矩阵在 Wave 1 任务 1.1 补全。
func TestLoadMinimalConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "state_dir: /var/lib/filecleaner\npolicies: []\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load minimal config: %v", err)
	}
	if cfg.StateDir != "/var/lib/filecleaner" {
		t.Fatalf("StateDir = %q, want %q", cfg.StateDir, "/var/lib/filecleaner")
	}
	if len(cfg.Policies) != 0 {
		t.Fatalf("Policies len = %d, want 0", len(cfg.Policies))
	}
}

// TestLoadMissingFile 验证配置文件不存在时返回错误而非零值配置。
func TestLoadMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	if _, err := Load(missing); err == nil {
		t.Fatal("Load missing file: want error, got nil")
	}
}
