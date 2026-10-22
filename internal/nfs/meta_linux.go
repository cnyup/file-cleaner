//go:build linux

package nfs

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// fstatAt 对父目录 fd 下的 name 做不跟随符号链接的 stat，返回文件模式。
// dirfd 相对查找只解析单个路径分量；与 lstat(dir/name, AT_SYMLINK_NOFOLLOW)
// 语义等价，但避免深路径逐级重复解析（NFS 上即逐级 LOOKUP RPC）。
func fstatAt(dirfd int, name string) (fs.FileMode, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, err
	}
	return fileModeFromStat(st.Mode), nil
}

// fileModeFromStat 将 st_mode 转换为 fs.FileMode（仅区分目录/常规/符号链接/
// 其他，满足扫描分类需要）。
func fileModeFromStat(mode uint32) fs.FileMode {
	var m fs.FileMode
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		m |= fs.ModeDir
	case unix.S_IFLNK:
		m |= fs.ModeSymlink
	}
	if mode&unix.S_IFMT == unix.S_IFREG {
		// 常规文件：Mode 为零值
		return m
	}
	if m == 0 {
		m = fs.ModeIrregular
	}
	return m
}
