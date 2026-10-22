package config

import "testing"

// TestMaxConcurrentRunsDefaults 缺省/0 → 2；1（串行逃生舱）合法。
func TestMaxConcurrentRunsDefaults(t *testing.T) {
	cfg := &Config{StateDir: "/tmp/st"}
	if got := cfg.ConcurrentRuns(); got != 2 {
		t.Fatalf("default ConcurrentRuns = %d want 2", got)
	}
	cfg.MaxConcurrentRuns = 0 // YAML 显式 0 或缺省
	if got := cfg.ConcurrentRuns(); got != 2 {
		t.Fatalf("explicit 0 ConcurrentRuns = %d want 2 (treated as unset)", got)
	}
	cfg.MaxConcurrentRuns = 1
	if got := cfg.ConcurrentRuns(); got != 1 {
		t.Fatalf("ConcurrentRuns = %d want 1 (serial escape hatch)", got)
	}
	cfg.MaxConcurrentRuns = 8
	if got := cfg.ConcurrentRuns(); got != 8 {
		t.Fatalf("ConcurrentRuns = %d want 8", got)
	}
}

// TestMaxConcurrentRunsValidation 负数 fail-closed；0/正数合法。
func TestMaxConcurrentRunsValidation(t *testing.T) {
	base := func(n int) *Config {
		return &Config{
			StateDir: "/tmp/st", MaxConcurrentRuns: n,
			Policies: []Policy{{
				ID: "p1", Root: "/r",
				Retention: Retention{Conditions: map[string]string{"mtime_older_than": "1d"}},
				Trash:     Trash{Dir: ".filecleaner-trash", Retention: "10d"},
			}},
		}
	}
	if errs := Validate(base(-1)); len(errs) == 0 {
		t.Fatal("negative max_concurrent_runs must fail")
	}
	for _, ok := range []int{0, 1, 4} {
		if errs := Validate(base(ok)); len(errs) != 0 {
			t.Fatalf("max_concurrent_runs=%d should validate: %v", ok, errs)
		}
	}
}
