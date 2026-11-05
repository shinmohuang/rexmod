package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mountinfo = `22 1 0:21 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
40 22 0:35 / /home rw,relatime shared:20 - nfs4 staff-fs:/home rw,vers=4.2
41 40 0:36 / /home/me/remote rw,nosuid - fuse.sshfs me@box: rw
42 22 0:37 / /mnt/my\040disk rw - fuse /dev/sdb1 rw
43 22 0:38 / /var/tmp rw - tmpfs tmpfs rw
`

func TestMountinfoFSType(t *testing.T) {
	for path, want := range map[string]string{
		"/home/me/.local/share/nvim": "nfs4",
		"/home":                      "nfs4",
		"/homework":                  "ext4", // not under /home
		"/home/me/remote/x":          "fuse.sshfs",
		"/mnt/my disk/data":          "fuse",
		"/var/tmp/rexmod-1":          "tmpfs",
		"/usr/share":                 "ext4",
	} {
		if got := mountinfoFSType(mountinfo, path); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
	for typ, want := range map[string]bool{"nfs4": true, "fuse.sshfs": true, "cifs": true, "smbfs": true, "lustre": true, "fuse": false, "ext4": false, "apfs": false, "tmpfs": false} {
		if isNetworkFS(typ) != want {
			t.Errorf("isNetworkFS(%s) = %v", typ, !want)
		}
	}
}

func TestEditorDataRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := editorDataRoot(Config{}); filepath.Dir(got) != "/var/tmp" {
		t.Errorf("default %q", got)
	}
	if got := editorDataRoot(Config{EditorDataDir: "off"}); got != "" {
		t.Errorf("off: %q", got)
	}
	if got := editorDataRoot(Config{EditorDataDir: "/scratch/me"}); got != "/scratch/me" {
		t.Errorf("path: %q", got)
	}
	if got := editorDataRoot(Config{EditorDataDir: "~/fast"}); got != filepath.Join(home, "fast") {
		t.Errorf("~/: %q", got)
	}
	if got := editorDataRoot(Config{EditorDataDir: "relative"}); got != "" {
		t.Errorf("relative: %q", got)
	}
}

// editorDataFixture: a data directory of Neovim's, a root for the copy, and
// fsTypeOf saying the data is on typ.
func editorDataFixture(t *testing.T, typ string) (env map[string]string, cfg Config, copies *[][2]string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("REXMOD_CACHE_DIR", "")
	t.Setenv("REXMOD_DATA_DIR", "")
	dataHome := t.TempDir()
	os.MkdirAll(filepath.Join(dataHome, "nvim", "lazy"), 0o755)
	old, oldCopy := fsTypeOf, startEditorDataCopy
	fsTypeOf = func(string) string { return typ }
	var started [][2]string
	startEditorDataCopy = func(src, dst string) { started = append(started, [2]string{src, dst}) }
	t.Cleanup(func() { fsTypeOf, startEditorDataCopy = old, oldCopy })
	return map[string]string{"XDG_DATA_HOME": dataHome}, Config{EditorDataDir: filepath.Join(t.TempDir(), "copy")}, &started
}

func TestEditorDataEnv(t *testing.T) {
	// on a local disk: nothing changes, nothing is copied
	env, cfg, copies := editorDataFixture(t, "ext4")
	if got := editorDataEnv(env, cfg); got != nil || len(*copies) != 0 {
		t.Errorf("local disk: %v %v", got, *copies)
	}

	// on NFS, no copy yet: the editor runs from its data, the copy starts
	env, cfg, copies = editorDataFixture(t, "nfs4")
	if got := editorDataEnv(env, cfg); got != nil {
		t.Errorf("copy not ready, env %v", got)
	}
	src, dst := filepath.Join(env["XDG_DATA_HOME"], "nvim"), filepath.Join(cfg.EditorDataDir, "data", "nvim")
	if len(*copies) != 1 || (*copies)[0] != [2]string{src, dst} {
		t.Fatalf("copies %v", *copies)
	}

	// the copy is there: the editor's XDG dirs point at it, rexmod's own stay
	os.MkdirAll(dst, 0o755)
	got := editorDataEnv(env, cfg)
	if got["XDG_DATA_HOME"] != filepath.Join(cfg.EditorDataDir, "data") || got["XDG_CACHE_HOME"] != filepath.Join(cfg.EditorDataDir, "cache") ||
		got["REXMOD_CACHE_DIR"] != cacheDir() || got["REXMOD_DATA_DIR"] != rexmodDataDir() {
		t.Errorf("copy ready: %v", got)
	}
	// a cache on a local disk of the user's own stays theirs
	localCache := t.TempDir()
	env["XDG_CACHE_HOME"] = localCache
	oldFS := fsTypeOf
	fsTypeOf = func(p string) string {
		if real, _ := filepath.EvalSymlinks(localCache); p == real {
			return "ext4"
		}
		return "nfs4"
	}
	got = editorDataEnv(env, cfg)
	fsTypeOf = oldFS
	if _, moved := got["XDG_CACHE_HOME"]; moved || got["XDG_DATA_HOME"] == "" {
		t.Errorf("local cache: %v", got)
	}
	delete(env, "XDG_CACHE_HOME")

	// off
	cfg.EditorDataDir = "off"
	if got := editorDataEnv(env, cfg); got != nil {
		t.Errorf("off: %v", got)
	}
	// NVIM_APPNAME picks its own directory
	env, cfg, copies = editorDataFixture(t, "nfs")
	env["NVIM_APPNAME"] = "rexmod-nvim"
	os.MkdirAll(filepath.Join(env["XDG_DATA_HOME"], "rexmod-nvim"), 0o755)
	editorDataEnv(env, cfg)
	if len(*copies) != 1 || filepath.Base((*copies)[0][1]) != "rexmod-nvim" {
		t.Errorf("appname copies %v", *copies)
	}
}

func TestEditorDataCopy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
	var notes []string
	old := editorDataNote
	editorDataNote = func(f string, a ...any) { notes = append(notes, f) }
	t.Cleanup(func() { editorDataNote = old })

	src := filepath.Join(t.TempDir(), "nvim")
	os.MkdirAll(filepath.Join(src, "lazy", "a"), 0o755)
	os.WriteFile(filepath.Join(src, "lazy", "a", "init.lua"), []byte("return 1"), 0o644)
	os.Symlink("a", filepath.Join(src, "lazy", "b"))
	dst := filepath.Join(t.TempDir(), "data", "nvim")
	if err := cmdEditorDataCopy([]string{src, dst}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "lazy", "a", "init.lua")); err != nil || string(b) != "return 1" {
		t.Errorf("copied file: %q %v", b, err)
	}
	if l, err := os.Readlink(filepath.Join(dst, "lazy", "b")); err != nil || l != "a" {
		t.Errorf("symlink: %q %v", l, err)
	}
	left, _ := filepath.Glob(dst + ".tmp-*")
	if len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
	// another copy holds the lock: this one leaves it alone
	dst2 := filepath.Join(t.TempDir(), "data", "nvim")
	os.MkdirAll(filepath.Dir(dst2), 0o700)
	os.WriteFile(filepath.Join(filepath.Dir(dst2), ".copy.lock"), []byte(itoa(os.Getpid())), 0o600)
	if err := cmdEditorDataCopy([]string{src, dst2}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst2); err == nil {
		t.Error("copied while locked")
	}
	// a stale lock (no such process) is taken over
	os.WriteFile(filepath.Join(filepath.Dir(dst2), ".copy.lock"), []byte("999999999"), 0o600)
	if err := cmdEditorDataCopy([]string{src, dst2}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst2); err != nil {
		t.Error("stale lock kept the copy from happening")
	}
}

func itoa(n int) string { return fmt.Sprint(n) }

func TestEditorDataCopyNoSpace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
	var notes []string
	oldNote, oldFree := editorDataNote, freeSpace
	editorDataNote = func(f string, a ...any) { notes = append(notes, fmt.Sprintf(f, a...)) }
	freeSpace = func(string) (uint64, error) { return 10, nil } // 10 bytes
	t.Cleanup(func() { editorDataNote, freeSpace = oldNote, oldFree })

	src := filepath.Join(t.TempDir(), "nvim")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "big"), make([]byte, 1000), 0o644)
	dst := filepath.Join(t.TempDir(), "data", "nvim")
	if err := cmdEditorDataCopy([]string{src, dst}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("copied without the room for it")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "MB free") {
		t.Errorf("notes %v", notes)
	}
	// and it doesn't try again on the next open, for a while
	freeSpace = func(string) (uint64, error) { return 1 << 40, nil }
	_ = cmdEditorDataCopy([]string{src, dst})
	if _, err := os.Stat(dst); err == nil {
		t.Error("tried again right away")
	}
}

// Mode bits come along (an executable stays one), without -a's ACLs.
func TestEditorDataCopyKeepsModes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
	src := filepath.Join(t.TempDir(), "nvim")
	os.MkdirAll(filepath.Join(src, "mason", "bin"), 0o755)
	os.WriteFile(filepath.Join(src, "mason", "bin", "marksman"), []byte("#!/bin/sh\n"), 0o755)
	dst := filepath.Join(t.TempDir(), "data", "nvim")
	if err := cmdEditorDataCopy([]string{src, dst}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dst, "mason", "bin", "marksman"))
	if err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Errorf("executable lost its mode: %v %v", st, err)
	}
}
