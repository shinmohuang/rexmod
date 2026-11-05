package main

import (
	"os"
	"syscall"
)

// ownedByMe says whether a file is this user's.
func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
