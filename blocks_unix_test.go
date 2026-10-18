//go:build !windows

package main

import (
	"os"
	"syscall"
	"testing"
)

// blocks returns the disk space p uses.
func blocks(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Blocks * 512
}
