//go:build !linux

package nfs

import (
	"io/fs"
	"os"
	"path/filepath"
)

// fstatAt 非 Linux 回退：完整路径 lstat。仅用于开发环境测试；
// 生产执行机为 Linux，热路径走 meta_linux.go 的 dirfd 相对 fstatat。
func fstatAt(dirfd int, name string) (fs.FileMode, error) {
	// dirfd 在非 Linux 下不可直接使用；调用方保证 name 相对当前打开目录。
	// 为保持测试语义，这里退化为工作目录相对解析是不可行的——
	// 因此 ScanTree 在非 Linux 上由调用方传入的 root 已是绝对路径，
	// 我们通过 /proc/self/fd 解析 dirfd 对应目录。
	dir := "/proc/self/fd/" + itoa(dirfd)
	info, err := os.Lstat(filepath.Join(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, err
		}
		return 0, err
	}
	return info.Mode(), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
