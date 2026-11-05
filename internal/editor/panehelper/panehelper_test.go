//go:build darwin || linux

package panehelper

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/alpcanaydin/rexmod/internal/editor/fdpass"
)

func TestHelloCarriesID(t *testing.T) {
	if _, has, err := ParseHello(HelloBytes(0, false)); err != nil || has {
		t.Fatal(has, err)
	}
	if id, has, err := ParseHello(HelloBytes(42, true)); err != nil || !has || id != 42 {
		t.Fatal(id, has, err)
	}
	for _, bad := range []string{"w", "px", ""} {
		if _, _, err := ParseHello([]byte(bad)); err != ErrBadHello {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestAcceptHello(t *testing.T) {
	dir, err := os.MkdirTemp("", "ph")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	r, w, _ := os.Pipe()
	defer r.Close()
	go func() {
		c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			return
		}
		fdpass.Send(c, HelloBytes(7, true), int(w.Fd()))
		w.Close()
		c.Close()
	}()
	h, err := AcceptHello(l)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if !h.HasID || h.ID != 7 {
		t.Fatal(h.ID, h.HasID)
	}
	h.TTY.Write([]byte("ok"))
	buf := make([]byte, 4)
	n, _ := r.Read(buf)
	if string(buf[:n]) != "ok" {
		t.Fatalf("%q", buf[:n])
	}
}
