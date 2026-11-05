package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type prewarmRuns struct {
	mu   sync.Mutex
	dirs []string
	done chan struct{}
}

func testPrewarmer(want bool) (*prewarmer, *prewarmRuns) {
	r := &prewarmRuns{done: make(chan struct{}, 10)}
	p := newPrewarmer()
	p.wanted = func() ([]string, bool) { return nil, want }
	p.run = func(dir string, _ []string) {
		r.mu.Lock()
		r.dirs = append(r.dirs, dir)
		r.mu.Unlock()
	}
	return p, r
}

// wait until the prewarmer has nothing running
func (p *prewarmer) idle(t *testing.T) {
	t.Helper()
	for range 200 {
		p.mu.Lock()
		running := p.running
		p.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("prewarm still running")
}

func TestPrewarmLimits(t *testing.T) {
	p, r := testPrewarmer(true)
	now := time.Now()
	p.asked("/r", now)
	p.idle(t)
	p.asked("/r", now.Add(time.Minute)) // too soon
	p.idle(t)
	p.asked("/other", now.Add(time.Minute))
	p.idle(t)
	p.asked("/r", now.Add(prewarmEvery+time.Second))
	p.idle(t)
	if want := []string{"/r", "/other", "/r"}; len(r.dirs) != 3 || r.dirs[0] != want[0] || r.dirs[1] != want[1] || r.dirs[2] != want[2] {
		t.Errorf("prewarmed %v, want %v", r.dirs, want)
	}

	// one at a time: asked while one runs, nothing more starts
	block := make(chan struct{})
	p2, _ := testPrewarmer(true)
	n := 0
	p2.run = func(string, []string) { n++; <-block }
	p2.asked("/a", now)
	time.Sleep(5 * time.Millisecond)
	p2.asked("/b", now)
	close(block)
	p2.idle(t)
	if n != 1 {
		t.Errorf("%d prewarms ran at once", n)
	}

	// not wanted: nothing runs
	p3, r3 := testPrewarmer(false)
	p3.asked("/r", now)
	p3.idle(t)
	if len(r3.dirs) != 0 {
		t.Errorf("unwanted prewarm ran: %v", r3.dirs)
	}
}

func TestPrewarmWanted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("REXMOD_CACHE_DIR", "")
	os.MkdirAll(filepath.Join(home, ".local", "share", "nvim"), 0o755)
	// a login environment as cached, so no shell runs
	os.MkdirAll(cacheDir(), 0o755)
	os.WriteFile(loginEnvPath(), []byte(`{"PATH":"/usr/bin:/bin"}`), 0o644)
	cfgDir := filepath.Join(home, ".config", "rexmod")
	os.MkdirAll(cfgDir, 0o755)
	setCfg := func(s string) { os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(s), 0o644) }

	oldFS, oldCopy := fsTypeOf, startEditorDataCopy
	t.Cleanup(func() { fsTypeOf, startEditorDataCopy = oldFS, oldCopy })
	startEditorDataCopy = func(string, string) {}

	fsTypeOf = func(string) string { return "ext4" }
	setCfg(`{}`)
	if _, ok := prewarmWanted(); ok {
		t.Error("local disk, auto: prewarmed")
	}
	setCfg(`{"editor_prewarm":"on"}`)
	if _, ok := prewarmWanted(); !ok {
		t.Error(`"on": not prewarmed`)
	}
	fsTypeOf = func(string) string { return "nfs4" }
	setCfg(`{"editor_data_dir":"off"}`)
	if _, ok := prewarmWanted(); !ok {
		t.Error("NFS without a copy, auto: not prewarmed")
	}
	setCfg(`{"editor_data_dir":"off","editor_prewarm":"off"}`)
	if _, ok := prewarmWanted(); ok {
		t.Error(`"off": prewarmed`)
	}
	// the copy is ready (and on a local disk): auto doesn't prewarm
	tmp, _ := filepath.EvalSymlinks(t.TempDir()) // /var is /private/var on macOS
	copyRoot := filepath.Join(tmp, "copy")
	os.MkdirAll(filepath.Join(copyRoot, "data", "nvim"), 0o755)
	fsTypeOf = func(p string) string {
		if len(p) >= len(copyRoot) && p[:len(copyRoot)] == copyRoot {
			return "ext4"
		}
		return "nfs4"
	}
	setCfg(`{"editor_data_dir":"` + copyRoot + `"}`)
	if _, ok := prewarmWanted(); ok {
		t.Error("copy ready, auto: prewarmed")
	}
}
