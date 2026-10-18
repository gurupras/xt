//go:build !windows

package xfer

import (
	"io/fs"
	"os"
	"syscall"
)

var umask = func() uint32 {
	m := syscall.Umask(0)
	syscall.Umask(m)
	return uint32(m)
}()

func ownerOf(fi fs.FileInfo) (uid, gid uint32, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return st.Uid, st.Gid, true
}

func isRoot() bool { return os.Geteuid() == 0 }

// oNoFollow makes OpenFile fail on a symlink instead of following it.
const oNoFollow = syscall.O_NOFOLLOW

// devOf returns the device a file lives on, for --one-file-system.
func devOf(fi fs.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}

// inodeKey identifies a file with more than one hard link.
type inodeKey struct{ dev, ino uint64 }

func hardLinkKey(fi fs.FileInfo) (inodeKey, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink < 2 {
		return inodeKey{}, false
	}
	return inodeKey{uint64(st.Dev), uint64(st.Ino)}, true
}
