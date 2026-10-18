//go:build windows

package xfer

import "io/fs"

const umask = 0

func ownerOf(fs.FileInfo) (uid, gid uint32, ok bool) { return 0, 0, false }

func isRoot() bool { return false }

const oNoFollow = 0

func devOf(fs.FileInfo) (uint64, bool) { return 0, false }

type inodeKey struct{ dev, ino uint64 }

// Hard links are not detected on Windows senders.
func hardLinkKey(fs.FileInfo) (inodeKey, bool) { return inodeKey{}, false }
