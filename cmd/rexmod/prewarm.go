package main

// Prewarming: where the editor's data is slow to read (a network filesystem,
// and no local copy of it yet or at all), rexmod serve loads the editor once
// in the background when the panel starts following a repository, so the
// first editor that opens reads from a warm cache. Language servers aren't
// started: on a shared machine they cost too much for a maybe.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const (
	prewarmEvery   = 10 * time.Minute // per repository, at most
	prewarmTimeout = 15 * time.Second
)

type prewarmer struct {
	mu      sync.Mutex
	last    map[string]time.Time
	running bool
	// run loads the editor with env in dir; tests stand in for it.
	run func(dir string, env []string)
	// wanted says whether to prewarm now; tests stand in for it.
	wanted func() (env []string, ok bool)
}

func newPrewarmer() *prewarmer {
	return &prewarmer{last: map[string]time.Time{}, run: runPrewarm, wanted: prewarmWanted}
}

// asked notes a request about repo and prewarms for it when it's due.
func (p *prewarmer) asked(repo string, now time.Time) {
	p.mu.Lock()
	if p.running || now.Sub(p.last[repo]) < prewarmEvery {
		p.mu.Unlock()
		return
	}
	p.last[repo] = now
	p.running = true
	p.mu.Unlock()
	go func() {
		defer func() { p.mu.Lock(); p.running = false; p.mu.Unlock() }()
		if env, ok := p.wanted(); ok {
			p.run(repo, env)
		}
	}()
}

// prewarmWanted decides, with editor_prewarm, and gives the editor's
// environment: "on" always, "off" never, unset when the data the editor
// would start from is on a network filesystem.
func prewarmWanted() ([]string, bool) {
	cfg := loadConfig()
	if cfg.EditorPrewarm == "off" {
		return nil, false
	}
	env := loginEnv()
	for k, v := range nvimEnv() {
		env[k] = v
	}
	// also starts the local copy, if it's due
	for k, v := range editorDataEnv(env, cfg) {
		env[k] = v
	}
	if cfg.EditorPrewarm != "on" {
		dataHome, app := editorDataSource(env)
		real, err := filepath.EvalSymlinks(filepath.Join(dataHome, app))
		if err != nil || !isNetworkFS(fsTypeOf(real)) {
			return nil, false
		}
	}
	list := os.Environ()
	for _, kv := range envArgs(env) {
		list = append(list, kv) // later ones win
	}
	return list, true
}

func runPrewarm(dir string, env []string) {
	ctx, cancel := context.WithTimeout(context.Background(), prewarmTimeout)
	defer cancel()
	// no swap file, no shada: nothing of it stays
	cmd := exec.CommandContext(ctx, nvimBin(), "--headless", "-n", "-i", "NONE", "+doautocmd User VeryLazy", "+qa!")
	cmd.Dir, cmd.Env = dir, env
	_ = cmd.Run()
}
