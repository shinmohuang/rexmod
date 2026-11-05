//go:build darwin || linux

package term

import (
	"bufio"
	"errors"
	"syscall"
	"unsafe"
)

// ErrNoTerminalSize is returned by Size when the ioctl fails.
var ErrNoTerminalSize = errors.New("term: no terminal size")

// Winsize is a terminal's size.
type Winsize struct {
	Rows, Cols, XPixel, YPixel uint16
}

// Terminal is a terminal in raw mode with buffered output: write a frame
// with Writer (or Write), then Flush.
type Terminal struct {
	// Fd is not necessarily stdin; the daemon drives panes' terminals.
	Fd       int
	original syscall.Termios
	Writer   *bufio.Writer
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if e != 0 {
		return e
	}
	return nil
}

// GetTermios reads fd's terminal settings.
func GetTermios(fd int) (syscall.Termios, error) {
	var t syscall.Termios
	err := ioctl(fd, ioctlGetTermios, unsafe.Pointer(&t))
	return t, err
}

// SetTermios applies settings after draining output and flushing input (TCSAFLUSH).
func SetTermios(fd int, t *syscall.Termios) error {
	return ioctl(fd, ioctlSetTermiosFlush, unsafe.Pointer(t))
}

// IsTerminal reports whether fd is a terminal.
func IsTerminal(fd int) bool {
	_, err := GetTermios(fd)
	return err == nil
}

// MakeRaw does what cfmakeraw(3) does.
func MakeRaw(t *syscall.Termios) {
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
}

// Open switches fd to raw mode. Fails if it isn't a terminal. bufSize <= 0
// picks bufio's default.
func Open(fd int, bufSize int) (*Terminal, error) {
	orig, err := GetTermios(fd)
	if err != nil {
		return nil, err
	}
	raw := orig
	MakeRaw(&raw)
	if err := SetTermios(fd, &raw); err != nil {
		return nil, err
	}
	if bufSize <= 0 {
		bufSize = 4096
	}
	return &Terminal{Fd: fd, original: orig, Writer: bufio.NewWriterSize(fdWriter(fd), bufSize)}, nil
}

// Close flushes, then restores the original settings. Errors are ignored.
func (t *Terminal) Close() {
	_ = t.Flush()
	_ = SetTermios(t.Fd, &t.original)
}

// Size is the current size in cells.
func (t *Terminal) Size() (Winsize, error) { return GetSize(t.Fd) }

// GetSize is fd's size in cells.
func GetSize(fd int) (Winsize, error) {
	var ws Winsize
	if ioctl(fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)) != nil {
		return ws, ErrNoTerminalSize
	}
	return ws, nil
}

// Write buffers p.
func (t *Terminal) Write(p []byte) (int, error) { return t.Writer.Write(p) }

// WriteString buffers s.
func (t *Terminal) WriteString(s string) (int, error) { return t.Writer.WriteString(s) }

// Flush sends the buffered frame.
func (t *Terminal) Flush() error { return t.Writer.Flush() }

// fdWriter writes all bytes, looping over short writes and EINTR.
type fdWriter int

func (fd fdWriter) Write(p []byte) (int, error) {
	off := 0
	for off < len(p) {
		n, err := syscall.Write(int(fd), p[off:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return off, err
		}
		off += n
	}
	return off, nil
}

func newBuf(fd int) *bufio.Writer { return bufio.NewWriter(fdWriter(fd)) }
