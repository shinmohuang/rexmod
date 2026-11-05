package term

import "syscall"

const (
	ioctlGetTermios      = syscall.TIOCGETA
	ioctlSetTermiosFlush = syscall.TIOCSETAF
)
