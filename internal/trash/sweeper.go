// sweeper：.retired/.purging 桶的后台物理删除。
//
// 流程（每桶）：.retired/<bucket> --rename--> .purging/<bucket>（崩溃标记）
// → 逐文件 unlink（moveSem 限速）→ 自底向上 rmdir → 桶消失。
// 崩溃恢复：启动 SweepOnce 发现 .purging 残留即续删（幂等）。
// 活跃桶（trash 根下的日期桶）永不触碰。
package trash

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// SweepOnce 执行一轮巡检：处理全部 .retired/.purging 桶。
// 返回删除的文件数与目录数，并落一条 sweep_result 审计事件（含
// files/dirs/duration_ms）。每轮必写事件（含空轮）——物理删除是全系统
// 唯一不可逆操作，"跑过且无事"与"没跑"必须可区分（2026-10-29 巡检：
// 骨架桶 files=0 曾使 SweepLoop 静默且零审计，排查花了 8 轮探针）。
// 不持策略锁（物理删除不阻塞主清理/retire；桶级安全由 rename 原子性保证）。
func (m *Mover) SweepOnce() (int, int, error) {
	start := nowForTest()
	var files, dirs int
	// 1) 恢复 .purging 残留（崩溃续删）
	n, dn, err := m.sweepDir(m.layout.PurgingDir(), true)
	files += n
	dirs += dn
	if err == nil {
		// 2) 处理 .retired：逐桶 rename 进 .purging 后删除
		n2, dn2, err2 := m.sweepDir(m.layout.RetiredDir(), false)
		files += n2
		dirs += dn2
		err = err2
	}
	// sweep_result 每轮必落盘（失败轮 ok:false + err；写失败不吞删除
	// 结果——事件是留痕，不是删除的前置条件）。
	m.writeSweepResult(start, files, dirs, err)
	return files, dirs, err
}

// writeSweepResult 落一条 sweep_result 事件（经 audit 导出 API）。
// 留痕用途：不进入 FindOpenIntents / restore 投影（store.go 查询不含
// 此类型），写失败不吞删除结果——事件是留痕，不是删除的前置条件。
func (m *Mover) writeSweepResult(start time.Time, files, dirs int, sweepErr error) {
	if m.audit == nil {
		return
	}
	runID := "sweep-" + start.Format("20060102-150405")
	msg := ""
	ok := sweepErr == nil
	if !ok {
		msg = sweepErr.Error()
	}
	_ = m.audit.WriteSweepResult(runID, m.policyID, ok, msg, int64(files), int64(dirs), nowForTest().Sub(start).Milliseconds())
}

// sweepDir 处理一个控制目录下的全部桶。
func (m *Mover) sweepDir(dir string, alreadyPurging bool) (int, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	totalFiles := 0
	totalDirs := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bucket := e.Name()
		src := filepath.Join(dir, bucket)
		var target string
		if alreadyPurging {
			target = src // 已在删除区，直接续删
		} else {
			if err := os.MkdirAll(m.layout.PurgingDir(), 0o755); err != nil {
				return totalFiles, totalDirs, err
			}
			target = filepath.Join(m.layout.PurgingDir(), bucket)
			if err := os.Rename(src, target); err != nil {
				return totalFiles, totalDirs, fmt.Errorf("sweep: stage %s: %w", bucket, err)
			}
		}
		n, dn, err := m.deleteTree(target)
		totalFiles += n
		totalDirs += dn
		if err != nil {
			return totalFiles, totalDirs, err
		}
	}
	return totalFiles, totalDirs, nil
}

// deleteTree 逐文件 unlink + 自底向上 rmdir。返回删除文件数与目录数。
func (m *Mover) deleteTree(root string) (int, int, error) {
	// 后序遍历：先文件后目录。
	var walk func(dir string) (int, int, error)
	walk = func(dir string) (int, int, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return 0, 0, err
		}
		n := 0
		dn := 0
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if e.IsDir() {
				sub, subd, err := walk(p)
				if err != nil {
					return n, dn, err
				}
				n += sub
				dn += subd
				continue
			}
			if e.Type()&fs.ModeSymlink != 0 {
				// 符号链接：unlink 链接本身（不跟随）
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					return n, dn, err
				}
				n++
				continue
			}
			// unlink 带限速（moveSem）保证不冲垮 NFS
			if m.moveSem != nil {
				m.moveSem <- struct{}{}
			}
			err := os.Remove(p)
			if m.moveSem != nil {
				<-m.moveSem
			}
			if err != nil && !os.IsNotExist(err) {
				return n, dn, err
			}
			n++
		}
		if dir != root {
			if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
				return n, dn, err
			}
			dn++
		}
		return n, dn, nil
	}
	n, dn, err := walk(root)
	if err != nil {
		return n, dn, err
	}
	if err := os.Remove(root); err != nil && !os.IsNotExist(err) {
		return n, dn, err
	}
	dn++ // 桶根
	return n, dn, nil
}

// nowForTest 测试时钟注入点。
var nowForTest = time.Now
