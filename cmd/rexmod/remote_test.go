package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The remote command must reach rexmod with every argument intact, whatever
// it holds: run it through a real shell and compare.
func TestRemoteCommandQuoting(t *testing.T) {
	args := []string{"comments", "send", "--session", "my session", "it's", `$HOME`, "a\"b", "back\\slash", "", "*"}
	echo := filepath.Join(t.TempDir(), "argv")
	if err := os.WriteFile(echo, []byte("#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", "-c", remoteCommand(echo, args)).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if !reflect.DeepEqual(got, args) {
		t.Errorf("args after the shell:\n got %q\nwant %q", got, args)
	}
}

func TestRemoteCommandHome(t *testing.T) {
	if got := remoteCommand("~/.local/bin/rexmod", []string{"state"}); got != `~/'.local/bin/rexmod' 'state'` {
		t.Errorf("got %s", got)
	}
	if got := remoteCommand("/opt/rex mod", nil); got != `'/opt/rex mod'` {
		t.Errorf("got %s", got)
	}
}

func TestSSHArgs(t *testing.T) {
	a := sshArgs("acps-gpu2", "~/.local/bin/rexmod", []string{"state", "--session", "s"})
	joined := strings.Join(a, " ")
	for _, want := range []string{"BatchMode=yes", "ConnectTimeout=", "ControlMaster=auto", "ControlPath=", "%C", "ControlPersist="} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh args lack %s: %v", want, a)
		}
	}
	if n := len(a); a[n-2] != "acps-gpu2" || a[n-1] != `~/'.local/bin/rexmod' 'state' '--session' 's'` {
		t.Errorf("destination and command: %q", a[n-2:])
	}
}

func TestClassify(t *testing.T) {
	bin := "~/.local/bin/rexmod"
	if err := classify("gpu2", "acps-gpu2", bin, "", 0); err != nil {
		t.Errorf("status 0: %v", err)
	}
	if err := classify("gpu2", "acps-gpu2", bin, "Permission denied (publickey).", 255); err == nil ||
		!strings.Contains(err.Error(), "can't reach Rex host gpu2") || !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("255: %v", err)
	}
	if err := classify("gpu2", "acps-gpu2", bin, "bash: line 1: /home/u/.local/bin/rexmod: No such file or directory", 127); err == nil ||
		!strings.Contains(err.Error(), "isn't installed on Rex host gpu2") {
		t.Errorf("127: %v", err)
	}
	err := classify("gpu2", "acps-gpu2", bin, "rexmod: window \"w\" not found\n", 1)
	var re *remoteExit
	if !errors.As(err, &re) || re.Code != 1 || err.Error() != `window "w" not found` {
		t.Errorf("remote failure: %#v", err)
	}
}

// fakeSSH stands in a script for ssh: it prints its arguments, one per
// line, and exits with $FAKE_SSH_EXIT, after $FAKE_SSH_STDERR on stderr.
func fakeSSH(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\nprintf '%s' \"$FAKE_SSH_STDERR\" >&2\nexit ${FAKE_SSH_EXIT:-0}\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := sshBin
	sshBin = p
	t.Cleanup(func() { sshBin = old })
	t.Setenv("HOME", t.TempDir()) // no user config
}

func TestRunRemote(t *testing.T) {
	fakeSSH(t)
	out, err := runRemote("gpu2", []string{"state", "--session", "s"})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if lines[len(lines)-2] != "gpu2" { // not configured: the label is the destination
		t.Errorf("destination %q", lines[len(lines)-2])
	}

	t.Setenv("FAKE_SSH_EXIT", "3")
	t.Setenv("FAKE_SSH_STDERR", "rexmod: boom")
	var re *remoteExit
	if _, err := runRemote("gpu2", []string{"state"}); !errors.As(err, &re) || re.Code != 3 {
		t.Errorf("remote failure: %v", err)
	}

	// A dead master is retried once without it, then reported.
	t.Setenv("FAKE_SSH_EXIT", "255")
	t.Setenv("FAKE_SSH_STDERR", "Control socket connect(/x): Connection refused")
	if _, err := runRemote("gpu2", []string{"state"}); err == nil || !strings.Contains(err.Error(), "can't reach") {
		t.Errorf("unreachable: %v", err)
	}
}
