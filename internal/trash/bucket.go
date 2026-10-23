// 日期分桶回收站（方案：docs/superpowers/specs/2026-09-29--trash-bucketing.md）。
//
// 布局：<trash>/<YYYY-MM-DD>.run-<id>/<原相对路径>
// 控制目录：<trash>/.retired（已到期隔离区）、<trash>/.purging（物理删除中）。
//
// 核心语义：retire = 整桶 rename 进 .retired（O(1)，探针实测 3.8ms@5000 文件，
// 与桶内文件数无关）；物理删除由后台巡检异步完成。
package trash

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bcyup/filecleaner/internal/audit"
)

// 控制目录名（保留字，不得作为桶名成分）。
const (
	RetiredDirName    = ".retired"
	PurgingDirName    = ".purging"
	bucketPrefix      = "run-"
	dateLayoutCompact = "2006-01-02"
	// DefaultTrashName 与 mover.go 默认一致（供 runner 引用）。
	DefaultTrashName = ".filecleaner-trash"
)

// BucketDir 桶的绝对路径（活跃区）。
func (l *Layout) BucketDir(bucket string) string {
	return filepath.Join(l.root, l.trashName, bucket)
}

// NewBucketName 生成桶名：日期.run-<rand>。
func (l *Layout) NewBucketName(now time.Time) string {
	return fmt.Sprintf("%s.%s%06x", now.Format(dateLayoutCompact), bucketPrefix, now.UnixNano()&0xffffff)
}

// BucketPath 桶内某相对路径的绝对路径（活跃区）。
func (l *Layout) BucketPath(bucket, rel string) string {
	return filepath.Join(l.BucketDir(bucket), filepath.FromSlash(rel))
}

// RetiredDir 隔离区目录。
func (l *Layout) RetiredDir() string {
	return filepath.Join(l.root, l.trashName, RetiredDirName)
}

// PurgingDir 物理删除中目录。
func (l *Layout) PurgingDir() string {
	return filepath.Join(l.root, l.trashName, PurgingDirName)
}

// isAlreadyGone 判定错误是否为「文件扫描后已被他人删走」（ENOENT 家族）。
// NFS 上 rename 可能返回裸 ENOENT 或包裹后的 *PathError——errors.Is 两者皆覆盖。
func isAlreadyGone(err error) bool {
	return err != nil && errors.Is(err, fs.ErrNotExist)
}

// MoveBatchBucket 分桶版批量移动：目标为桶内路径。
// 返回（物理成功数, already_gone 数, 聚合错误）。already_gone = rename/
// recheck 阶段 ENOENT——文件在扫描后、移动前被业务侧删走，属良性竞态
// （生产实测 2026-10-16：14911 失败全为 ENOENT），不计入 move_errors、
// 不构成聚合错误。真失败（EACCES 等）仍进聚合错误。
func (m *Mover) MoveBatchBucket(runID, policyID, bucket string, reqs []MoveReq, rc Recheck) (int, int, error) {
	if len(reqs) == 0 {
		return 0, 0, nil
	}
	intents := make([]audit.MoveIntent, len(reqs))
	for i, r := range reqs {
		clean := filepath.Clean(r.Rel)
		if filepath.IsAbs(clean) || clean == ".." || len(clean) >= 3 && clean[:3] == ".." + string(filepath.Separator) {
			return 0, 0, fmt.Errorf("trash: rel %q escapes root", r.Rel)
		}
		ino := r.ScanInode
		if ino == 0 {
			ino = inodeOfPath(r.Src)
		}
		intents[i] = audit.MoveIntent{
			RunID: runID, PolicyID: policyID,
			Src: r.Src, Trash: m.layout.BucketPath(bucket, r.Rel), Inode: ino,
		}
	}
	okCount, goneCount := 0, 0
	err := m.audit.CommitMovesFiltering(intents, func() []error {
		var wg sync.WaitGroup
		errs := make([]error, len(reqs))
		for i, r := range reqs {
			wg.Add(1)
			go func(i int, r MoveReq) {
				defer wg.Done()
				if m.moveSem != nil {
					m.moveSem <- struct{}{}
					defer func() { <-m.moveSem }()
				}
				errs[i] = m.moveOneTo(r, Recheck{}, m.layout.BucketPath(bucket, r.Rel))
			}(i, r)
		}
		wg.Wait()
		okCount, goneCount = countOutcome(errs)
		return errs
	}, isAlreadyGone)
	return okCount, goneCount, err
}

// Retire 整桶退休：rename 进 .retired（O(1)）。桶内文件保持原样等待后台
// 物理删除。审计记桶级事件。
func (m *Mover) Retire(policyID, bucket string) error {
	src := m.layout.BucketDir(bucket)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("retire: bucket %q not found: %w", bucket, err)
	}
	dst := m.layout.RetiredDir()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	runID := fmt.Sprintf("retire-%d", time.Now().UnixNano())
	if err := m.audit.CommitPurge(runID, policyID, src); err != nil {
		return err
	}
	if err := os.Rename(src, filepath.Join(dst, bucket)); err != nil {
		_ = m.audit.WritePurgeResult(runID, policyID, src, false, err.Error())
		return fmt.Errorf("retire rename: %w", err)
	}
	return m.audit.WritePurgeResult(runID, policyID, filepath.Join(dst, bucket), true, "")
}

// RestoreFromBucket 从桶内恢复 rel 到原路径；桶不在活跃区（已 retire）→ 拒绝。
func (m *Mover) RestoreFromBucket(runID, policyID, bucket, rel string) error {
	trashPath := m.layout.BucketPath(bucket, rel)
	srcPath := filepath.Join(m.layout.root, filepath.FromSlash(rel))

	// 活跃桶才可恢复：桶目录必须存在于活跃区。
	if _, err := os.Stat(m.layout.BucketDir(bucket)); err != nil {
		return fmt.Errorf("restore: bucket %q not in active area (retired buckets are not restorable)", bucket)
	}
	if err := m.audit.CommitRestore(runID, policyID, srcPath, trashPath); err != nil {
		return err
	}
	actErr := func() error {
		if _, err := os.Lstat(srcPath); err == nil {
			return fmt.Errorf("restore: target exists, refusing to overwrite %s", srcPath)
		}
		if err := os.MkdirAll(filepath.Dir(srcPath), 0o755); err != nil {
			return err
		}
		return os.Rename(trashPath, srcPath)
	}()
	ok := actErr == nil
	msg := ""
	if !ok {
		msg = actErr.Error()
	}
	if err := m.audit.WriteRestoreResult(runID, policyID, trashPath, srcPath, ok, msg); err != nil && actErr == nil {
		return err
	}
	return actErr
}
