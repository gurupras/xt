package main

import "testing"

// blocks is not implemented on Windows; TestSparse is skipped there.
func blocks(t *testing.T, p string) int64 { return 0 }
