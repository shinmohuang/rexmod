//go:build darwin || linux

// Package panehelper is `pane SOCKET [ID]`: the helper each Rex pane runs.
// It hands its terminal to the daemon, then waits. When the daemon hangs up
// it exits, and Rex closes the pane. The ID says which window the daemon
// made the pane for; the launcher's own pane has none. Ported from neorex (MIT).
package panehelper

import (
	"errors"
	"net"
	"os"
	"strconv"
	"syscall"

	"github.com/alpcanaydin/rexmod/internal/editor/fdpass"
	"github.com/alpcanaydin/rexmod/internal/editor/term"
)

// Messages a helper sends the daemon, one byte each.
const (
	// MsgHello comes first, with the terminal's fd attached, then the ID in
	// decimal if any.
	MsgHello byte = 'p'
	// MsgResized: the terminal was resized. Only the helper gets SIGWINCH
	// (it's in the terminal's foreground process group; the daemon isn't).
	MsgResized byte = 'w'
)

var (
	ErrNotATerminal = errors.New("panehelper: stdin is not a terminal")
	ErrBadHello     = errors.New("panehelper: bad hello")
	ErrNoTerminal   = errors.New("panehelper: hello carried no terminal")
)

// Run is the helper: it sends stdin's terminal to the daemon at socketPath,
// forwards resizes, and returns when the daemon hangs up.
func Run(socketPath string, id uint32, hasID bool) error {
	const tty = 0
	if !term.IsTerminal(tty) {
		return ErrNotATerminal
	}
	// before the hello, so a resize in between isn't missed
	sigs := term.NewSignals(syscall.SIGWINCH)
	defer sigs.Stop()

	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := fdpass.Send(conn, HelloBytes(id, hasID), tty); err != nil {
		return err
	}

	hangup := make(chan struct{})
	go func() {
		// nothing is sent this way yet; hang-up is the only message
		buf := make([]byte, 64)
		for {
			if n, err := conn.Read(buf); n == 0 || err != nil {
				close(hangup)
				return
			}
		}
	}()
	for {
		select {
		case <-hangup:
			return nil
		case sig := <-sigs.C:
			var set term.SignalSet
			set.Add(sig)
			set |= sigs.Drain()
			if set.Contains(syscall.SIGWINCH) {
				if _, err := conn.Write([]byte{MsgResized}); err != nil {
					return err
				}
			}
		}
	}
}

// HelloBytes is the hello message: 'p', then the ID if there is one.
func HelloBytes(id uint32, hasID bool) []byte {
	b := []byte{MsgHello}
	if hasID {
		b = strconv.AppendUint(b, uint64(id), 10)
	}
	return b
}

// ParseHello returns the ID in a hello; hasID is false for the launcher's pane.
func ParseHello(b []byte) (id uint32, hasID bool, err error) {
	if len(b) == 0 || b[0] != MsgHello {
		return 0, false, ErrBadHello
	}
	if len(b) == 1 {
		return 0, false, nil
	}
	n, err := strconv.ParseUint(string(b[1:]), 10, 32)
	if err != nil {
		return 0, false, ErrBadHello
	}
	return uint32(n), true, nil
}

// Hello is a helper's connection and the terminal it handed over.
type Hello struct {
	Conn *net.UnixConn
	TTY  *os.File
	// ID is meaningless when HasID is false (the launcher's pane).
	ID    uint32
	HasID bool
}

// Close closes the terminal and the connection.
func (h Hello) Close() {
	h.TTY.Close()
	h.Conn.Close()
}

// AcceptHello is the daemon's side: takes the next helper and its hello.
// Closes both on failure.
func AcceptHello(l *net.UnixListener) (Hello, error) {
	conn, err := l.AcceptUnix()
	if err != nil {
		return Hello{}, err
	}
	buf := make([]byte, 16)
	n, fd, err := fdpass.Recv(conn, buf)
	if err != nil {
		conn.Close()
		return Hello{}, err
	}
	if fd < 0 {
		conn.Close()
		return Hello{}, ErrNoTerminal
	}
	id, hasID, err := ParseHello(buf[:n])
	if err != nil {
		syscall.Close(fd)
		conn.Close()
		return Hello{}, err
	}
	return Hello{Conn: conn, TTY: os.NewFile(uintptr(fd), "tty"), ID: id, HasID: hasID}, nil
}
