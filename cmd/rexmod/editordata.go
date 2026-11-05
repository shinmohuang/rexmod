package main

// The editor's data on a local disk. When Neovim's data directory (plugins,
// language servers from Mason) is on a network filesystem, as a shared home
// on a cluster often is, every start reads hundreds of files over the
// network: a cold start took ~1 s more than on a local disk. So rexmod keeps
// a copy on a local disk and points the editor's XDG_DATA_HOME (and
// XDG_CACHE_HOME) at it. The copy is made in the background the first time;
// until it's ready the editor runs from where its data is. Homes on a local
// disk, and "editor_data_dir": "off", are left as they are.

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// networkFS are filesystem types (as /proc/self/mountinfo or statfs name
// them) whose files are read over the network. A FUSE filesystem counts
// only by a subtype known to be remote; plain "fuse" could be a local disk.
var networkFS = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true, "afpfs": true,
	"webdav": true, "davfs": true, "afs": true, "lustre": true, "gpfs": true, "beegfs": true,
	"ceph": true, "9p": true, "glusterfs": true, "fuse.sshfs": true, "fuse.rclone": true,
	"fuse.s3fs": true, "fuse.gcsfuse": true, "fuse.glusterfs": true, "fuse.ceph-fuse": true,
}

func isNetworkFS(fstype string) bool { return networkFS[strings.ToLower(fstype)] }

// fsTypeOf names the filesystem path is on; tests stand in for it.
var fsTypeOf = fsType

// freeSpace is the room left on dir's filesystem; tests stand in for it.
var freeSpace = diskFree

// mountinfoFSType finds, in /proc/self/mountinfo's format, the type of the
// filesystem mounted deepest above path.
func mountinfoFSType(mountinfo, path string) string {
	best, typ := -1, ""
	sc := bufio.NewScanner(strings.NewReader(mountinfo))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		// id parent maj:min root mountpoint options [optional...] - fstype source superopts
		f := strings.Fields(sc.Text())
		sep := -1
		for i, x := range f {
			if x == "-" {
				sep = i
				break
			}
		}
		if len(f) < 5 || sep < 0 || sep+1 >= len(f) {
			continue
		}
		mp := unescapeMount(f[4])
		if !under(path, mp) || len(mp) <= best {
			continue
		}
		best, typ = len(mp), f[sep+1]
	}
	return typ
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// under says whether path is mp or below it.
func under(path, mp string) bool {
	if mp == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == mp || strings.HasPrefix(path, strings.TrimSuffix(mp, "/")+"/")
}

// editorDataSource is Neovim's data directory for the editor's environment
// env: $XDG_DATA_HOME/$NVIM_APPNAME, ~/.local/share/nvim by default (on
// macOS too).
func editorDataSource(env map[string]string) (dataHome, app string) {
	app = env["NVIM_APPNAME"]
	if app == "" {
		app = "nvim"
	}
	dataHome = env["XDG_DATA_HOME"]
	if !filepath.IsAbs(dataHome) {
		home, _ := os.UserHomeDir()
		dataHome = filepath.Join(home, ".local", "share")
	}
	return dataHome, app
}

// editorCacheHome is the editor's XDG_CACHE_HOME, ~/.cache by default.
func editorCacheHome(env map[string]string) string {
	if d := env["XDG_CACHE_HOME"]; filepath.IsAbs(d) {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache")
}

// existingParent is path, or its nearest parent that exists, links
// resolved: what a filesystem type can be asked of.
func existingParent(path string) string {
	for p := path; ; p = filepath.Dir(p) {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return real
		}
		if p == filepath.Dir(p) {
			return p
		}
	}
}

// editorDataRoot is where the local copy goes: editor_data_dir, or
// /var/tmp/rexmod-UID (kept across reboots, and usually a local disk; the
// XDG directories are in the home, and XDG_RUNTIME_DIR is small and goes
// when you log out). "" when it's off.
func editorDataRoot(cfg Config) string {
	switch d := cfg.EditorDataDir; {
	case d == "off":
		return ""
	case filepath.IsAbs(d):
		return d
	case d != "":
		home, _ := os.UserHomeDir()
		if rest, ok := strings.CutPrefix(d, "~/"); ok {
			return filepath.Join(home, rest)
		}
		return "" // a relative path: unclear, so off
	}
	return filepath.Join("/var/tmp", fmt.Sprintf("rexmod-%d", os.Getuid()))
}

// editorDataNote is where the copy says what it did; a variable for tests.
var editorDataNote = func(format string, args ...any) {
	f, err := os.OpenFile(filepath.Join(cacheDir(), "editor-data.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s rexmod: %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// startEditorDataCopy starts the background copy; a variable for tests.
var startEditorDataCopy = func(src, dst string) {
	cmd := exec.Command(selfPath(), "editor-data-copy", src, dst)
	// its own session, and nothing of ours: the open that started it
	// returns (and rexmod serve sees it end) while the copy goes on
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// editorDataEnv is what the editor's environment env gains so its Neovim
// runs from a local copy of its data: nothing when the data isn't on a
// network filesystem, the copy is off, or isn't ready yet (then it is
// started).
func editorDataEnv(env map[string]string, cfg Config) map[string]string {
	root := editorDataRoot(cfg)
	if root == "" {
		return nil
	}
	dataHome, app := editorDataSource(env)
	src := filepath.Join(dataHome, app)
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return nil // nothing to copy (yet)
	}
	real, err := filepath.EvalSymlinks(src)
	if err != nil || !isNetworkFS(fsTypeOf(real)) {
		return nil
	}
	if !ownDir(root) {
		editorDataNote("editor data stays in %s: %s isn't a directory of yours", src, root)
		return nil
	}
	dst := filepath.Join(root, "data", app)
	if st, err := os.Stat(dst); err == nil && st.IsDir() {
		out := map[string]string{
			"XDG_DATA_HOME":   filepath.Join(root, "data"),
			"REXMOD_DATA_DIR": rexmodDataDir(),
		}
		// The cache too, unless it's on a local disk already (an
		// XDG_CACHE_HOME of the user's own, say): theirs stays.
		if cache := editorCacheHome(env); isNetworkFS(fsTypeOf(existingParent(cache))) {
			out["XDG_CACHE_HOME"] = filepath.Join(root, "cache")
			out["REXMOD_CACHE_DIR"] = cacheDir()
		}
		return out
	}
	startEditorDataCopy(src, dst)
	return nil
}

// ownDir makes dir (0700) if needed and says whether it's a directory of
// ours, not someone else's or a link.
func ownDir(dir string) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	fi, err := os.Lstat(dir)
	return err == nil && fi.IsDir() && ownedByMe(fi)
}

// editorDataSpaceFactor: the copy needs this much of the data's size free.
const editorDataSpaceFactor = 1.2

// editorDataRetry: after a copy that couldn't be made, how long until the
// next open tries again.
const editorDataRetry = time.Hour

// cmdEditorDataCopy copies the editor's data from src to dst: into a
// temporary directory beside dst, renamed when complete, so a half copy is
// never used. One copy at a time; a failed one waits editorDataRetry.
func cmdEditorDataCopy(args []string) error {
	if len(args) != 2 || !filepath.IsAbs(args[0]) || !filepath.IsAbs(args[1]) {
		return errors.New("usage: rexmod editor-data-copy SRC DST")
	}
	src, dst := args[0], args[1]
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	unlock, ok := lockFile(filepath.Join(parent, ".copy.lock"))
	if !ok {
		return nil // another copy is on it
	}
	defer unlock()
	if _, err := os.Stat(dst); err == nil {
		return nil // made meanwhile
	}
	failed := filepath.Join(parent, ".copy.failed")
	if st, err := os.Stat(failed); err == nil && time.Since(st.ModTime()) < editorDataRetry {
		return nil
	}
	size, err := dirSize(src)
	if err != nil {
		return err
	}
	free, err := freeSpace(parent)
	if err != nil {
		return err
	}
	if float64(free) < float64(size)*editorDataSpaceFactor {
		editorDataNote("editor data stays in %s: %s has %d MB free, the copy needs %d MB", src, parent, free>>20, int64(float64(size)*editorDataSpaceFactor)>>20)
		_ = os.WriteFile(failed, nil, 0o600)
		return nil
	}
	tmp := fmt.Sprintf("%s.tmp-%d", dst, os.Getpid())
	_ = os.RemoveAll(tmp)
	start := time.Now()
	// -R -P: the tree, links as links. Not -a: a network home's files carry
	// ACLs (NFSv4) a local disk can't take, and cp fails on them; mode bits
	// come along anyway, which is all Neovim's data needs.
	if out, err := exec.Command("cp", "-RP", src, tmp).CombinedOutput(); err != nil {
		_ = os.RemoveAll(tmp)
		_ = os.WriteFile(failed, nil, 0o600)
		editorDataNote("copying the editor data from %s failed: %v: %s", src, err, firstLines(string(out), 3))
		return nil
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	_ = os.Remove(failed)
	editorDataNote("editor data copied from %s to %s (%d MB, %s)", src, dst, size>>20, time.Since(start).Round(time.Second))
	return nil
}

// firstLines is the first n lines of s, for a log.
func firstLines(s string, n int) string {
	lines := strings.SplitN(strings.TrimSpace(s), "\n", n+1)
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, " / ")
}

// dirSize adds up the sizes of the files under dir.
func dirSize(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable: cp will say
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n, err
}

// lockFile takes path as a lock (O_EXCL, our pid in it); a lock whose
// process is gone is taken over.
func lockFile(path string) (unlock func(), ok bool) {
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprint(f, os.Getpid())
			f.Close()
			return func() { _ = os.Remove(path) }, true
		}
		b, _ := os.ReadFile(path)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid > 0 && syscall.Kill(pid, 0) == nil {
			return nil, false
		}
		_ = os.Remove(path) // stale
	}
	return nil, false
}
