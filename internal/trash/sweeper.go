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

// SweepOnce 执行一轮巡检：处理全部 .retired/.purging 桶，返回删除的文件数。
// 不持策略锁（物理删除不阻塞主清理/retire；桶级安全由 rename 原子性保证）。
func (m *Mover) SweepOnce() (int, error) {
	// 1) 恢复 .purging 残留（崩溃续删）
	n, err := m.sweepDir(m.layout.PurgingDir(), true)
	if err != nil {
		return n, err
	}
	// 2) 处理 .retired：逐桶 rename 进 .purging 后删除
	n2, err := m.sweepDir(m.layout.RetiredDir(), false)
	return n + n2, err
}

// sweepDir 处理一个控制目录下的全部桶。
func (m *Mover) sweepDir(dir string, alreadyPurging bool) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	total := 0
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
				return total, err
			}
			target = filepath.Join(m.layout.PurgingDir(), bucket)
			if err := os.Rename(src, target); err != nil {
				return total, fmt.Errorf("sweep: stage %s: %w", bucket, err)
			}
		}
		n, err := m.deleteTree(target)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// deleteTree 逐文件 unlink + 自底向上 rmdir。返回删除文件数。
func (m *Mover) deleteTree(root string) (int, error) {
	counted := 0
	// 后序遍历：先文件后目录。
	var walk func(dir string) (int, error)
	walk = func(dir string) (int, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return 0, err
		}
		n := 0
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if e.IsDir() {
				sub, err := walk(p)
				if err != nil {
					return n, err
				}
				n += sub
				continue
			}
			if e.Type()&fs.ModeSymlink != 0 {
				// 符号链接：unlink 链接本身（不跟随）
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					return n, err
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
				return n, err
			}
			n++
		}
		if dir != root {
			if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
				return n, err
			}
		}
		return n, nil
	}
	n, err := walk(root)
	if err != nil {
		return n, err
	}
	counted = n
	if err := os.Remove(root); err != nil && !os.IsNotExist(err) {
		return counted, err
	}
	return counted, nil
}

// nowForTest 测试时钟注入点。
var nowForTest = time.Now
