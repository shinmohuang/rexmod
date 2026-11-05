//go:build darwin || linux

package main

// The editor: one headless Neovim behind Rex panes, each Neovim window in
// its own Rex pane. Ported from neorex (MIT): src/daemon/launch.zig and
// serve.zig.

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alpcanaydin/rexmod/internal/editor/daemon"
	"github.com/alpcanaydin/rexmod/internal/editor/nvim"
	"github.com/alpcanaydin/rexmod/internal/editor/panehelper"
	"github.com/alpcanaydin/rexmod/internal/editor/rex"
	"github.com/alpcanaydin/rexmod/internal/editor/term"
)

// Longest path a Unix socket takes on macOS, less one for the terminator.
const maxSockPath = 103

// editorBase is $TMPDIR/rexmod-ed-PID, which every daemon file is named from.
func editorBase() (string, error) {
	tmp := strings.TrimRight(os.TempDir(), "/")
	base := fmt.Sprintf("%s/rexmod-ed-%d", tmp, os.Getpid())
	if len(base)+len(".nvim.sock") > maxSockPath {
		base = fmt.Sprintf("/tmp/rexmod-ed-%d", os.Getpid())
	}
	return base, nil
}

// cmdEditor opens Neovim in this pane: starts the daemon in the
// background, then hands it this terminal and waits as its pane helper.
func cmdEditor(args []string) error {
	if !term.IsTerminal(0) {
		return errors.New("editor: stdin isn't a terminal")
	}
	if os.Getenv("REX_SERVER") == "" || os.Getenv("REX_SESSION") == "" {
		return errors.New("editor: run it in a Rex pane")
	}
	base, err := editorBase()
	if err != nil {
		return err
	}
	logf, err := os.Create(base + ".log")
	if err != nil {
		return err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command(selfPath(), append([]string{"editor-daemon", base}, args...)...)
	cmd.Stdout, cmd.Stderr = w, logf
	// out of the pane's session, so closing the pane doesn't signal it
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	w.Close()
	logf.Close()
	if err != nil {
		return err
	}
	// one byte once it's listening; nothing if it failed
	b := make([]byte, 1)
	n, _ := r.Read(b)
	r.Close()
	if n != 1 {
		return fmt.Errorf("editor: the daemon didn't start; see %s.log", base)
	}
	_ = cmd.Process.Release()
	return panehelper.Run(base+".sock", 0, false)
}

// cmdEditorPane is the helper each of the editor's Rex panes runs.
func cmdEditorPane(args []string) error {
	switch len(args) {
	case 1:
		return panehelper.Run(args[0], 0, false)
	case 2:
		id, err := strconv.ParseUint(args[1], 10, 32)
		if err != nil {
			return err
		}
		return panehelper.Run(args[0], uint32(id), true)
	}
	return errors.New("usage: rexmod editor-pane SOCKET [ID]")
}

// listenArg is the address in a --listen among Neovim's args.
func listenArg(args []string) string {
	for i, a := range args {
		if a == "--listen" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// cmdEditorDaemon is the background half of `rexmod editor`.
func cmdEditorDaemon(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: rexmod editor-daemon BASE [nvim args]")
	}
	base, nvimArgs := args[0], args[1:]
	log.SetFlags(log.Ltime)
	sock := base + ".sock"
	rexURL, sessionID := os.Getenv("REX_SERVER"), os.Getenv("REX_SESSION")
	if rexURL == "" || sessionID == "" {
		return errors.New("not in Rex")
	}
	_ = os.Remove(sock)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return err
	}
	defer os.Remove(sock)
	defer l.Close()

	// one byte on stdout says we're listening; then stdout goes nowhere
	if _, err := os.Stdout.Write([]byte("r")); err != nil {
		return err
	}
	if dn, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		_ = dupTo(int(dn.Fd()), 1)
		dn.Close()
	}

	launch, err := panehelper.AcceptHello(l)
	if err != nil {
		return fmt.Errorf("no pane: %w", err)
	}

	nvimSock := listenArg(nvimArgs)
	argv := []string{"--headless"}
	if nvimSock == "" {
		nvimSock = base + ".nvim.sock"
		argv = append(argv, "--listen", nvimSock)
	}
	argv = append(argv, nvimArgs...)
	_ = os.Remove(nvimSock)
	nv := exec.Command(nvimBin(), argv...)
	nv.Stderr = os.Stderr // the log
	nv.Env = append(os.Environ(), "REXMOD_MULTIGRID=1")
	if err := nv.Start(); err != nil {
		launch.Close()
		return err
	}
	// last, after the terminals are restored. Neovim told to quit (below)
	// gets a moment; otherwise SIGHUP makes it save swap files and exit.
	exited := make(chan struct{})
	go func() { _, _ = nv.Process.Wait(); close(exited) }()
	defer func() {
		select {
		case <-exited:
			return
		case <-time.After(time.Second):
		}
		_ = nv.Process.Signal(syscall.SIGHUP)
		<-exited
	}()

	nc, err := nvim.ConnectRetry(nvimSock)
	if err != nil {
		launch.Close()
		return err
	}
	defer nc.Close()
	// With nothing unsaved, quit cleanly: a hang-up would leave every
	// buffer's swap file behind, and the next open would warn about it.
	defer func() {
		_, _ = nc.ExecLua(`for _, b in ipairs(vim.api.nvim_list_bufs()) do
  if vim.bo[b].modified and vim.bo[b].buftype == '' then return end
end
vim.cmd('qa!')`)
		_ = nc.Flush()
	}()
	rc, err := rex.Connect(rexURL)
	if err != nil {
		launch.Close()
		return err
	}
	defer rc.Close()

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	block := os.Getenv("REX_BLOCK")
	if _, err := rex.Block(block); err != nil {
		block = ""
	}
	d := daemon.New(selfPath(), sock, []string{"editor-pane"}, nc, rex.NewSession(rc, sessionID))
	why, err := d.Run(launch, block, l, sigs)
	if err != nil {
		return err
	}
	log.Printf("rexmod editor: %v", why)
	// a clean end needs no log
	_ = os.Remove(base + ".log")
	return nil
}
