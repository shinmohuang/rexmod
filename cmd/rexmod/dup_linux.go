package main

import "syscall"

// linux/arm64 has no dup2, only dup3.
func dupTo(oldfd, newfd int) error { return syscall.Dup3(oldfd, newfd, 0) }
