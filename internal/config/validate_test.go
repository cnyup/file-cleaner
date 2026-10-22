package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCfg 写入临时配置文件并返回路径。
func writeCfg(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return p
}

func TestLoadUnknownField(t *testing.T) {
	p := writeCfg(t, "state_dir: /x\npolicies: []\nunknown_top: 1\n")
	if _, err := Load(p); err == nil {
		t.Fatal("unknown field must be rejected (KnownFields)")
	}
}

func TestValidateDuplicatePolicyID(t *testing.T) {
	p := writeCfg(t, `
state_dir: /tmp/fc-state
policies:
  - id: a
    root: /tmp/fc-root
    schedule: "30 2 * * *"
    retention:
      conditions:
        mtime_older_than: 30d
    trash:
      retention: 30d
  - id: a
    root: /tmp/fc-root2
    schedule: "30 2 * * *"
    retention:
      conditions:
        mtime_older_than: 30d
    trash:
      retention: 30d
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	errs := Validate(cfg)
	if len(errs) == 0 {
		t.Fatal("duplicate policy id must fail")
	}
}

func TestValidateBadCron(t *testing.T) {
	p := writeCfg(t, `
state_dir: /tmp/fc-state
policies:
  - id: a
    root: /tmp/fc-root
    schedule: "not a cron"
    retention:
      conditions:
        mtime_older_than: 30d
    trash:
      retention: 30d
`)
	cfg, _ := Load(p)
	errs := Validate(cfg)
	if len(errs) == 0 {
		t.Fatal("invalid cron must fail")
	}
}

func TestValidateBadDuration(t *testing.T) {
	p := writeCfg(t, `
state_dir: /tmp/fc-state
policies:
  - id: a
    root: /tmp/fc-root
    schedule: "30 2 * * *"
    retention:
      conditions:
        mtime_older_than: 30x
    trash:
      retention: 30d
`)
	cfg, err := Load(p)
	if err == nil {
		// 30x 必须在 duration 解析层报错（Load 或 Validate 均可）
		t.Fatal("invalid duration 30x must fail somewhere")
	}
	_ = cfg
}

func TestValidateEmptyRoot(t *testing.T) {
	p := writeCfg(t, `
state_dir: /tmp/fc-state
policies:
  - id: a
    root: ""
    schedule: "30 2 * * *"
    retention:
      conditions:
        mtime_older_than: 30d
    trash:
      retention: 30d
`)
	cfg, _ := Load(p)
	errs := Validate(cfg)
	if len(errs) == 0 {
		t.Fatal("empty root must fail")
	}
}

func TestValidateMatchOperator(t *testing.T) {
	base := `
state_dir: /tmp/fc-state
policies:
  - id: a
    root: /tmp/fc-root
    schedule: "30 2 * * *"
    retention:
      match: %s
      conditions:
        mtime_older_than: 30d
        ctime_older_than: 30d
    trash:
      retention: 30d
`
	for _, m := range []string{"AND", "OR"} {
		cfg, err := Load(writeCfg(t, interpolate(base, m)))
		if err != nil {
			t.Fatalf("match=%s load: %v", m, err)
		}
		if len(Validate(cfg)) != 0 {
			t.Fatalf("match=%s must validate", m)
		}
	}
	cfg, err := Load(writeCfg(t, interpolate(base, "XOR")))
	if err == nil {
		_ = cfg
		t.Fatal("match=XOR must be rejected")
	}
}

func interpolate(tpl, v string) string {
	// 简单替换 %s
	for i := 0; i < len(tpl); i++ {
		if tpl[i] == '%' && i+1 < len(tpl) && tpl[i+1] == 's' {
			return tpl[:i] + v + tpl[i+2:]
		}
	}
	return tpl
}

func TestValidateTrashNesting(t *testing.T) {
	p := writeCfg(t, `
state_dir: /tmp/fc-state
policies:
  - id: a
    root: /tmp/fc-root
    schedule: "30 2 * * *"
    retention:
      conditions:
        mtime_older_than: 30d
    trash:
      dir: /elsewhere/trash
      retention: 30d
`)
	cfg, _ := Load(p)
	errs := Validate(cfg)
	if len(errs) == 0 {
		t.Fatal("trash dir outside root must fail (default layout requires same fs under root)")
	}
}

func TestDurationParsing(t *testing.T) {
	cases := map[string]time.Duration{
		"30d":  30 * 24 * time.Hour,
		"24h":  24 * time.Hour,
		"30m":  30 * time.Minute,
		"10s":  10 * time.Second,
		"1h":   time.Hour,
	}
	for in, want := range cases {
		got, err := ParseDuration(in)
		if err != nil {
			t.Fatalf("ParseDuration(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseDuration(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"30x", "", "d", "-5d", "1.5d"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Fatalf("ParseDuration(%q) must fail", bad)
		}
	}
}

func TestPolicyHashStable(t *testing.T) {
	a := Policy{ID: "a", Root: "/r"}
	b := Policy{ID: "a", Root: "/r"}
	if a.Hash() != b.Hash() {
		t.Fatal("identical policies must have identical hash")
	}
	c := Policy{ID: "a", Root: "/r2"}
	if a.Hash() == c.Hash() {
		t.Fatal("different policies must differ in hash")
	}
}
