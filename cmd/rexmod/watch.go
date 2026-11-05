package main

// Change detection for rexmod serve: the repositories the service was asked
// about get a fingerprint every repoScanEvery (slow ones less often), made
// here on the host, so finding a change costs no network. git status says
// which files differ; their sizes and mtimes catch a file edited again;
// .git/rexmod holds turns and comments; the index catches staging.

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	repoScanEvery = 500 * time.Millisecond
	repoSlowEvery = 2 * time.Second
	repoSlowCost  = 300 * time.Millisecond
	repoIdle      = 10 * time.Minute
)

type watchedRepo struct {
	gitDir string
	print  string
	asked  time.Time // last request about it
	due    time.Time // next scan
}

type repoWatch struct {
	mu      sync.Mutex
	repos   map[string]*watchedRepo
	changed func()
	// fingerprint is a variable so tests can count or stub scans.
	fingerprint func(repo, gitDir string) string
}

func newRepoWatch(changed func()) *repoWatch {
	return &repoWatch{repos: map[string]*watchedRepo{}, changed: changed, fingerprint: repoFingerprint}
}

// track notes a request about repo; a new one starts from its present state.
func (w *repoWatch) track(repo string, now time.Time) {
	w.mu.Lock()
	r, ok := w.repos[repo]
	w.mu.Unlock()
	if ok {
		w.mu.Lock()
		r.asked = now
		w.mu.Unlock()
		return
	}
	gd, err := git(repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return
	}
	r = &watchedRepo{gitDir: strings.TrimSpace(gd), asked: now, due: now.Add(repoScanEvery)}
	r.print = w.fingerprint(repo, r.gitDir)
	w.mu.Lock()
	w.repos[repo] = r
	w.mu.Unlock()
}

// scan fingerprints the repositories that are due, drops idle ones, and
// calls changed once if any differ.
func (w *repoWatch) scan(now time.Time) {
	w.mu.Lock()
	type job struct {
		repo string
		r    *watchedRepo
	}
	var jobs []job
	for repo, r := range w.repos {
		if now.Sub(r.asked) > repoIdle {
			delete(w.repos, repo)
			continue
		}
		if !now.Before(r.due) {
			jobs = append(jobs, job{repo, r})
		}
	}
	w.mu.Unlock()
	any := false
	for _, j := range jobs {
		t0 := time.Now()
		p := w.fingerprint(j.repo, j.r.gitDir)
		every := repoScanEvery
		if time.Since(t0) > repoSlowCost {
			every = repoSlowEvery
		}
		w.mu.Lock()
		j.r.due = now.Add(every)
		if p != "" && p != j.r.print {
			j.r.print = p
			any = true
		}
		w.mu.Unlock()
	}
	if any && w.changed != nil {
		w.changed()
	}
}

func (w *repoWatch) watching(repo string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.repos[repo]
	return ok
}

// repoFingerprint is a hash of what the panel shows of repo, or "" when git
// fails (a scan then changes nothing).
func repoFingerprint(repo, gitDir string) string {
	st, err := git(repo, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all")
	if err != nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(st))
	for _, p := range statusPaths(st) {
		if fi, err := os.Lstat(filepath.Join(repo, p)); err == nil {
			fmt.Fprintf(h, "\x00%s %d %d", p, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	if fi, err := os.Stat(filepath.Join(gitDir, "index")); err == nil {
		fmt.Fprintf(h, "\x00index %d %d", fi.Size(), fi.ModTime().UnixNano())
	}
	_ = filepath.WalkDir(filepath.Join(gitDir, "rexmod"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			fmt.Fprintf(h, "\x00%s %d %d", p, fi.Size(), fi.ModTime().UnixNano())
		}
		return nil
	})
	return fmt.Sprintf("%x", h.Sum(nil))
}

// statusPaths returns the paths in git status --porcelain=v2 -z output:
// changed (1), renamed or copied (2, whose original path follows as an
// entry of its own), unmerged (u) and untracked (?).
func statusPaths(st string) []string {
	var out []string
	entries := strings.Split(st, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 2 {
			continue
		}
		var fields int
		switch e[:2] {
		case "1 ":
			fields = 9
		case "2 ":
			fields = 10
			i++ // the original path
		case "u ":
			fields = 11
		case "? ":
			out = append(out, e[2:])
			continue
		default:
			continue
		}
		if f := strings.SplitN(e, " ", fields); len(f) == fields {
			out = append(out, f[fields-1])
		}
	}
	return out
}
