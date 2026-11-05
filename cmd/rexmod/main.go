// rexmod glues Rex, git, Neovim and the coding agents Rex supports (Claude
// Code, Codex, OpenCode, Pi) together: it powers the Changes panel, opens
// files in the editor pane, runs the file picker, keeps review comments and
// records agent turns.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const usage = `rexmod: Rex Changes panel, editor and review helpers

  rexmod target   [--session S] [--window W]           agent pane + repo of a window (JSON)
  rexmod state    [--session S] [--window W]           target + all change sets (JSON, for the panel)
  rexmod info     [--session S] [--window W]           agent status, session, plan usage, processes, ports (JSON)
  rexmod changes  [--repo R] [--mode M]                M = last-turn | uncommitted | branch (JSON)
  rexmod files    [--repo R] [--changed] [--host H]    files of the repo, one per line (H: a Rex host)
  rexmod open     [--session S] [--window W] FILE[:LINE]  open in the editor pane
  rexmod pick     [--session S] [--window W]           fuzzy file picker in a floating layer
  rexmod preview  FILE [LINE]                          numbered preview (used by the picker)
  rexmod comments add --file F --line L [--end-line E] [--hold] --text T
  rexmod comments list [--file F] [--pending] [--json]
  rexmod comments edit --id ID --text T              change a comment (it becomes pending again)
  rexmod comments delete --id ID
  rexmod comments resolve --id ID                     the agent clears a note it addressed
  rexmod comments clear [--sent]
  rexmod comments send [--session S] [--window W]      paste pending comments into the agent
  rexmod follow   --session S --window W --block B     follow (and focus) another agent pane of the window
  rexmod hook prompt|stop [--text]                     agent turn hooks (JSON on stdin; --text: plain comments out)
  rexmod warm                                          cache the login environment for the editor pane
  rexmod serve                                         on a remote Rex host: answer the Mac's rexmod over Rex

A session on another Rex host (rex hosts) runs there, through its rexmod serve or over ssh: target,
state, info, files, follow, open, pick and comments send; see scripts/install-remote.sh and
remote_hosts in ~/.config/rexmod/config.json.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if handled, err := routeRemote(os.Args[1], os.Args[2:]); handled {
		if err != nil {
			exitRemote(err)
			fmt.Fprintln(os.Stderr, "rexmod:", err)
			os.Exit(1)
		}
		return
	}
	var err error
	switch os.Args[1] {
	case "target":
		err = cmdTarget(os.Args[2:])
	case "state":
		err = cmdState(os.Args[2:])
	case "changes":
		err = cmdChanges(os.Args[2:])
	case "info":
		err = cmdInfo(os.Args[2:])
	case "files":
		err = cmdFiles(os.Args[2:])
	case "open":
		err = cmdOpen(os.Args[2:])
	case "pick":
		err = cmdPick(os.Args[2:])
	case "preview":
		err = cmdPreview(os.Args[2:])
	case "comments":
		err = cmdComments(os.Args[2:])
	case "hook":
		err = cmdHook(os.Args[2:])
	case "follow":
		err = cmdFollow(os.Args[2:])
	case "editor":
		err = cmdEditor(os.Args[2:])
	case "editor-pane":
		err = cmdEditorPane(os.Args[2:])
	case "editor-daemon":
		err = cmdEditorDaemon(os.Args[2:])
	case "editor-data-copy":
		err = cmdEditorDataCopy(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "remote-events":
		err = cmdRemoteEvents(os.Args[2:])
	case "warm":
		// Refresh the cached login environment if it is stale.
		_ = loginEnv()
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rexmod:", err)
		os.Exit(1)
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// parse lets flags and positional arguments mix in any order.
func parse(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	return pos
}

func targetFlags(fs *flag.FlagSet) (*string, *string) {
	s := fs.String("session", os.Getenv("REX_SESSION"), "Rex session")
	w := fs.String("window", "", "Rex window (default: active)")
	return s, w
}

// cmdFollow makes the panel follow another agent pane of a window: it
// becomes the remembered agent and gets the focus.
func cmdFollow(args []string) error {
	fs := flag.NewFlagSet("follow", flag.ExitOnError)
	s, w := targetFlags(fs)
	block := fs.String("block", "", "agent pane to follow")
	parse(fs, args)
	if *s == "" || *block == "" {
		return fmt.Errorf("follow: --session and --block are required")
	}
	window := *w
	if window == "" {
		t, err := resolveTarget(*s, "")
		if err != nil {
			return err
		}
		window = t.WindowID
	}
	rememberAgent(window, *block)
	_, err := rexRun("focus", "-s", *s, *block)
	return err
}

func cmdTarget(args []string) error {
	fs := flag.NewFlagSet("target", flag.ExitOnError)
	s, w := targetFlags(fs)
	parse(fs, args)
	t, err := resolveTarget(*s, *w)
	if err != nil {
		return err
	}
	return printJSON(t)
}

type State struct {
	Target   *Target             `json:"target"`
	Changes  map[string]*Changes `json:"changes,omitempty"`
	Comments int                 `json:"pending_comments"`
	// Pending lists those comments so the panel can show where they are.
	Pending []PendingComment `json:"comments,omitempty"`
	Error   string           `json:"error,omitempty"`
	Time    string           `json:"time"`
}

// PendingComment is what the panel needs of a pending comment.
type PendingComment struct {
	ID      string `json:"id"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line"`
	Text    string `json:"text"`
}

func cmdState(args []string) error {
	fs := flag.NewFlagSet("state", flag.ExitOnError)
	s, w := targetFlags(fs)
	repo := fs.String("repo", "", "use this repo instead of resolving the agent pane")
	parse(fs, args)
	st := State{Time: time.Now().Format(time.RFC3339)}
	if *repo != "" {
		root, err := repoRoot(*repo)
		if err != nil {
			return err
		}
		st.Target = &Target{Repo: root, RepoName: baseName(root), Branch: currentBranch(root)}
		if gd, err := git(root, "rev-parse", "--absolute-git-dir"); err == nil {
			st.Target.GitDir = strings.TrimSpace(gd)
		}
	} else {
		t, err := resolveTarget(*s, *w)
		if err != nil {
			st.Error = err.Error()
			return printJSON(st)
		}
		st.Target = t
	}
	if st.Target.Repo == "" {
		return printJSON(st)
	}
	now, err := snapshotTree(st.Target.Repo)
	if err != nil {
		st.Error = err.Error()
		return printJSON(st)
	}
	st.Changes = map[string]*Changes{}
	for _, m := range []string{"last-turn", "uncommitted", "branch"} {
		c, err := computeChanges(st.Target.Repo, m, now, st.Target.AgentBlockID)
		if err != nil {
			c = &Changes{Mode: m, Note: err.Error(), Files: []FileChange{}}
		}
		st.Changes[m] = c
	}
	if cs, err := openComments(st.Target.Repo); err == nil {
		alive := aliveBlocks()
		for _, c := range cs.Load() {
			if c.Pending() && c.Owned(st.Target.AgentBlockID, alive) {
				st.Comments++
				st.Pending = append(st.Pending, PendingComment{c.ID, c.File, c.Line, c.EndLine, c.Text})
			}
		}
		cs.Close()
	}
	return printJSON(st)
}

func repoFrom(flagRepo, file string) (string, error) {
	if flagRepo != "" {
		return repoRoot(flagRepo)
	}
	if file != "" {
		dir := file
		if fi, err := os.Stat(file); err != nil || !fi.IsDir() {
			dir = filepath.Dir(file)
		}
		if r, err := repoRoot(dir); err == nil {
			return r, nil
		}
	}
	wd, _ := os.Getwd()
	return repoRoot(wd)
}

func cmdChanges(args []string) error {
	fs := flag.NewFlagSet("changes", flag.ExitOnError)
	repo := fs.String("repo", "", "repository (default: cwd)")
	mode := fs.String("mode", "uncommitted", "last-turn | uncommitted | branch")
	block := fs.String("block", "", "last-turn: the turn of the agent in this Rex pane (default: newest of any)")
	parse(fs, args)
	r, err := repoFrom(*repo, "")
	if err != nil {
		return err
	}
	c, err := computeChanges(r, *mode, "", *block)
	if err != nil {
		return err
	}
	return printJSON(c)
}

func cmdFiles(args []string) error {
	fs := flag.NewFlagSet("files", flag.ExitOnError)
	repo := fs.String("repo", "", "repository (default: cwd)")
	changed := fs.Bool("changed", false, "only files changed on this branch")
	parse(fs, args)
	r, err := repoFrom(*repo, "")
	if err != nil {
		return err
	}
	files, err := listFiles(r, *changed)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		fmt.Println(strings.Join(files, "\n"))
	}
	return nil
}

func cmdOpen(args []string) error {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	s, w := targetFlags(fs)
	line := fs.Int("line", 0, "line to jump to")
	theme := fs.String("rex-theme", "", "Rex's theme from the Mac (on a remote host)")
	pos := parse(fs, args)
	saveRexTheme(*theme)
	if len(pos) == 0 {
		return fmt.Errorf("open: missing file")
	}
	file, l := parseFileLine(pos[0])
	if *line > 0 {
		l = *line
	}
	return openInEditor(*s, *w, file, l)
}

func cmdPick(args []string) error {
	fs := flag.NewFlagSet("pick", flag.ExitOnError)
	s, w := targetFlags(fs)
	inner := fs.Bool("inner", false, "run fzf here (used inside the floating layer)")
	here := fs.Bool("here", false, "resolve the agent pane's repo, then run fzf in this terminal")
	repo := fs.String("repo", "", "repository for --inner")
	theme := fs.String("rex-theme", "", "Rex's theme from the Mac (on a remote host)")
	parse(fs, args)
	saveRexTheme(*theme)
	if *here {
		t, err := resolveTarget(*s, *w)
		if err != nil {
			return err
		}
		if t.Repo == "" {
			return fmt.Errorf("the agent pane of this tab is not in a git repository")
		}
		return pickInner(t.SessionID, t.WindowID, t.Repo)
	}
	if *inner {
		return pickInner(*s, *w, *repo)
	}
	return pickOuter(*s, *w)
}

func cmdPreview(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("preview: missing file")
	}
	line := 0
	if len(args) > 1 {
		fmt.Sscanf(args[1], "%d", &line)
	}
	return preview(args[0], line)
}

func cmdComments(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("comments: add | list | edit | delete | resolve | clear | send")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("comments "+sub, flag.ExitOnError)
	repo := fs.String("repo", "", "repository (default: from --file or cwd)")
	file := fs.String("file", "", "file")
	line := fs.Int("line", 0, "first line")
	end := fs.Int("end-line", 0, "last line")
	text := fs.String("text", "", "comment text (or - for stdin)")
	snippet := fs.String("snippet", "", "code the comment refers to (default: read from the file)")
	targetBlock := fs.String("target-block", "", "Rex pane of the agent the note is for (default: the agent in the editor's tab)")
	hold := fs.Bool("hold", false, "keep the note out of the agent's next message; it goes only when sent from the panel")
	id := fs.String("id", "", "comment id")
	pendingOnly := fs.Bool("pending", false, "only comments not yet sent to the agent")
	sentOnly := fs.Bool("sent", false, "only comments already sent")
	asJSON := fs.Bool("json", false, "JSON output")
	s, w := targetFlags(fs)
	pos := parse(fs, args)

	switch sub {
	case "add":
		if *text == "-" || (*text == "" && len(pos) == 0) {
			b, _ := io.ReadAll(os.Stdin)
			*text = string(b)
		} else if *text == "" {
			*text = strings.Join(pos, " ")
		}
		if *file == "" || *line == 0 {
			return fmt.Errorf("comments add: --file and --line are required")
		}
		r, err := repoFrom(*repo, *file)
		if err != nil {
			return err
		}
		target := *targetBlock
		if target == "" {
			target = editorTarget()
		}
		c, err := addComment(r, *file, *line, *end, *text, *snippet, target, *hold)
		if err != nil {
			return err
		}
		return printJSON(c)

	case "list":
		r, err := repoFrom(*repo, *file)
		if err != nil {
			return err
		}
		st, err := openComments(r)
		if err != nil {
			return err
		}
		all := st.Load()
		st.Close()
		rel := ""
		if *file != "" {
			rel = relToRepo(r, *file)
		}
		out := []Comment{}
		for _, c := range all {
			if rel != "" && c.File != rel {
				continue
			}
			if *pendingOnly && !c.Pending() {
				continue
			}
			out = append(out, c)
		}
		if *asJSON {
			return printJSON(map[string]any{"repo": r, "comments": out})
		}
		for _, c := range out {
			state := "pending"
			if !c.Pending() {
				state = "sent"
			} else if c.Hold {
				state = "held"
			}
			fmt.Printf("%s  %s:%s  [%s]  %s\n", c.ID, c.File, c.Range(), state, c.Text)
		}
		return nil

	case "edit":
		if *text == "-" {
			b, _ := io.ReadAll(os.Stdin)
			*text = string(b)
		}
		if strings.TrimSpace(*text) == "" {
			return fmt.Errorf("comments edit: --text is required (delete the comment instead)")
		}
		r, err := repoFrom(*repo, *file)
		if err != nil {
			return err
		}
		st, err := openComments(r)
		if err != nil {
			return err
		}
		defer st.Close()
		all := st.Load()
		for i := range all {
			if all[i].ID == *id {
				all[i].Text = strings.TrimSpace(*text)
				all[i].SentAt = "" // an edited note goes to the agent again
				return st.Save(all)
			}
		}
		return fmt.Errorf("no comment with id %q", *id)

	case "resolve":
		r, err := repoFrom(*repo, *file)
		if err != nil {
			return err
		}
		return resolveComment(r, *id, os.Getenv("REX_BLOCK"))

	case "delete":
		r, err := repoFrom(*repo, *file)
		if err != nil {
			return err
		}
		st, err := openComments(r)
		if err != nil {
			return err
		}
		defer st.Close()
		all := st.Load()
		kept := all[:0]
		found := false
		for _, c := range all {
			if c.ID == *id {
				found = true
				continue
			}
			kept = append(kept, c)
		}
		if !found {
			return fmt.Errorf("no comment with id %q", *id)
		}
		return st.Save(kept)

	case "clear":
		r, err := repoFrom(*repo, *file)
		if err != nil {
			return err
		}
		st, err := openComments(r)
		if err != nil {
			return err
		}
		defer st.Close()
		if !*sentOnly {
			return st.Save(nil)
		}
		var kept []Comment
		for _, c := range st.Load() {
			if c.Pending() {
				kept = append(kept, c)
			}
		}
		return st.Save(kept)

	case "send":
		return sendComments(*s, *w, *repo, *file)
	}
	return fmt.Errorf("comments: unknown subcommand %q", sub)
}

// sendComments pastes pending comments into the agent's pane and submits
// them. Bracketed paste, then Enter a moment later: every agent Rex supports
// takes that as one message.
func sendComments(session, window, repo, file string) error {
	var agent, sess string
	name := "the agent"
	if session != "" {
		t, err := resolveTarget(session, window)
		if err != nil {
			return err
		}
		agent, sess = t.AgentBlockID, t.SessionID
		if t.Agent != "" {
			name = t.Agent
		}
		if repo == "" && file == "" {
			repo = t.Repo
		}
	}
	r, err := repoFrom(repo, file)
	if err != nil {
		return err
	}
	if agent == "" {
		return fmt.Errorf("no agent pane (Claude Code, Codex, OpenCode or Pi) in this window")
	}
	pending, err := takePending(r, agent, true)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return fmt.Errorf("no pending comments")
	}
	text := formatComments(pending)
	if err := writeBlock(sess, agent, "\x1b[200~"+text+"\x1b[201~"); err != nil {
		return err
	}
	time.Sleep(250 * time.Millisecond)
	if err := writeBlock(sess, agent, "\r"); err != nil {
		return err
	}
	fmt.Printf("sent %d comment(s) to %s\n", len(pending), name)
	return nil
}

func writeBlock(session, block, data string) error {
	b, _ := json.Marshal(map[string]string{"data": data})
	f, err := os.CreateTemp("", "rexmod-write-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	f.Write(b)
	f.Close()
	_, err = rexRun("block", "call", "-s", session, "-b", block, "write", "@"+f.Name())
	return err
}

type hookInput struct {
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	Event          string `json:"hook_event_name"`
	TranscriptPath string `json:"transcript_path"`
	TurnID         string `json:"turn_id"` // Codex only
}

// openCodePane returns the Rex pane the OpenCode TUI recorded for a
// session, waiting up to wait for it to appear.
func openCodePane(session string, wait time.Duration) string {
	path := filepath.Join(cacheDir(), "opencode", sanitize(session))
	deadline := time.Now().Add(wait)
	for {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// cmdHook records agent turns and hands pending review comments to the
// agent. Claude Code and Codex call it as a command hook (UserPromptSubmit,
// Stop) and read hookSpecificOutput JSON; the OpenCode plugin and the Pi
// extension pass --text and add the plain text to the user's message. It
// never fails the turn: problems go to stderr, exit status 0.
func cmdHook(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("hook: prompt | stop")
	}
	text, agent := false, ""
	rest := args[1:]
	for i, a := range rest {
		switch {
		case a == "--text":
			text = true
		case a == "--agent" && i+1 < len(rest):
			agent = rest[i+1]
		}
	}
	var in hookInput
	b, _ := io.ReadAll(os.Stdin)
	_ = json.Unmarshal(b, &in)
	// Codex and OpenCode run hooks in a shared background server, not in the
	// agent's pane: REX_BLOCK there names whichever pane started the server.
	if agent == "" && (in.TurnID != "" || strings.Contains(in.TranscriptPath, "/.codex/")) {
		agent = "codex"
	}
	switch agent {
	case "codex":
		os.Setenv("REX_BLOCK", agentPaneIn("Codex", in.Cwd))
	case "opencode":
		// The TUI half records which pane shows the session.
		pane := ""
		if in.SessionID != "" {
			wait := 0 * time.Millisecond
			if args[0] == "prompt" {
				wait = time.Second // a new session's TUI may not have recorded it yet
			}
			pane = openCodePane(in.SessionID, wait)
		}
		if pane == "" {
			pane = agentPaneIn("OpenCode", in.Cwd)
		}
		os.Setenv("REX_BLOCK", pane)
		// OpenCode 2's plugin context doesn't say where the session works.
		if in.Cwd == "" && pane != "" {
			in.Cwd = paneCwd(pane)
		}
	}
	if in.Cwd == "" {
		in.Cwd, _ = os.Getwd()
	}
	in.Cwd = worktreeInUse(in.Cwd, in.TranscriptPath)
	if agent == "" {
		// Claude Code and Pi run the hook from the agent's own process.
		recordPaneCwd(os.Getenv("REX_BLOCK"), in.Cwd, hookAncestors())
	} else if p := paneForeground(os.Getenv("REX_BLOCK")); p != nil && agentOf(p) != "" {
		// Codex and OpenCode run it from a shared server: name the pane's agent.
		recordPaneCwd(os.Getenv("REX_BLOCK"), in.Cwd, []int{p.Pid})
	}
	repo, err := repoRoot(in.Cwd)
	if err != nil {
		return nil // not a git repo: nothing to record
	}
	switch args[0] {
	case "prompt":
		if err := turnStart(repo, in.SessionID); err != nil {
			fmt.Fprintln(os.Stderr, "rexmod: turn start:", err)
		}
		// Only the notes meant for this agent: hooks inherit the REX_BLOCK
		// of the pane the agent runs in. An agent outside Rex (another
		// terminal or app) gets none of the notes left for a Rex pane.
		block := os.Getenv("REX_BLOCK")
		if block == "" {
			block = outsideRex
		}
		pending, err := takePending(repo, block, false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rexmod: comments:", err)
		}
		if len(pending) > 0 && text {
			fmt.Print(formatComments(pending))
		} else if len(pending) > 0 {
			return printJSON(map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":     "UserPromptSubmit",
					"additionalContext": formatComments(pending),
				},
			})
		}
	case "stop":
		if err := turnStop(repo, in.SessionID); err != nil {
			fmt.Fprintln(os.Stderr, "rexmod: turn stop:", err)
		}
	default:
		return fmt.Errorf("hook: unknown event %q", args[0])
	}
	return nil
}
