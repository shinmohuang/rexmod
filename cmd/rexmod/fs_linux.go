package main

import (
	"os"
	"syscall"
)

// fsType is the type of the filesystem path is on, from mountinfo: it names
// FUSE filesystems by their subtype (fuse.sshfs), which statfs can't.
func fsType(path string) string {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	return mountinfoFSType(string(b), path)
}

// freeSpace is what an unprivileged user may still write on dir's filesystem.
func diskFree(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
