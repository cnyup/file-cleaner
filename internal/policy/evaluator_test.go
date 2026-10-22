package policy

import (
	"errors"
	"testing"
	"time"
)

// meta 是测试用的文件元数据。
type meta struct {
	mtime, ctime, atime time.Time
}

func TestTimeConditionsMatrix(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	old := now.Add(-31 * 24 * time.Hour) // 满足 30d
	new := now.Add(-24 * time.Hour)      // 不满足 30d

	mk := func(m, c, a time.Time) FileMeta {
		return FileMeta{MTime: m, CTime: c, ATime: a}
	}

	// 全老 / 全新 / mtime 新其余老 / 全部边界（恰等于阈值）
	older := func(d time.Duration) time.Time { return now.Add(-d) }
	threshold := 30 * 24 * time.Hour

	cases := []struct {
		name    string
		conds   Conditions
		match   string // "" = AND 默认
		meta    FileMeta
		want    bool
	}{
		{"and-all-old", Conditions{"mtime": threshold, "ctime": threshold}, "", mk(old, old, old), true},
		{"and-mtime-new", Conditions{"mtime": threshold, "ctime": threshold}, "", mk(new, old, old), false},
		{"or-one-old", Conditions{"mtime": threshold, "atime": threshold}, "OR", mk(new, old, new), false},
		{"or-ctime-old", Conditions{"mtime": threshold, "ctime": threshold}, "OR", mk(new, old, now), true},
		{"or-none-old", Conditions{"mtime": threshold, "atime": threshold}, "OR", mk(new, new, new), false},
		{"boundary-eq-mtime", Conditions{"mtime": threshold}, "", mk(older(threshold), now, now), true},
		{"boundary-minus-1ns-mtime", Conditions{"mtime": threshold}, "", mk(older(threshold).Add(time.Nanosecond), now, now), false},
		{"boundary-plus-1ns-mtime", Conditions{"mtime": threshold}, "", mk(older(threshold).Add(-time.Nanosecond), now, now), true},
		{"boundary-eq-ctime", Conditions{"ctime": threshold}, "", mk(now, older(threshold), now), true},
		{"boundary-eq-atime", Conditions{"atime": threshold}, "", mk(now, now, older(threshold)), true},
	}
	for _, tc := range cases {
		e, err := NewEvaluator(tc.conds, tc.match, func() time.Time { return now })
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := e.Match(tc.meta); got != tc.want {
			t.Errorf("%s: Match=%v want=%v", tc.name, got, tc.want)
		}
	}
}

func TestQuietPeriod(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	e, err := NewEvaluator(Conditions{"mtime": 30 * time.Minute}, "", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	e.SetQuietPeriod(30 * time.Minute)

	// mtime 50 分钟前：满足 30m 条件；但 ctime 10 分钟前（静默窗口内）→ 拦截
	m := FileMeta{MTime: now.Add(-50 * time.Minute), CTime: now.Add(-10 * time.Minute)}
	if e.Match(m) {
		t.Error("ctime within quiet period must disqualify")
	}
	// ctime 31 分钟前（窗口外）且 mtime 条件满足 → 匹配
	m.CTime = now.Add(-31 * time.Minute)
	if !e.Match(m) {
		t.Error("ctime outside quiet period with mtime condition met should match")
	}
}

func TestInvalidConditions(t *testing.T) {
	// 空 cond
	if _, err := NewEvaluator(Conditions{}, "", time.Now); err == nil {
		t.Error("empty conditions must fail (fail-closed)")
	}
	// 非法 match
	if _, err := NewEvaluator(Conditions{"mtime": time.Hour}, "XOR", time.Now); err == nil {
		t.Error("invalid match operator must fail")
	}
	// 负 duration
	if _, err := NewEvaluator(Conditions{"mtime": -time.Hour}, "", time.Now); err == nil {
		t.Error("negative duration must fail")
	}
}

// ---------- 排除规则 ----------

func TestExclusionPatterns(t *testing.T) {
	f, err := NewFilter([]string{"*.log", "re:^keep_", "tmp*"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"a.log":       true,
		"keep_me":     true,
		"tmpfile":     true,
		"keepx":       false, // re:^keep_ 要求下划线
		"data.bin":    false,
		"mylog.txt":   false, // glob 只匹配尾部 .log
		"KEEP_ME":     false, // 大小写敏感
	} {
		if got := f.MatchName(name); got != want {
			t.Errorf("MatchName(%q)=%v want=%v", name, got, want)
		}
	}
}

func TestExclusionDirs(t *testing.T) {
	f, err := NewFilter(nil, []string{"cache", ".snapshots", "node_modules"})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"cache", ".snapshots", "node_modules"} {
		if !f.SkipDir(d) {
			t.Errorf("SkipDir(%q) must be true", d)
		}
	}
	for _, d := range []string{"src", "Cache", "cachex"} {
		if f.SkipDir(d) {
			t.Errorf("SkipDir(%q) must be false", d)
		}
	}
}

func TestBadPatternFailClosed(t *testing.T) {
	// 非法 glob
	if _, err := NewFilter([]string{"[bad"}, nil); err == nil {
		t.Error("malformed glob must fail at construction (fail-closed)")
	}
	// 非法正则
	if _, err := NewFilter([]string{"re:("}, nil); err == nil {
		t.Error("malformed regex must fail at construction")
	}
}

func TestFilterZeroValue(t *testing.T) {
	// 零值 Filter 必须安全：不 panic，全部放行
	var f Filter
	if f.MatchName("x") || f.SkipDir("y") {
		t.Error("zero filter must not match anything")
	}
	_ = errors.New
}
