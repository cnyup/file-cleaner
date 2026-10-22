//go:build !linux

package nfs

import (
	"os"
	"path/filepath"
	"syscall"
)

// fstatFull 非 Linux 回退（开发/测试环境）。生产执行机为 Linux。
func fstatFull(dirfd int, name string) (FileMetaFull, error) {
	dir := "/proc/self/fd/" + itoa(dirfd)
	var st syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(dir, name), &st); err != nil {
		return FileMetaFull{}, err
	}
	return FileMetaFull{
		Size:  st.Size,
		MTime: st.Mtim.Sec*1e9 + st.Mtim.Nsec,
		CTime: st.Ctim.Sec*1e9 + st.Ctim.Nsec,
		ATime: st.Atim.Sec*1e9 + st.Atim.Nsec,
		Dev:   uint64(st.Dev),
		Ino:   st.Ino,
		Mode:  uint32(st.Mode),
	}, nil
}

var _ = os.Getpid
