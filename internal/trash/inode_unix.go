//go:build linux || darwin

package trash

import (
	"os"
	"syscall"
)

// inodeFromSyscall 从 FileInfo.Sys 提取 inode（unix 平台）。
func inodeFromSyscall(st os.FileInfo) uint64 {
	if si, ok := st.Sys().(*syscall.Stat_t); ok {
		return si.Ino
	}
	return 0
}
