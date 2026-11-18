package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// streamingYaml 基准配置：mode=streaming + 窗口 + checkpoint_dir。
const streamingYaml = `
state_dir: /tmp/fc
policies:
  - id: mega
    root: /tmp/fc/temp
    schedule: "0 1 * * *"
    empty_dirs:
      enabled: true
      retention: 7d
      mode: streaming
      window: "01:00-07:00"
      checkpoint_dir: /tmp/fc/ck
    retention:
      conditions:
        mtime_older_than: 7d
`

// TestEmptyDirsStreamingConfigRoundTrip streaming 段解析：mode/window/
// checkpoint_dir 落结构；window 解析出起止分钟。
func TestEmptyDirsStreamingConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(streamingYaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	e := cfg.Policies[0].EmptyDirs
	if e.Mode != "streaming" {
		t.Errorf("mode=%q want streaming", e.Mode)
	}
	if e.Window != "01:00-07:00" {
		t.Errorf("window=%q", e.Window)
	}
	if e.CheckpointDir != "/tmp/fc/ck" {
		t.Errorf("checkpoint_dir=%q", e.CheckpointDir)
	}
	start, end, err := ParseWindow(e.Window)
	if err != nil {
		t.Fatalf("ParseWindow: %v", err)
	}
	if start != 60 || end != 420 {
		t.Errorf("window minutes = %d-%d, want 60-420", start, end)
	}
}

// TestParseWindowCases 窗口解析：正常/跨天/非法格式/越界值。
func TestParseWindowCases(t *testing.T) {
	cases := []struct {
		in       string
		s, e     int
		wantErr  bool
	}{
		{"01:00-07:00", 60, 420, false},
		{"23:00-06:00", 1380, 360, false}, // 跨天：start > end 合法
		{"00:00-24:00", 0, 1440, false},
		{"1:00-7:00", 0, 0, true},   // 非 HH:MM
		{"25:00-26:00", 0, 0, true}, // 越界小时
		{"01:61-02:00", 0, 0, true}, // 越界分钟
		{"0100-0700", 0, 0, true},   // 缺冒号
		{"", 0, 0, true},            // 空串
	}
	for _, tc := range cases {
		s, e, err := ParseWindow(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseWindow(%q): want err, got %d-%d", tc.in, s, e)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseWindow(%q): %v", tc.in, err)
			continue
		}
		if s != tc.s || e != tc.e {
			t.Errorf("ParseWindow(%q) = %d-%d, want %d-%d", tc.in, s, e, tc.s, tc.e)
		}
	}
}

// TestInWindow 窗口判定：含边界、跨天窗口、窗口外。
func TestInWindow(t *testing.T) {
	mk := func(h, m int) time.Time { return time.Date(2026, 11, 18, h, m, 0, 0, time.Local) }
	cases := []struct {
		window string
		at     time.Time
		in     bool
	}{
		{"01:00-07:00", mk(1, 0), true},   // 起点含
		{"01:00-07:00", mk(3, 30), true},
		{"01:00-07:00", mk(7, 0), false},  // 终点不含（07:00 后必须停）
		{"01:00-07:00", mk(0, 59), false},
		{"01:00-07:00", mk(12, 0), false},
		{"23:00-06:00", mk(23, 30), true}, // 跨天
		{"23:00-06:00", mk(2, 0), true},
		{"23:00-06:00", mk(12, 0), false},
		{"23:00-06:00", mk(23, 0), true},
		{"23:00-06:00", mk(6, 0), false},
	}
	for _, tc := range cases {
		in, err := InWindow(tc.window, tc.at)
		if err != nil {
			t.Fatalf("InWindow(%q): %v", tc.window, err)
		}
		if in != tc.in {
			t.Errorf("InWindow(%q, %s) = %v, want %v", tc.window, tc.at.Format("15:04"), in, tc.in)
		}
	}
}

// TestValidateStreamingRequiresFields mode=streaming 时的必填校验：
// window 必填且合法；checkpoint_dir 必填（无 checkpoint 的 streaming
// 断点不可续，违反长跑设计）。
func TestValidateStreamingRequiresFields(t *testing.T) {
	base := func() Policy {
		return Policy{
			ID:       "mega",
			Root:     "/tmp/fc/temp",
			Schedule: "0 1 * * *",
			Retention: Retention{Conditions: map[string]string{"mtime_older_than": "7d"}},
			EmptyDirs: EmptyDirs{
				Enabled:   true,
				Retention: "7d",
				Mode:      "streaming",
			},
		}
	}
	cases := []struct {
		name string
		mut  func(*Policy)
		err  bool
	}{
		{"no window", func(p *Policy) {}, true},
		{"bad window", func(p *Policy) { p.EmptyDirs.Window = "9:00" }, true},
		{"no checkpoint_dir", func(p *Policy) { p.EmptyDirs.Window = "01:00-07:00" }, true},
		{"valid", func(p *Policy) {
			p.EmptyDirs.Window = "01:00-07:00"
			p.EmptyDirs.CheckpointDir = "/tmp/fc/ck"
		}, false},
		{"legacy mode ignores window", func(p *Policy) {
			p.EmptyDirs.Mode = ""
			p.EmptyDirs.Window = "9:00" // legacy 模式不校验 window
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base()
			tc.mut(&p)
			cfg := &Config{StateDir: "/tmp/fc", Policies: []Policy{p}}
			errs := Validate(cfg)
			got := len(errs) > 0
			if got != tc.err {
				t.Errorf("errs=%v want err=%v (errs: %v)", errs, tc.err, errs)
			}
		})
	}
}

// TestValidateStreamingBadMode 未知 mode 值拒绝（只认空=legacy/streaming）。
func TestValidateStreamingBadMode(t *testing.T) {
	p := Policy{
		ID:       "mega",
		Root:     "/tmp/fc/temp",
		Schedule: "0 1 * * *",
		Retention: Retention{Conditions: map[string]string{"mtime_older_than": "7d"}},
		EmptyDirs: EmptyDirs{
			Enabled:    true,
			Retention:  "7d",
			Mode:       "turbo",
			Window:     "01:00-07:00",
			CheckpointDir: "/tmp/fc/ck",
		},
	}
	cfg := &Config{StateDir: "/tmp/fc", Policies: []Policy{p}}
	errs := Validate(cfg)
	if len(errs) == 0 {
		t.Fatal("Validate must reject unknown mode")
	}
}
