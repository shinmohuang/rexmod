package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// rexBin finds the rex CLI. Processes started by the Rex app do not always
// inherit the login shell PATH, so fall back to the server bundle.
func rexBin() string {
	if p, err := exec.LookPath("rex"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	p := home + "/Library/Application Support/com.superlogical.rex/RexServer/Versions/Current/Rex Server.app/Contents/MacOS/rex"
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return "rex"
}

func rexRun(args ...string) ([]byte, error) {
	if out, ok := rexViaAPI(args); ok {
		return out, nil
	}
	return rexCLI(args...)
}

// rexCLI runs the rex CLI.
func rexCLI(args ...string) ([]byte, error) {
	cmd := exec.Command(rexBin(), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("rex %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func rexJSON(v any, args ...string) error {
	out, err := rexRun(args...)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

type rexBlock struct {
	BlockID string `json:"block_id"`
	Label   string `json:"label"`
	Flavor  string `json:"flavor"`
}

type rexLayer struct {
	LayerID string     `json:"layer_id"`
	Kind    string     `json:"kind"`
	Blocks  []rexBlock `json:"blocks"`
}

type rexWindow struct {
	WindowID       string     `json:"window_id"`
	Label          string     `json:"label"`
	FocusedBlockID string     `json:"focused_block_id"`
	Layers         []rexLayer `json:"layers"`
}

type rexView struct {
	SessionID      string      `json:"session_id"`
	Label          string      `json:"label"`
	ActiveWindowID string      `json:"active_window_id"`
	Windows        []rexWindow `json:"windows"`
}

func sessionView(session string) (*rexView, error) {
	if v := takePrimedView(session); v != nil {
		return v, nil
	}
	var v rexView
	if err := rexJSON(&v, "api", "call", "-s", session, "session.view"); err != nil {
		return nil, err
	}
	return &v, nil
}

func (v *rexView) window(id string) *rexWindow {
	if id == "" {
		id = v.ActiveWindowID
	}
	for i := range v.Windows {
		if v.Windows[i].WindowID == id {
			return &v.Windows[i]
		}
	}
	if len(v.Windows) > 0 && id == v.ActiveWindowID {
		return &v.Windows[0]
	}
	return nil
}

func (w *rexWindow) blocks() []rexBlock {
	var out []rexBlock
	for _, l := range w.Layers {
		out = append(out, l.Blocks...)
	}
	return out
}

type procInfo struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Argv0  string `json:"argv0"`
	Cwd    string `json:"cwd"`
	Pid    int    `json:"pid"`
	Invoke string `json:"invoked_path"`
}

type blockProcess struct {
	Child      *procInfo  `json:"child"` // the pane's login process; its child is the shell
	Foreground *procInfo  `json:"foreground"`
	Ancestors  []procInfo `json:"ancestors"`
}

func blockProc(session, block string) (*blockProcess, error) {
	var p blockProcess
	if err := rexJSON(&p, "block", "call", "-s", session, "-b", block, "process"); err != nil {
		return nil, err
	}
	return &p, nil
}

// agentNames maps the process names of the coding agents Rex supports
// ("rex skill status": Claude Code, Codex, OpenCode, Pi) to display names.
// Pi runs on Node and sets its title, so Rex reports it by argv0.
var agentNames = map[string]string{
	"claude":   "Claude",
	"codex":    "Codex",
	"opencode": "OpenCode",
	"pi":       "Pi",
}

// agentOf names the coding agent in the foreground of a pane, or "".
func agentOf(p *procInfo) string {
	if p == nil {
		return ""
	}
	for _, n := range []string{p.Name, p.Argv0, p.Invoke} {
		if n == "" {
			continue
		}
		if a, ok := agentNames[strings.ToLower(strings.TrimPrefix(filepath.Base(n), "-"))]; ok {
			return a
		}
	}
	if strings.Contains(p.Path, "claude-code") {
		return "Claude"
	}
	return ""
}

func isNvim(p *procInfo) bool {
	return p != nil && (p.Name == "nvim" || p.Name == "vim")
}

// Target describes what the panel and the editor act on: the coding agent's
// pane of a Rex window and the git repository it works in.
type Target struct {
	SessionID      string `json:"session_id"`
	SessionLabel   string `json:"session_label"`
	WindowID       string `json:"window_id"`
	FocusedBlockID string `json:"focused_block_id"`
	AgentBlockID   string `json:"agent_block_id,omitempty"`
	Agent          string `json:"agent,omitempty"` // Claude, Codex, OpenCode or Pi
	// Agents lists every agent pane of the window; the panel follows
	// AgentBlockID, the one focused last.
	Agents        []AgentPane `json:"agents,omitempty"`
	EditorBlockID string      `json:"editor_block_id,omitempty"`
	AnchorBlockID string      `json:"anchor_block_id,omitempty"` // where a new editor pane splits from
	Cwd           string      `json:"cwd,omitempty"`
	Repo          string      `json:"repo,omitempty"`
	RepoName      string      `json:"repo_name,omitempty"`
	Branch        string      `json:"branch,omitempty"`
	GitDir        string      `json:"git_dir,omitempty"`
	// Host is the Rex host (rex hosts) the session lives on; empty here.
	// Paths are then that host's.
	Host string `json:"host,omitempty"`
}

// AgentPane is one coding agent running in a pane of the window.
type AgentPane struct {
	BlockID string `json:"block_id"`
	Agent   string `json:"agent"`
	Name    string `json:"name"` // Agent, numbered when the window has two of a kind
	cwd     string
}

// agentPaneIn finds the Rex pane where agent (a display name from
// agentNames) runs in dir: in it, or anywhere in the same repository. For
// agents whose hooks run in a shared background server (Codex, OpenCode),
// whose environment names whichever pane started that server. Several
// matches are ambiguous and give "", so a note never reaches the wrong pane.
func agentPaneIn(agent, dir string) string {
	var list struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	if dir == "" || rexJSON(&list, "ls", "--json") != nil {
		return ""
	}
	root, _ := repoRoot(dir)
	common := commonGitDir(dir)
	type cand struct{ session, block string }
	var cands []cand
	for _, s := range list.Sessions {
		v, err := sessionView(s.SessionID)
		if err != nil {
			continue
		}
		for _, w := range v.Windows {
			for _, b := range w.blocks() {
				if strings.HasSuffix(b.Flavor, "terminal.shell") {
					cands = append(cands, cand{s.SessionID, b.BlockID})
				}
			}
		}
	}
	// 3: in dir; 2: in the same worktree; 1: in another worktree of the
	// same repository (the agent may have moved into one); 0: no.
	score := make([]int, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func(i int, c cand) {
			defer wg.Done()
			p, err := blockProc(c.session, c.block)
			if err != nil || p.Foreground == nil || agentOf(p.Foreground) != agent {
				return
			}
			cwd := p.Foreground.Cwd
			if r := recordedPaneCwd(c.block, p.Foreground.Pid); r != "" {
				cwd = r
			}
			switch {
			case cwd == dir:
				score[i] = 3
			case root != "" && func() bool { r, err := repoRoot(cwd); return err == nil && r == root }():
				score[i] = 2
			case common != "" && commonGitDir(cwd) == common:
				score[i] = 1
			}
		}(i, c)
	}
	wg.Wait()
	for best := 3; best > 0; best-- {
		found := ""
		for i, sc := range score {
			if sc != best {
				continue
			}
			if found != "" {
				return "" // two of them: can't tell which
			}
			found = cands[i].block
		}
		if found != "" {
			return found
		}
	}
	return ""
}

// commonGitDir names the repository dir belongs to, the same for all of
// its worktrees, or "".
func commonGitDir(dir string) string {
	if dir == "" {
		return ""
	}
	out, err := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// paneForeground returns the foreground process of a Rex pane, wherever it
// is, or nil.
func paneForeground(block string) *procInfo {
	var list struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	if block == "" || rexJSON(&list, "ls", "--json") != nil {
		return nil
	}
	for _, s := range list.Sessions {
		v, err := sessionView(s.SessionID)
		if err != nil {
			continue
		}
		for _, w := range v.Windows {
			for _, b := range w.blocks() {
				if sameBlock(b.BlockID, block) {
					if p, err := blockProc(s.SessionID, b.BlockID); err == nil {
						return p.Foreground
					}
					return nil
				}
			}
		}
	}
	return nil
}

// paneCwd returns the working directory of the foreground process in a Rex
// pane, wherever it is, or "".
func paneCwd(block string) string {
	if p := paneForeground(block); p != nil {
		return p.Cwd
	}
	return ""
}

// lastAgentFile remembers, per Rex window, the agent pane focused last, so
// the panel keeps following it while you work in the editor or a shell.
func lastAgentFile(window string) string {
	return filepath.Join(cacheDir(), "agent-"+sanitize(window))
}

func lastAgent(window string) string {
	b, _ := os.ReadFile(lastAgentFile(window))
	return strings.TrimSpace(string(b))
}

func rememberAgent(window, block string) {
	if window == "" || block == "" || lastAgent(window) == block {
		return
	}
	_ = os.WriteFile(lastAgentFile(window), []byte(block+"\n"), 0o644)
}

const (
	editorLabel = "editor"
	pickerLabel = "files"
)

func resolveTarget(session, window string) (*Target, error) {
	if session == "" {
		session = os.Getenv("REX_SESSION")
	}
	if session == "" {
		return nil, fmt.Errorf("no session given (use --session or REX_SESSION)")
	}
	v, err := sessionView(session)
	if err != nil {
		return nil, err
	}
	w := v.window(window)
	if w == nil {
		return nil, fmt.Errorf("window %q not found in session %s", window, session)
	}
	t := &Target{SessionID: v.SessionID, SessionLabel: v.Label, WindowID: w.WindowID, FocusedBlockID: w.FocusedBlockID}

	var focusedCwd, agentCwd, fallbackCwd string
	blocks := w.blocks()
	procs := make([]*blockProcess, len(blocks))
	var wg sync.WaitGroup
	for i, b := range blocks {
		if !strings.HasSuffix(b.Flavor, "terminal.shell") {
			continue
		}
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			if p, err := blockProc(session, id); err == nil {
				procs[i] = p
			}
		}(i, b.BlockID)
	}
	wg.Wait()
	for i, b := range blocks {
		if !strings.HasSuffix(b.Flavor, "terminal.shell") {
			continue
		}
		if b.Label == editorLabel && t.EditorBlockID == "" {
			t.EditorBlockID = b.BlockID
		}
		p := procs[i]
		if p == nil || p.Foreground == nil {
			continue
		}
		fg := p.Foreground
		if b.Label == pickerLabel {
			continue // the picker layer itself
		}
		if b.BlockID == w.FocusedBlockID {
			focusedCwd = fg.Cwd
		}
		if fallbackCwd == "" && fg.Cwd != "" {
			if _, err := repoRoot(fg.Cwd); err == nil {
				fallbackCwd = fg.Cwd
			}
		}
		if a := agentOf(fg); a != "" {
			cwd := fg.Cwd
			if r := recordedPaneCwd(b.BlockID, fg.Pid); r != "" {
				cwd = r // e.g. a Claude Code worktree
			} else if a == "Claude" {
				// No hook since it started (or since rexmod was updated).
				cwd = worktreeInUse(cwd, claudeTranscript(fg.Pid))
			}
			t.Agents = append(t.Agents, AgentPane{BlockID: b.BlockID, Agent: a, Name: a, cwd: cwd})
		}
	}
	// Follow the focused agent pane; with the focus elsewhere, the agent
	// focused last; otherwise the first one.
	if len(t.Agents) > 0 {
		pick, last := 0, lastAgent(w.WindowID)
		for i, a := range t.Agents {
			if a.BlockID == w.FocusedBlockID {
				pick = i
				rememberAgent(w.WindowID, a.BlockID)
				break
			}
			if a.BlockID == last {
				pick = i
			}
		}
		t.AgentBlockID, t.Agent, agentCwd = t.Agents[pick].BlockID, t.Agents[pick].Agent, t.Agents[pick].cwd
		count := map[string]int{}
		for _, a := range t.Agents {
			count[a.Agent]++
		}
		seen := map[string]int{}
		for i, a := range t.Agents {
			if count[a.Agent] > 1 {
				seen[a.Agent]++
				t.Agents[i].Name = fmt.Sprintf("%s %d", a.Agent, seen[a.Agent])
			}
		}
	}
	// A new editor goes next to the agent, else next to the focused tiled pane,
	// never into a floating layer such as the picker.
	tiled := map[string]bool{}
	firstTiled := ""
	for _, l := range w.Layers {
		if l.Kind != "tiled" {
			continue
		}
		for _, b := range l.Blocks {
			tiled[b.BlockID] = true
			if firstTiled == "" && b.Label != editorLabel {
				firstTiled = b.BlockID
			}
		}
	}
	switch {
	case t.AgentBlockID != "" && tiled[t.AgentBlockID]:
		t.AnchorBlockID = t.AgentBlockID
	case tiled[w.FocusedBlockID]:
		t.AnchorBlockID = w.FocusedBlockID
	default:
		t.AnchorBlockID = firstTiled
	}
	t.Cwd = agentCwd
	if t.Cwd == "" {
		t.Cwd = focusedCwd
		if _, err := repoRoot(t.Cwd); t.Cwd == "" || err != nil {
			if fallbackCwd != "" {
				t.Cwd = fallbackCwd
			}
		}
	}
	if t.Cwd != "" {
		if root, err := repoRoot(t.Cwd); err == nil {
			t.Repo = root
			t.RepoName = baseName(root)
			t.Branch = currentBranch(root)
			if gd, err := git(root, "rev-parse", "--absolute-git-dir"); err == nil {
				t.GitDir = strings.TrimSpace(gd)
			}
		}
	}
	return t, nil
}

// aliveBlocks returns the IDs of every open Rex pane, or nil when Rex can't
// be asked. A variable so tests can stand in for Rex.
var aliveBlocks = func() map[string]bool {
	out, err := exec.Command(rexBin(), "block", "ls", "-A", "-q", "--short=false").Output()
	if err != nil {
		return nil
	}
	alive := map[string]bool{}
	for _, l := range strings.Fields(string(out)) {
		alive[l] = true
	}
	return alive
}
