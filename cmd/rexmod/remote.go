package main

// Remote hosts: a Rex session can live on another machine's Rex server (rex
// hosts, reached over Tailscale). Its agent, repository and hooks are there
// too, so rexmod runs there: this side finds the session's host and runs the
// same command on it over ssh, where rexmod asks that host's own Rex server
// and git. Rex's API can't run a command and hand back its output, hence ssh.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sshBin is the ssh client; tests stand in a script.
var sshBin = "ssh"

// sshOptions never prompt (BatchMode), bound the connection time, and share
// one connection per host (ControlMaster) so a refresh costs one round trip,
// not a handshake. %C keeps the socket path short.
func sshOptions() []string {
	return []string{
		"-T", "-n",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + filepath.Join(cacheDir(), "ssh", "%C"),
		"-o", "ControlPersist=10m",
	}
}

// shQuote quotes s for a POSIX shell, which is how ssh hands the command on.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteCommand is the command line ssh runs on the host: bin with args, each
// quoted. A leading ~/ in bin stays unquoted so the remote shell expands it.
func remoteCommand(bin string, args []string) string {
	var b strings.Builder
	if rest, ok := strings.CutPrefix(bin, "~/"); ok {
		b.WriteString("~/" + shQuote(rest))
	} else {
		b.WriteString(shQuote(bin))
	}
	for _, a := range args {
		b.WriteString(" " + shQuote(a))
	}
	return b.String()
}

// sshArgs is the full ssh argument list for running rexmod args on dest.
func sshArgs(dest, bin string, args []string) []string {
	return append(sshOptions(), dest, remoteCommand(bin, args))
}

// remoteExit is a remote rexmod that ran and failed: its stderr and exit
// code go to the caller as they are.
type remoteExit struct {
	Code   int
	Stderr string
}

func (e *remoteExit) Error() string {
	if s := strings.TrimSpace(e.Stderr); s != "" {
		return strings.TrimPrefix(s, "rexmod: ")
	}
	return fmt.Sprintf("remote rexmod exited with status %d", e.Code)
}

// runRemote runs rexmod with args on the Rex host label and returns its
// stdout. Failures say which: ssh couldn't connect, rexmod isn't installed
// there, or rexmod ran and failed (a *remoteExit).
func runRemote(label string, args []string) ([]byte, error) {
	dest, bin := loadConfig().remote(label)
	_ = os.MkdirAll(filepath.Join(cacheDir(), "ssh"), 0o700) // ssh won't make ControlPath's dir
	out, stderr, code, err := sshExec(sshArgs(dest, bin, args))
	if code == 255 && staleControl(stderr) {
		// A dead master's socket: go around it once.
		opts := append(sshOptions(), "-o", "ControlPath=none", dest, remoteCommand(bin, args))
		out, stderr, code, err = sshExec(opts)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: can't run ssh: %v", label, err)
	}
	return out, classify(label, dest, bin, stderr, code)
}

// classify turns ssh's exit status into the error the caller sees.
func classify(label, dest, bin, stderr string, code int) error {
	switch {
	case code == 0:
		return nil
	case code == 255:
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = "connection failed"
		}
		return fmt.Errorf("can't reach Rex host %s over ssh (%s): %s; it needs a login that doesn't prompt, such as a key", label, dest, msg)
	case code == 127 || code == 126 && strings.Contains(stderr, bin):
		return fmt.Errorf("rexmod isn't installed on Rex host %s (%s): expected at %s", label, dest, bin)
	}
	return &remoteExit{Code: code, Stderr: stderr}
}

func staleControl(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "controlsocket") || strings.Contains(s, "mux_client") || strings.Contains(s, "control socket")
}

// sshExec runs ssh and returns stdout, stderr and the exit code; err is only
// for an ssh that couldn't start.
func sshExec(args []string) (stdout []byte, stderr string, code int, err error) {
	cmd := exec.Command(sshBin, args...)
	var out, eb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &eb
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.Bytes(), eb.String(), ee.ExitCode(), nil
	}
	if err != nil {
		return nil, eb.String(), -1, err
	}
	return out.Bytes(), eb.String(), 0, nil
}
