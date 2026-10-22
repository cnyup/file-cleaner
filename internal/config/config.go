// Package config 定义并加载 filecleaner 的 YAML 配置。
//
// 解析纪律：
//   - yaml.v3 KnownFields 严格模式：未知字段直接报错，防止拼写错误
//     静默失效（例如 max_age 写成 maxAge 后策略退化为空条件）；
//   - duration 字段在加载层即解析（"30d"/"24h"/"30m"/"10s"），非法值
//     在 Load 阶段失败，不留给运行时；
//   - match 只允许 AND/OR，默认 AND；
//   - 完整 fail-closed 校验（cron/root/trash 嵌套/policy id 唯一）在
//     Validate，单个策略失败只禁用该策略并返回结构化错误。
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// Config 是配置文件的顶层结构。
type Config struct {
	StateDir string `yaml:"state_dir"`
	// RunOnStart 为 true 时 daemon 启动即对每个 run job 先执行一轮
	// 清理再进入定时循环（消除部署到首个 cron 触发间的空窗）。
	// on-start 轮走同一把策略锁：上次未完成则跳过，不与 cron 轮并行。
	RunOnStart bool `yaml:"run_on_start"`
	// MaxConcurrentRuns 并行执行上限（全局信号量）：多策略调度在同一
	// 时刻触发时，最多 N 个 RunOnce/Purge 同时执行；其余 job 触发即
	// 入队、在信号量上等待（ctx 取消可退出）。=1 退化为完全串行
	// （现历史行为）。缺省 2。
	// 拓扑提示：不同 NAS 存储 ≈ 吞吐叠加；同一 NFS 服务器的多个
	// export 并行会在服务端争元数据吞吐（readdir 密集型扫描）。
	MaxConcurrentRuns int `yaml:"max_concurrent_runs"`
	// AuditLog 审计日志轮转（全局）。不配 = daily + 15d（默认行为）。
	AuditLog AuditLog `yaml:"audit_log"`
	Policies  []Policy `yaml:"policies"`
}

// AuditLog 审计轮转配置。
type AuditLog struct {
	// Rotation daily（默认）：按天切割 audit-YYYYMMDD.jsonl，超过
	// retention 的分片自动清理；off：旧行为单文件 audit.jsonl，永不
	// 自动清理（逃生舱 = 永久保留语义）。
	Rotation string `yaml:"rotation"`
	// Retention 分片保留期（如 15d），仅 daily 模式生效；空 = 15d 默认。
	Retention string `yaml:"retention"`
}

// 常量：轮转默认值。
const (
	AuditRotationDaily  = "daily"
	AuditRotationOff    = "off"
	auditRetentionDefault = "15d"
)

// AuditRotationMode 返回规范化轮转模式（空 → daily）。
func (c *Config) AuditRotationMode() string {
	if c.AuditLog.Rotation == "" {
		return AuditRotationDaily
	}
	return c.AuditLog.Rotation
}

// AuditRetentionDur 返回规范化保留期字符串（空 → 15d 默认；"0" 原样
// 表示永久保留）。
func (c *Config) AuditRetentionDur() (time.Duration, error) {
	r := c.AuditLog.Retention
	if r == "" {
		r = auditRetentionDefault
	}
	d, err := ParseDuration(r)
	if err != nil {
		return 0, err
	}
	return d, nil
}

// maxConcurrentRunsDefault 并行执行上限缺省值。
const maxConcurrentRunsDefault = 2

// ConcurrentRuns 返回规范化并行上限（0/未配 → 2）。
func (c *Config) ConcurrentRuns() int {
	if c.MaxConcurrentRuns <= 0 {
		return maxConcurrentRunsDefault
	}
	return c.MaxConcurrentRuns
}

// Policy 是单个清理策略。
type Policy struct {
	ID          string       `yaml:"id"`
	Root        string       `yaml:"root"`
	Schedule    string       `yaml:"schedule"`
	Retention   Retention    `yaml:"retention"`
	Exclusions  Exclusions   `yaml:"exclusions"`
	Trash       Trash        `yaml:"trash"`
	Concurrency Concurrency  `yaml:"concurrency"`
	// EmptyDirs 空目录清理（可选，不配即关）。判定 = readdir 空 +
	// mtime 超 retention（mtime-only 是实证结论：本工具每日 readdir 刷
	// atime，atime 门永不放行；目录 mtime 在条目增删时都更新，天然挡住
	// 刚清空的目录）。rmdir 物理删除（非空必失败，无数据丢失路径）。
	EmptyDirs EmptyDirs `yaml:"empty_dirs"`
}

// EmptyDirs 空目录清理配置。
type EmptyDirs struct {
	Enabled   bool   `yaml:"enabled"`
	Retention string `yaml:"retention"` // 年龄门；enabled 时必填且合法（Validate fail-closed）
}

// Retention 时间条件组合。
type Retention struct {
	Match          string             `yaml:"match"` // AND | OR，默认 AND
	Conditions     map[string]string  `yaml:"conditions"` // mtime_older_than / ctime_older_than / atime_older_than
	MinQuietPeriod string             `yaml:"min_quiet_period"`
}

// Exclusions 排除规则。
type Exclusions struct {
	Dirs     []string `yaml:"dirs"`
	Patterns []string `yaml:"patterns"` // glob 或 re: 前缀
}

// Trash 回收站配置。
type Trash struct {
	Dir           string `yaml:"dir"`            // 相对 root；默认 .filecleaner-trash
	Retention     string `yaml:"retention"`      // 回收站保留期
	PurgeSchedule string `yaml:"purge_schedule"` // purge 独立 cron
}

// Concurrency 并发配置。
type Concurrency struct {
	DirReadWorkers int `yaml:"dir_read_workers"`
	StatWorkers    int `yaml:"stat_workers"`
	MoveWorkers    int `yaml:"move_workers"`
	MaxOpsPerSec   int `yaml:"max_ops_per_sec"` // 0 = 不限速
}

// DefaultTrashDir 是回收站默认目录名（root 下）。
const DefaultTrashDir = ".filecleaner-trash"

// ParseDuration 解析 "30d"/"24h"/"30m"/"10s" 形式的时长。
// 负数与未知单位报错。
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	unit := s[len(s)-1]
	numStr := s[:len(s)-1]
	n, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	switch unit {
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 's':
		return time.Duration(n) * time.Second, nil
	default:
		return 0, fmt.Errorf("unknown unit in duration %q", s)
	}
}

// Load 从 path 读取 YAML 并严格解析（KnownFields + duration 校验）。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := validateDurations(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validateDurations 在加载层校验全部 duration 字段。
func validateDurations(cfg *Config) error {
	for i := range cfg.Policies {
		p := &cfg.Policies[i]
		for _, kv := range []struct{ name, val string }{
			{"min_quiet_period", p.Retention.MinQuietPeriod},
			{"trash.retention", p.Trash.Retention},
		} {
			if kv.val == "" {
				continue
			}
			if _, err := ParseDuration(kv.val); err != nil {
				return fmt.Errorf("policy %q: %s: %w", p.ID, kv.name, err)
			}
		}
		for k, v := range p.Retention.Conditions {
			switch k {
			case "mtime_older_than", "ctime_older_than", "atime_older_than":
			default:
				return fmt.Errorf("policy %q: unknown condition %q", p.ID, k)
			}
			if _, err := ParseDuration(v); err != nil {
				return fmt.Errorf("policy %q: conditions.%s: %w", p.ID, k, err)
			}
		}
		if m := p.Retention.Match; m != "" && m != "AND" && m != "OR" {
			return fmt.Errorf("policy %q: match must be AND or OR, got %q", p.ID, m)
		}
	}
	return nil
}

// Validate 对已加载配置做 fail-closed 校验；返回每个失败策略的错误。
// 可检查的静态项在此完成；st_dev 一致性等运行时项由引擎在启动时校验。
func Validate(cfg *Config) []error {
	var errs []error
	seen := map[string]bool{}
	if cfg.StateDir == "" {
		errs = append(errs, fmt.Errorf("state_dir is required"))
	}
	// max_concurrent_runs：显式配置 <1 即非法（缺省/0 走默认 2，
	// YAML 显式 0 也按缺省处理——语义是"未有效指定"）
	if cfg.MaxConcurrentRuns < 0 {
		errs = append(errs, fmt.Errorf("max_concurrent_runs must be >= 1, got %d", cfg.MaxConcurrentRuns))
	}
	// audit_log 段：模式与保留期 fail-closed 校验
	switch cfg.AuditRotationMode() {
	case AuditRotationDaily, AuditRotationOff:
	default:
		errs = append(errs, fmt.Errorf("audit_log.rotation must be %q or %q, got %q",
			AuditRotationDaily, AuditRotationOff, cfg.AuditLog.Rotation))
	}
	auditKeep, auditKeepErr := cfg.AuditRetentionDur()
	if auditKeepErr != nil {
		errs = append(errs, fmt.Errorf("bad audit_log.retention: %w", auditKeepErr))
	}
	// 交叉校验：audit 分片保留期必须长于任一策略 trash.retention——
	// 否则 prune 会先于 trash 桶到期删掉 restore 依据（fail-closed）。
	// 仅在 audit_log 段显式配置时生效：不配段走默认 15d（> 生产 trash
	// 10d），不追溯判定既有合法配置，避免向后兼容破坏。
	auditCfgSet := cfg.AuditLog.Rotation != "" || cfg.AuditLog.Retention != ""
	if auditCfgSet && auditKeepErr == nil && auditKeep > 0 {
		for i := range cfg.Policies {
			p := &cfg.Policies[i]
			if p.Trash.Retention == "" {
				continue
			}
			tr, err := ParseDuration(p.Trash.Retention)
			if err != nil {
				continue // trash 段自身的错误由别处报
			}
			if tr >= auditKeep {
				errs = append(errs, fmt.Errorf(
					"policy %q: trash.retention (%s) >= audit_log.retention (%s) — audit shards must outlive trash records for restore",
					p.ID, p.Trash.Retention, cfg.AuditLog.Retention))
			}
		}
	}
	for i := range cfg.Policies {
		p := &cfg.Policies[i]
		if p.ID == "" {
			errs = append(errs, fmt.Errorf("policy[%d]: id is required", i))
			continue
		}
		if seen[p.ID] {
			errs = append(errs, fmt.Errorf("policy %q: duplicate id", p.ID))
			continue
		}
		seen[p.ID] = true
		if p.Root == "" {
			errs = append(errs, fmt.Errorf("policy %q: root is required", p.ID))
		}
		if p.Schedule != "" {
			if _, err := cron.ParseStandard(p.Schedule); err != nil {
				errs = append(errs, fmt.Errorf("policy %q: bad schedule: %w", p.ID, err))
			}
		}
		if p.Trash.PurgeSchedule != "" {
			if _, err := cron.ParseStandard(p.Trash.PurgeSchedule); err != nil {
				errs = append(errs, fmt.Errorf("policy %q: bad purge_schedule: %w", p.ID, err))
			}
		}
		if len(p.Retention.Conditions) == 0 {
			errs = append(errs, fmt.Errorf("policy %q: at least one retention condition required", p.ID))
		}
		if p.EmptyDirs.Enabled {
			if p.EmptyDirs.Retention == "" {
				errs = append(errs, fmt.Errorf("policy %q: empty_dirs.retention is required when enabled (age gate prevents deleting dirs business is creating)", p.ID))
			} else if _, err := ParseDuration(p.EmptyDirs.Retention); err != nil {
				errs = append(errs, fmt.Errorf("policy %q: bad empty_dirs.retention: %w", p.ID, err))
			}
		}
		if err := validateTrashPath(p); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// validateTrashPath 校验回收站目录：必须为空（默认 DefaultTrashDir）
// 或为 root 下的相对路径；绝对路径/越出 root 一律拒绝（默认布局要求
// 回收站与源文件同文件系统，位于 root 下是最强保证）。
func validateTrashPath(p *Policy) error {
	dir := p.Trash.Dir
	if dir == "" {
		return nil
	}
	if strings.HasPrefix(dir, "/") {
		return fmt.Errorf("policy %q: trash.dir must be relative to root, got absolute %q", p.ID, dir)
	}
	clean := filepath.Clean(dir)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("policy %q: trash.dir must not escape root: %q", p.ID, dir)
	}
	return nil
}

// Hash 返回策略配置内容哈希（版本标识），与 JSON 序列化稳定性无关，
// 直接哈希规范化后的字段拼接。
func (p Policy) Hash() string {
	h := sha256.New()
	fmt.Fprintf(h, "id=%s\nroot=%s\nschedule=%s\nmatch=%s\nquiet=%s\ntrash=%s/%s\npurge=%s\n",
		p.ID, p.Root, p.Schedule, p.Retention.Match, p.Retention.MinQuietPeriod,
		p.Trash.Dir, p.Trash.Retention, p.Trash.PurgeSchedule)
	// conditions 排序保证稳定性
	keys := make([]string, 0, len(p.Retention.Conditions))
	for k := range p.Retention.Conditions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "cond[%s]=%s\n", k, p.Retention.Conditions[k])
	}
	for _, d := range p.Exclusions.Dirs {
		fmt.Fprintf(h, "exdir=%s\n", d)
	}
	for _, pt := range p.Exclusions.Patterns {
		fmt.Fprintf(h, "expat=%s\n", pt)
	}
	fmt.Fprintf(h, "conc=%d/%d/%d/%d\n", p.Concurrency.DirReadWorkers, p.Concurrency.StatWorkers,
		p.Concurrency.MoveWorkers, p.Concurrency.MaxOpsPerSec)
	return hex.EncodeToString(h.Sum(nil))[:16]
}
