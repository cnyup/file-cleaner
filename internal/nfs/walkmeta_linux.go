//go:build linux

package nfs

import (
	"golang.org/x/sys/unix"
)

// fstatFull 对父目录 fd 下 name 做不跟随符号链接的 stat，返回完整元数据。
// 一次 fstatat 同时取 size/三时间/dev/ino/mode——禁止多次 stat 拆字段。
func fstatFull(dirfd int, name string) (FileMetaFull, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return FileMetaFull{}, err
	}
	return FileMetaFull{
		Size:  st.Size,
		MTime: st.Mtim.Sec*1e9 + st.Mtim.Nsec,
		CTime: st.Ctim.Sec*1e9 + st.Ctim.Nsec,
		ATime: st.Atim.Sec*1e9 + st.Atim.Nsec,
		Dev:   uint64(st.Dev),
		Ino:   st.Ino,
		Mode:  st.Mode,
	}, nil
}
