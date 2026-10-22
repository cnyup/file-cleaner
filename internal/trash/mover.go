// Package trash 实现回收站布局、原子 mover、恢复与到期 purge。
//
// 不变量（与设计文档对齐）：
//   - 移动 = 同文件系统 rename；EXDEV 绝不回退 copy（记 error 跳过）；
//   - rename 前重核验（inode/mtime 与扫描快照一致，属性变化即跳过）；
//   - intent 持久化（audit 事务内部 fsync）先于 rename；
//   - restore 不覆盖已存在的原路径文件；
//   - purge 只删投影中已知且到期的条目，未知回收站文件永不删。
package trash

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bcyup/filecleaner/internal/audit"
)

// Layout 回收站路径布局。
type Layout struct {
	root      string
	trashName string
}

// NewLayout root 为策略根目录。
func NewLayout(root, trashName string) *Layout {
	if trashName == "" {
		trashName = ".filecleaner-trash"
	}
	return &Layout{root: root, trashName: trashName}
}

// TrashPath rel 的回收站绝对路径。
func (l *Layout) TrashPath(rel string) string {
	return filepath.Join(l.root, l.trashName, filepath.FromSlash(rel))
}

// Root 返回策略根。
func (l *Layout) Root() string { return l.root }

// TrashName 返回回收站目录名。
func (l *Layout) TrashName() string { return l.trashName }

// OpenAudit 打开审计 JSONL（测试与调用方便捷入口）。
func OpenAudit(path string) (*audit.JSONL, error) { return audit.OpenJSONL(path) }

// Recheck 扫描时快照，rename 前核验用。
type Recheck struct {
	Mtime time.Time
	Inode uint64
}

// MoveReq 单个移动请求。
type MoveReq struct {
	Src string // 绝对路径
	Rel string // 相对 root 斜杠路径
	// ScanInode/ScanMtime 是扫描阶段的元数据快照。携带时 moveOne 直接
	// 用快照核验（省一次 NFS Lstat 往返）；零值时回退为移动前独立核验。
	ScanInode uint64
	ScanMtime time.Time
}

// Mover 执行移动/恢复/purge。
type Mover struct {
	layout *Layout
	audit  *audit.JSONL
	store  *audit.Store // 可为 nil（测试模式：purge 退化为不删）
	// moveSem 限制批内并发 rename（NFS 元数据写并发）；nil = 串行（兼容旧行为）。
	moveSem chan struct{}
	// mkdirCache 已确认存在的回收站父目录（消除每文件 MkdirAll 的重复
	// stat/mkdir RPC；继承旧工程验证过的优化）。
	mkdirCache sync.Map
}

// SetMoveConcurrency 启用批内并发 rename（workers<=0 不改变串行行为）。
func (m *Mover) SetMoveConcurrency(workers int) {
	if workers > 1 {
		m.moveSem = make(chan struct{}, workers)
	}
}

// ensureTrashDir 确保回收站父目录存在，命中缓存零 syscall。
func (m *Mover) ensureTrashDir(dir string) error {
	if _, ok := m.mkdirCache.Load(dir); ok {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	m.mkdirCache.Store(dir, struct{}{})
	return nil
}

// NewMover 构造（store 可 nil）。
func NewMover(layout *Layout, jl *audit.JSONL) *Mover {
	return &Mover{layout: layout, audit: jl}
}

// SetStore 注入投影（purge 需要）。
func (m *Mover) SetStore(st *audit.Store) { m.store = st }

// MoveBatch 批量移动（intent → rename → result 事务）。
// 返回批内物理成功数与聚合错误（首个非 nil）。
func (m *Mover) MoveBatch(runID, policyID string, reqs []MoveReq, rc Recheck) (int, error) {
	if len(reqs) == 0 {
		return 0, nil
	}
	intents := make([]audit.MoveIntent, len(reqs))
	for i, r := range reqs {
		clean := filepath.Clean(r.Rel)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return 0, fmt.Errorf("trash: rel %q escapes root", r.Rel)
		}
		// inode 优先取扫描快照（零 RPC）；未携带才 Lstat（手动路径兼容）。
		ino := r.ScanInode
		if ino == 0 {
			ino = inodeOfPath(r.Src)
		}
		intents[i] = audit.MoveIntent{RunID: runID, PolicyID: policyID, Src: r.Src, Trash: m.layout.TrashPath(r.Rel), Inode: ino}
	}

	okCount := 0
	err := m.audit.CommitMoves(intents, func() []error {
		// 批内并发 rename（受 moveSem 约束；未启用时退化为串行）。
		// 审计事务语义不变：intent 已全部落盘，本批全部 rename 完成后才写 result。
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
				errs[i] = m.moveOne(r, rc)
			}(i, r)
		}
		wg.Wait()
		okCount = countNil(errs)
		return errs
	})
	return okCount, err
}

// countNil 统计 nil 元素个数（物理成功数）。
func countNil(errs []error) int {
	n := 0
	for _, e := range errs {
		if e == nil {
			n++
		}
	}
	return n
}

// moveOne 扁平布局移动（兼容路径）；分桶布局走 bucket.go 的 moveOneTo。
func (m *Mover) moveOne(r MoveReq, rc Recheck) error {
	dst := m.layout.TrashPath(r.Rel)
	if _, err := os.Lstat(dst); err == nil {
		dst = fmt.Sprintf("%s.%d", dst, time.Now().UnixNano())
	}
	return m.moveOneTo(r, rc, dst)
}

// moveOneTo 移动到指定 dst：mkdir → 核验 → rename。
// 核验语义：
//   - 携带 ScanInode：rename 后对 dst stat 一次，比对 inode——不匹配
//     （扫描后被业务替换）则原子搬回原位并报错。每文件 2 RPC；
//   - 未携带：移动前 Lstat 核验（旧行为）。
func (m *Mover) moveOneTo(r MoveReq, rc Recheck, dst string) error {
	if err := m.ensureTrashDir(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("mkdir trash parent: %w", err)
	}

	if r.ScanInode == 0 && r.ScanMtime.IsZero() {
		// 旧路径：移动前核验。
		st, err := os.Lstat(r.Src)
		if err != nil {
			return fmt.Errorf("recheck lstat: %w", err)
		}
		if rc.Inode != 0 {
			if got := inodeOfStat(st); got != 0 && got != rc.Inode {
				return fmt.Errorf("recheck: inode changed")
			}
		}
		if !rc.Mtime.IsZero() && !st.ModTime().Equal(rc.Mtime) {
			return fmt.Errorf("recheck: mtime changed")
		}
		if err := os.Rename(r.Src, dst); err != nil {
			return fmt.Errorf("rename: %w", err)
		}
		return nil
	}

	// 快照模式：rename → 事后核验 → 不符回滚。
	if err := os.Rename(r.Src, dst); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	got := inodeOfPath(dst)
	if got == 0 || got == r.ScanInode {
		return nil
	}
	// 进站的不是扫描时的文件：搬回原位，报错（fail-closed）。
	if err := os.Rename(dst, r.Src); err != nil {
		return fmt.Errorf("post-verify inode mismatch (%d != %d) AND rollback failed: %v (file left in trash: %s)", got, r.ScanInode, err, dst)
	}
	return fmt.Errorf("post-verify: inode changed (%d != %d), rolled back", got, r.ScanInode)
}

// inodeOfStat 从 FileInfo 提取 inode（可能返回 0 = 不支持）。
func inodeOfStat(st os.FileInfo) uint64 {
	type inoer interface{ Ino() uint64 }
	if si, ok := st.Sys().(inoer); ok {
		return si.Ino()
	}
	return inodeFromSyscall(st)
}

// inodeOfPath Lstat 一次并返回 inode（0 = 失败/不支持）。
func inodeOfPath(p string) uint64 {
	st, err := os.Lstat(p)
	if err != nil {
		return 0
	}
	return inodeOfStat(st)
}

// Restore 从回收站恢复 rel 到原路径；原路径已存在 → 拒绝。
func (m *Mover) Restore(runID, policyID, rel string) error {
	trashPath := m.layout.TrashPath(rel)
	srcPath := filepath.Join(m.layout.root, filepath.FromSlash(rel))

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

// Purge 删除投影中状态为 moved 且入站时间早于 expiry 的条目。
// 未知回收站文件（投影无记录）永不删除。返回被删路径。
// 批量事务 + 批内并发 unlink（受 moveSem 约束）。
func (m *Mover) Purge(policyID string, expiry time.Time) ([]string, error) {
	if m.store == nil {
		return nil, fmt.Errorf("trash: purge requires projection store")
	}
	if _, err := m.store.ImportJSONL(m.audit.Path()); err != nil {
		return nil, fmt.Errorf("trash: refresh projection: %w", err)
	}
	rows, err := m.store.QueryMoved(policyID)
	if err != nil {
		return nil, err
	}

	// 过滤到期候选
	var candidates []string
	for _, row := range rows {
		movedAt, ok, err := m.store.MovedAt(row.Trash)
		if err != nil || !ok {
			continue // 未知条目：fail-closed 跳过
		}
		if movedAt.After(expiry) {
			continue
		}
		candidates = append(candidates, row.Trash)
	}

	runID := fmt.Sprintf("purge-%d", time.Now().UnixNano())
	var purged []string
	const chunk = 512
	for start := 0; start < len(candidates); start += chunk {
		end := min(start+chunk, len(candidates))
		batch := candidates[start:end]
		err := m.audit.CommitPurgeBatch(runID, policyID, batch, func() []error {
			var wg sync.WaitGroup
			errs := make([]error, len(batch))
			for i, p := range batch {
				wg.Add(1)
				go func(i int, p string) {
					defer wg.Done()
					if m.moveSem != nil {
						m.moveSem <- struct{}{}
						defer func() { <-m.moveSem }()
					}
					if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
						errs[i] = err
					}
				}(i, p)
			}
			wg.Wait()
			// 按 per-file 结果收集物理已删路径（ENOENT 视为已删）。
			for i, p := range batch {
				if errs[i] == nil {
					purged = append(purged, p)
				}
			}
			return errs
		})
		if err != nil {
			return purged, err
		}
	}
	return purged, nil
}
