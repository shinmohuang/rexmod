package main

import "syscall"

// fsType is the type of the filesystem path is on (statfs's f_fstypename).
func fsType(path string) string {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return ""
	}
	b := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// freeSpace is what an unprivileged user may still write on dir's filesystem.
func diskFree(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
