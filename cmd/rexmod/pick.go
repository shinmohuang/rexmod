package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "rexmod"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// pickOuter opens a floating Rex layer that runs the picker.
func pickOuter(session, window string) error {
	t, err := resolveTarget(session, window)
	if err != nil {
		return err
	}
	if t.Repo == "" {
		return fmt.Errorf("the agent pane of this window is not in a git repository")
	}
	req := map[string]any{
		"session_id": t.SessionID,
		"window_id":  t.WindowID,
		"bounds":     []float64{0.15, 0.1, 0.7, 0.75},
		"focus":      true,
		"layout": map[string]any{
			"block": map[string]any{
				"flavor": "com.superlogical.terminal.shell",
				"label":  "files",
				"options": map[string]any{
					"shell":   "none",
					"cwd":     t.Repo,
					"command": []string{selfPath(), "pick", "--inner", "--session", t.SessionID, "--window", t.WindowID, "--repo", t.Repo},
				},
			},
		},
	}
	b, _ := json.Marshal(req)
	cmd := exec.Command(rexBin(), "api", "call", "-s", t.SessionID, "session.new_layer", "-")
	cmd.Stdin = bytes.NewReader(b)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("new_layer: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// rexBackground returns the appearance Rex draws in, "dark" or "light". Rex
// doesn't answer the terminal's color queries, so read its setting.
func rexBackground() string {
	if runtime.GOOS != "darwin" {
		// a remote host: the appearance the Mac handed over
		if a := savedAppearance(); a == "dark" || a == "light" {
			return a
		}
		return "dark"
	}
	out, _ := exec.Command("defaults", "read", "com.superlogical.rex", "Workspace.appearance").Output()
	if a := strings.TrimSpace(string(out)); a == "dark" || a == "light" {
		return a
	}
	// "system", or never set. The key only exists in dark mode.
	out, _ = exec.Command("defaults", "read", "-g", "AppleInterfaceStyle").Output()
	if strings.TrimSpace(string(out)) == "Dark" {
		return "dark"
	}
	return "light"
}

func fzfColors(background string) string {
	if background == "light" {
		return "light,bg+:#e8e8e8,fg+:#000000,hl:#0077aa,hl+:#0077aa,pointer:#9900cc,marker:#9900cc,prompt:#9900cc,info:#888888,header:#888888,border:#d4d4d4,spinner:#9900cc"
	}
	return "bg+:#161616,fg+:#ffffff,hl:#00e5ff,hl+:#00e5ff,pointer:#bb00ff,marker:#bb00ff,prompt:#bb00ff,info:#555555,header:#555555,border:#2a2a2a,spinner:#bb00ff"
}

func fzfBin() string {
	return toolBin("fzf")
}

// toolBin finds a tool on PATH or where it's usually installed: Homebrew,
// or ~/.local/bin on a remote host (whose Rex panes may not have it on PATH).
func toolBin(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	cands := []string{"/opt/homebrew/bin/" + name, "/usr/local/bin/" + name}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, ".local", "bin", name))
	}
	for _, p := range cands {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

// pickInner runs fzf over the repo's files and opens the choice in the editor.
func pickInner(session, window, repo string) error {
	self := selfPath()
	files, err := listFiles(repo, false)
	if err != nil {
		return err
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	reloadAll := fmt.Sprintf("%s files --repo %s", q(self), q(repo))
	reloadChanged := reloadAll + " --changed"
	args := []string{
		"--layout=reverse", "--info=inline-right", "--prompt=files ❯ ", "--pointer=▌", "--marker=┃",
		"--header=enter open · ctrl-g changed on branch · ctrl-f all files · esc close",
		"--preview", q(self) + " preview {}",
		"--preview-window=right,55%,border-left",
		"--color=" + fzfColors(rexBackground()),
		"--bind", "ctrl-g:reload(" + reloadChanged + ")+change-prompt(changed ❯ )",
		"--bind", "ctrl-f:reload(" + reloadAll + ")+change-prompt(files ❯ )",
		"--scheme=path",
	}
	cmd := exec.Command(fzfBin(), args...)
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(strings.Join(files, "\n") + "\n")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil // esc or ctrl-c: nothing chosen
	}
	choice := strings.TrimSpace(string(out))
	if choice == "" {
		return nil
	}
	return openInEditor(session, window, filepath.Join(repo, choice), 0)
}

// previewScript is nvim/preview.lua next to the rexmod checkout.
func previewScript() string {
	// <prefix>/bin/rexmod next to <prefix>/nvim, in an install or a checkout
	// (build/rexmod next to nvim/).
	cands := []string{filepath.Join(filepath.Dir(selfPath()), "..", "nvim", "preview.lua")}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, "Library", "Application Support", "rexmod", "nvim", "preview.lua"))
	}
	if runtime.GOOS != "darwin" {
		cands = append(cands, filepath.Join(rexmodDataDir(), "nvim", "preview.lua")) // a remote host
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// preview prints a file for the fzf preview pane, highlighted the way the
// editor highlights it (treesitter or Vim syntax with the rex colorscheme).
func preview(path string, focus int) error {
	if script := previewScript(); script != "" {
		cmd := exec.Command(nvimBin(), "--headless", "-u", "NONE", "-i", "NONE", "-l", script, path, fmt.Sprint(focus), "400")
		cmd.Env = append(os.Environ(), "NVIM_LOG_FILE=/dev/null", "REXMOD_BACKGROUND="+rexBackground(), "REXMOD_REX_THEME="+rexThemePath())
		for k, v := range nvimEnv() {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			_, err = os.Stdout.Write(out)
			return err
		}
	}
	return plainPreview(path, focus)
}

func plainPreview(path string, focus int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 8000)
	n, _ := f.Read(head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		fmt.Println("(binary file)")
		return nil
	}
	_, _ = f.Seek(0, 0)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for i := 1; sc.Scan() && i <= 500; i++ {
		mark := "\x1b[38;5;240m"
		if i == focus {
			mark = "\x1b[38;2;187;0;255m"
		}
		fmt.Fprintf(w, "%s%5d\x1b[0m  %s\n", mark, i, sc.Text())
	}
	return nil
}
