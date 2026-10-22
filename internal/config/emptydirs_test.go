package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEmptyDirsConfigRoundTrip 验证 empty_dirs 段的解析：
// enabled+retention 正确落结构；缺省时零值（功能关）。
func TestEmptyDirsConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	yaml := `
state_dir: /tmp/fc
policies:
  - id: p1
    root: /tmp/fc/root
    schedule: "0 2 * * *"
    empty_dirs:
      enabled: true
      retention: 7d
    retention:
      match: AND
      conditions:
        mtime_older_than: 7d
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Policies) != 1 {
		t.Fatalf("policies=%d", len(cfg.Policies))
	}
	p := cfg.Policies[0]
	if !p.EmptyDirs.Enabled {
		t.Error("empty_dirs.enabled want true")
	}
	d, err := ParseDuration(p.EmptyDirs.Retention)
	if err != nil {
		t.Fatalf("retention parse: %v", err)
	}
	if d != 7*24*3600e9 {
		t.Errorf("retention=%v want 7d", d)
	}
}

// TestEmptyDirsOmittedByDefault 不配 empty_dirs 段时零值，向后兼容。
func TestEmptyDirsOmittedByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	yaml := `
state_dir: /tmp/fc
policies:
  - id: p1
    root: /tmp/fc/root
    schedule: "0 2 * * *"
    retention:
      conditions:
        mtime_older_than: 7d
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Policies[0].EmptyDirs.Enabled {
		t.Error("empty_dirs.enabled want false by default")
	}
}

// TestEmptyDirsValidateRequiresRetention enabled 但 retention 缺失/非法 →
// Validate 报错（fail-closed：年龄门是防误删的核心，不允许无门运行）。
func TestEmptyDirsValidateRequiresRetention(t *testing.T) {
	cases := []struct {
		name      string
		retention string
		wantErr   bool
	}{
		{"empty retention", "", true},
		{"invalid retention", "7x", true},
		{"valid", "7d", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Policy{
				ID:       "p1",
				Root:     "/tmp/fc/root",
				Schedule: "0 2 * * *",
				Retention: Retention{
					Conditions: map[string]string{"mtime_older_than": "7d"},
				},
				EmptyDirs: EmptyDirs{
					Enabled:   true,
					Retention: tc.retention,
				},
			}
			cfg := &Config{StateDir: "/tmp/fc", Policies: []Policy{p}}
			errs := Validate(cfg)
			got := len(errs) > 0
			if got != tc.wantErr {
				t.Errorf("Validate errs=%v want err=%v (errs: %v)", errs, tc.wantErr, errs)
			}
		})
	}
}
