package term

import "syscall"

// TCSETSF is TCSETS + 2 on every Linux architecture Go supports but the
// few (ppc, mips, sparc) rexmod isn't built for; syscall leaves it out.
const (
	ioctlGetTermios      = syscall.TCGETS
	ioctlSetTermiosFlush = syscall.TCSETS + 2
)
