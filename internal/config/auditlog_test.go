package config

import (
	"strings"
	"testing"
)

func auditCfg(rotation, retention string, trashRetention string) *Config {
	return &Config{
		StateDir: "/tmp/st",
		Policies: []Policy{{
			ID: "p1", Root: "/r", Schedule: "0 2 * * *",
			Retention: Retention{Conditions: map[string]string{"mtime_older_than": "7d"}},
			Trash:     Trash{Dir: ".filecleaner-trash", Retention: trashRetention},
		}},
		AuditLog: AuditLog{Rotation: rotation, Retention: retention},
	}
}

// TestAuditLogDefaults 不配段 = daily + 15d。
func TestAuditLogDefaults(t *testing.T) {
	cfg := auditCfg("", "", "10d")
	if cfg.AuditRotationMode() != AuditRotationDaily {
		t.Fatalf("default rotation = %q want daily", cfg.AuditRotationMode())
	}
	d, err := cfg.AuditRetentionDur()
	if err != nil || d.Hours() != 15*24 {
		t.Fatalf("default retention = %v err=%v want 360h", d, err)
	}
	if errs := Validate(cfg); len(errs) != 0 {
		t.Fatalf("default audit_log should validate clean: %v", errs)
	}
}

// TestAuditLogOffExplicit off 显式合法；非法模式 fail-closed。
func TestAuditLogOffExplicit(t *testing.T) {
	if errs := Validate(auditCfg("off", "15d", "10d")); len(errs) != 0 {
		t.Fatalf("off should validate: %v", errs)
	}
	errs := Validate(auditCfg("weekly", "15d", "10d"))
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "audit_log.rotation") {
		t.Fatalf("bad rotation should fail: %v", errs)
	}
}

// TestAuditLogRetentionValidation 非法保留期报错。永久保留语义由
// rotation: off 承载（单文件永不 prune），retention 只接受正时长。
func TestAuditLogRetentionValidation(t *testing.T) {
	errs := Validate(auditCfg("daily", "abc", "10d"))
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "audit_log.retention") {
		t.Fatalf("bad retention should fail: %v", errs)
	}
	errs = Validate(auditCfg("daily", "0", "10d"))
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "audit_log.retention") {
		t.Fatalf("bare 0 should fail (use rotation:off for keep-forever): %v", errs)
	}
}

// TestAuditLogCrossCheck trash.retention >= audit retention 时报错
// （prune 不能先于 trash 到期删 restore 依据）。
func TestAuditLogCrossCheck(t *testing.T) {
	errs := Validate(auditCfg("daily", "15d", "15d")) // 相等也算违反
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "trash.retention") {
		t.Fatalf("trash 15d vs audit 15d should fail: %v", errs)
	}
	errs = Validate(auditCfg("daily", "15d", "20d"))
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "trash.retention") {
		t.Fatalf("trash 20d vs audit 15d should fail: %v", errs)
	}
	if errs := Validate(auditCfg("daily", "30d", "10d")); len(errs) != 0 {
		t.Fatalf("trash 10d vs audit 30d should pass: %v", errs)
	}
}
