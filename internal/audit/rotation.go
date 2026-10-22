// rotation.go — audit JSONL 按天轮转与保留期清理。
//
// 设计要点（与 plans/audit-日志按天轮转 一致）：
//   - 分片名 audit-YYYYMMDD.jsonl（本地时区）；legacy audit.jsonl 永不
//     被 PruneOldFiles 删除（fail-safe，留运维手动处置）。
//   - 轮转发生在每次打开时（writer 本就每次 run 打开/关闭），无长开
//     writer 切换问题；跨午夜长跑归属启动日，事件内 ts 为准。
//   - audit 包零依赖 config：rotation 用原始参数表达，config 层负责
//     翻译（Wave 2 接线）。
package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RotationMode 轮转模式。
type RotationMode string

const (
	// RotationDaily 按天切割：写 audit-YYYYMMDD.jsonl。
	RotationDaily RotationMode = "daily"
	// RotationOff 旧行为：单文件 audit.jsonl（逃生舱）。
	RotationOff RotationMode = "off"
)

const (
	auditBaseName    = "audit.jsonl"
	auditShardPrefix = "audit-"
	auditShardExt    = ".jsonl"
	shardDateLayout  = "20060102"
)

// OpenJSONLRotating 按模式打开审计 writer：daily 写 now 当天的分片，
// off 写 legacy audit.jsonl。dir 为 state 目录。
func OpenJSONLRotating(dir string, mode RotationMode, now time.Time) (*JSONL, error) {
	var path string
	switch mode {
	case RotationDaily:
		path = filepath.Join(dir, "audit-"+now.Format(shardDateLayout)+".jsonl")
	case RotationOff, "":
		path = filepath.Join(dir, auditBaseName)
	default:
		return nil, fmt.Errorf("audit: unknown rotation mode %q", mode)
	}
	return OpenJSONL(path)
}

// AuditFiles 返回 dir 下全部审计 JSONL（legacy + 日期分片），排序稳定：
// legacy 最前、分片按日期升序。事件唯一键幂等使顺序只影响导入效率。
// dir 不存在时返回空切片（与 ImportJSONL 的 IsNotExist 语义对齐）。
func AuditFiles(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, auditShardPrefix+"*"+auditShardExt))
	if err != nil {
		return nil, err
	}
	var shards []string
	for _, m := range matches {
		if _, ok := shardDate(m); ok {
			shards = append(shards, m)
		}
	}
	sort.Strings(shards) // audit-YYYYMMDD 字典序 = 日期序
	legacy := filepath.Join(dir, auditBaseName)
	if _, err := os.Stat(legacy); err == nil {
		return append([]string{legacy}, shards...), nil
	}
	return shards, nil
}

// shardDate 从文件名解析分片日期；非分片形态返回 ok=false。
func shardDate(path string) (time.Time, bool) {
	name := filepath.Base(path)
	if !strings.HasPrefix(name, auditShardPrefix) || !strings.HasSuffix(name, auditShardExt) {
		return time.Time{}, false
	}
	d := strings.TrimSuffix(strings.TrimPrefix(name, auditShardPrefix), auditShardExt)
	if len(d) != len(shardDateLayout) {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(shardDateLayout, d, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// PruneOldFiles 删除日期早于 now-keep 的分片。keep=0 表示永久保留
// （no-op）。legacy audit.jsonl 与不可解析日期的文件永不删除。
// 返回被删除的文件名（basename 列表）。
func PruneOldFiles(dir string, keep time.Duration, now time.Time) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, auditShardPrefix+"*"+auditShardExt))
	if err != nil {
		return nil, err
	}
	cutoff := now.Add(-keep)
	var removed []string
	for _, m := range matches {
		d, ok := shardDate(m)
		if !ok {
			continue // 不可解析：跳过，永不误删
		}
		if d.Before(cutoff) {
			if err := os.Remove(m); err != nil {
				return removed, fmt.Errorf("audit: prune %s: %w", m, err)
			}
			removed = append(removed, filepath.Base(m))
		}
	}
	return removed, nil
}

// FindOpenIntentsAll 聚合 dir 下全部审计文件的未闭环 intent
// （intent 与 result 可能落在不同分片——跨午夜长跑的常态）。
// 与单文件 FindOpenIntents 同语义：截断尾行跳过。
func FindOpenIntentsAll(dir string) ([]MoveIntent, error) {
	files, err := AuditFiles(dir)
	if err != nil {
		return nil, err
	}
	open := map[string]MoveIntent{}
	for _, f := range files {
		if err := scanOpenIntents(f, open); err != nil {
			return nil, err
		}
	}
	out := make([]MoveIntent, 0, len(open))
	for _, v := range open {
		out = append(out, v)
	}
	return out, nil
}
