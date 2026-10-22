// Package policy 实现清理条件判定与排除规则。
//
// 纪律：
//   - 纯函数：Evaluator/Filter 不触碰文件系统，输入 FileMeta 输出 bool；
//   - 正则与 glob 在构造阶段编译并校验，非法规则 fail-closed（构造失败），
//     热路径只做匹配；
//   - 条件求值异常（未知键、负时长）在构造期拒绝，热路径无错误分支。
package policy

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// FileMeta 是策略评估所需的文件元数据（与 nfs 层解耦）。
type FileMeta struct {
	MTime time.Time
	CTime time.Time
	ATime time.Time
}

// Conditions 键为 mtime/ctime/atime，值为"早于该时长才命中"的阈值。
type Conditions map[string]time.Duration

// Evaluator 时间条件判定器。
type Evaluator struct {
	conds   Conditions
	or      bool
	nowFn   func() time.Time
	quiet   time.Duration // >0 时启用静默期
}

// NewEvaluator 构造判定器。match 为 "" / "AND" / "OR"；空条件与负时长
// 一律报错（fail-closed）。
func NewEvaluator(conds Conditions, match string, nowFn func() time.Time) (*Evaluator, error) {
	if len(conds) == 0 {
		return nil, fmt.Errorf("policy: empty conditions")
	}
	for k, d := range conds {
		switch k {
		case "mtime", "ctime", "atime":
		default:
			return nil, fmt.Errorf("policy: unknown condition %q", k)
		}
		if d <= 0 {
			return nil, fmt.Errorf("policy: condition %s duration must be > 0, got %v", k, d)
		}
	}
	switch match {
	case "", "AND":
	case "OR":
	default:
		return nil, fmt.Errorf("policy: match must be AND or OR, got %q", match)
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Evaluator{conds: conds, or: match == "OR", nowFn: nowFn}, nil
}

// SetQuietPeriod 设置静默期：近 quiet 内 mtime/ctime 有变化的文件不命中。
func (e *Evaluator) SetQuietPeriod(q time.Duration) { e.quiet = q }

// Match 判定 meta 是否命中清理条件。
// 语义：时间 t 命中条件 k 当且仅当 now - t >= conds[k]（等于边界命中）。
func (e *Evaluator) Match(m FileMeta) bool {
	now := e.nowFn()
	if e.quiet > 0 {
		cutoff := now.Add(-e.quiet)
		if m.MTime.After(cutoff) || m.CTime.After(cutoff) {
			return false
		}
	}
	if e.or {
		for k, d := range e.conds {
			if timeOf(m, k).Before(now.Add(-d)) || timeOf(m, k).Equal(now.Add(-d)) {
				return true
			}
		}
		return false
	}
	for k, d := range e.conds {
		t := timeOf(m, k)
		cutoff := now.Add(-d)
		if t.After(cutoff) {
			return false
		}
	}
	return true
}

// timeOf 取 meta 的指定时间字段。
func timeOf(m FileMeta, k string) time.Time {
	switch k {
	case "mtime":
		return m.MTime
	case "ctime":
		return m.CTime
	default:
		return m.ATime
	}
}

// Filter 文件名与目录排除规则。
type Filter struct {
	globs   []string
	regexps []*regexp.Regexp
	dirs    map[string]struct{}
}

// NewFilter 编译排除规则。patterns 支持 glob 与 "re:" 前缀正则；
// dirs 为目录 basename 集合。任何非法模式构造失败（fail-closed）。
func NewFilter(patterns, dirs []string) (*Filter, error) {
	f := &Filter{dirs: make(map[string]struct{}, len(dirs))}
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d != "" {
			f.dirs[d] = struct{}{}
		}
	}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(p, "re:"); ok {
			re, err := regexp.Compile(rest)
			if err != nil {
				return nil, fmt.Errorf("policy: bad exclusion regex %q: %w", p, err)
			}
			f.regexps = append(f.regexps, re)
			continue
		}
		if _, err := filepath.Match(p, "x"); err != nil {
			return nil, fmt.Errorf("policy: bad exclusion glob %q: %w", p, err)
		}
		f.globs = append(f.globs, p)
	}
	return f, nil
}

// MatchName 文件名是否命中排除规则。
func (f *Filter) MatchName(name string) bool {
	for _, g := range f.globs {
		if ok, _ := filepath.Match(g, name); ok {
			return true
		}
	}
	for _, re := range f.regexps {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// SkipDir 目录 basename 是否被排除。
func (f *Filter) SkipDir(name string) bool {
	_, ok := f.dirs[name]
	return ok
}
