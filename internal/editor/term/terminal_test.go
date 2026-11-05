//go:build darwin || linux

package term

import (
	"os"
	"syscall"
	"testing"
)

func TestNotATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if IsTerminal(int(r.Fd())) {
		t.Fatal("pipe is a terminal")
	}
	if _, err := Open(int(r.Fd()), 0); err == nil {
		t.Fatal("Open on a pipe succeeded")
	}
	if _, err := GetSize(int(r.Fd())); err != ErrNoTerminalSize {
		t.Fatal(err)
	}
}

func TestMakeRaw(t *testing.T) {
	tio := syscall.Termios{Lflag: syscall.ICANON | syscall.ECHO, Oflag: syscall.OPOST}
	MakeRaw(&tio)
	if tio.Lflag&(syscall.ICANON|syscall.ECHO) != 0 || tio.Oflag&syscall.OPOST != 0 || tio.Cflag&syscall.CS8 != syscall.CS8 {
		t.Fatalf("%+v", tio)
	}
}

func TestFdWriter(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	tm := &Terminal{Fd: int(w.Fd())}
	tm.Writer = newBuf(int(w.Fd()))
	tm.WriteString("hi")
	if err := tm.Flush(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	n, _ := r.Read(buf)
	if string(buf[:n]) != "hi" {
		t.Fatalf("%q", buf[:n])
	}
}
