//go:build linux

// emptydirs_getdents_linux.go getdents64 直读层（Linux）。
//
// 为什么不用 os.File.ReadDir：其内部按整块 getdents 缓冲推进 fd 偏移，
// ReadDir(n) 返回 n 条后 fd 偏移已越过缓冲内其余条目（~200 条/8KB）。
// 以「fd 当前偏移」当续扫 cookie 会跳过未处理条目——窗口边界处每轮
// 丢一缓冲的目录。直读 getdents64 并解析每条 dirent 的 d_off
//（探针①-⑥ 同款布局：ino u64 | d_off i64 | reclen u16 | dtype u8 | name），
// checkpoint 精确到最后一条已处理条目，零丢失。
package nfs

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

// dirEntryReader 目录条目流（per-entry cookie 语义）。
type dirEntryReader struct {
	f       *os.File
	buf     []byte
	pending []rawEntry // 已解析未交出
	eof     bool
}

type rawEntry struct {
	name  string
	dOff  int64
	isDir bool
}

func newDirEntryReader(f *os.File) *dirEntryReader {
	return &dirEntryReader{f: f, buf: make([]byte, 64*1024)}
}

// seekCookie 定位到 cookie（0 = 从头）。NFSv3 语义：cookie 是「上一条
// 之后」的位置，getdents 从该位置返回后续条目。fresh fd 定位（探针③④：
// 同 fd EOF 后非零寻址不可靠，本引擎每轮一个 fd，seek 仅开轮时执行一次）。
func (r *dirEntryReader) seekCookie(cookie int64) error {
	if cookie <= 0 {
		_, err := r.f.Seek(0, io.SeekStart)
		return err
	}
	_, err := r.f.Seek(cookie, io.SeekStart)
	return err
}

// next 交出至多 n 条；lastOff = 最后一条交出条目的 d_off（n 条全来自
// pending 时为该条目自身 d_off）。eof=true 表示目录耗尽。
func (r *dirEntryReader) next(n int) (out []rawEntry, lastOff int64, eof bool, err error) {
	for len(r.pending) < n && !r.eof {
		nr, ge := getdents64(int(r.f.Fd()), r.buf)
		if ge != nil {
			return nil, 0, false, ge
		}
		if nr == 0 {
			r.eof = true
			break
		}
		// 解析整个 getdents 块。
		pos := 0
		for pos < nr {
			ino := int64(le64(r.buf[pos:]))
			doff := int64(le64(r.buf[pos+8:]))
			reclen := int(le16(r.buf[pos+16:]))
			if reclen < 19 || pos+reclen > nr {
				return nil, 0, false, os.ErrInvalid
			}
			dtype := r.buf[pos+18]
			name := cstr(r.buf[pos+19 : pos+reclen])
			if name != "." && name != ".." {
				r.pending = append(r.pending, rawEntry{
					name:  name,
					dOff:  doff,
					isDir: dtype == syscall.DT_DIR,
				})
			}
			_ = ino
			pos += reclen
		}
	}
	take := n
	if take > len(r.pending) {
		take = len(r.pending)
	}
	out = r.pending[:take]
	r.pending = r.pending[take:]
	if len(out) > 0 {
		lastOff = out[len(out)-1].dOff
	}
	return out, lastOff, r.eof && len(r.pending) == 0, nil
}

func getdents64(fd int, buf []byte) (int, error) {
	n, _, errno := syscall.Syscall(
		syscall.SYS_GETDENTS64,
		uintptr(fd),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}

func le64(b []byte) uint64 {
	_ = b[7]
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func le16(b []byte) uint16 {
	_ = b[1]
	return uint16(b[0]) | uint16(b[1])<<8
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
