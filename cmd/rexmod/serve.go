package main

// rexmod serve: the rexmod of a remote Rex host, kept running. It is a
// client of that host's Rex server and declares one action, rexmod.run,
// which another client (the Mac's rexmod, through rex -S) calls with
// client.perform to run an allow-listed rexmod command here and get its
// output back. It also watches the repositories it was asked about and, on
// a change, bumps its client label: every label change is a client_changed
// event, which the panel follows (rex events --all) to refresh at once.
// Who may call is Rex's business: the server forwards requests only from
// clients it admits.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"time"

	"github.com/alpcanaydin/rexmod/internal/editor/rex"
)

// serveRunTimeout bounds one command; a variable for tests.
var serveRunTimeout = 20 * time.Second

const (
	serveAction = "rexmod.run"
	serveLabel  = "rexmod-serve"
	serveProto  = 1
	// Room for the reply envelope (call_id, result keys) under the limit.
	serveEnvelope = 4096
)

// serveRex is the part of the Rex connection the service uses.
type serveRex interface {
	Clients() ([]rex.ClientInfo, error)
	SetLabel(label string) error
	DeclareActions(actions ...rex.Action) error
	Reply(callID string, result any) error
	ReplyError(callID, code, message string) error
	DetachBlock(session, block string) error
	Sessions() ([]rex.SessionInfo, error)
	SessionBlocks(session string) ([]string, error)
	DestroySession(session string) error
}

// serveSession is the label of the session the Mac starts the service in.
const serveSession = "rexmod"

// server is one running service.
type server struct {
	rex      serveRex
	events   <-chan *rex.Event
	done     <-chan struct{}
	maxMsg   int
	id       string // our client ID
	seq      int    // label generation; each bump is an event
	run      func(argv []string) (stdout, stderr string, code int, err error)
	watch    *repoWatch
	exe      string // our binary, to notice an upgrade
	exeMtime time.Time
	session  string // our pane's session and block, when started in one
	block    string
}

// serveReply is what rexmod.run answers.
type serveReply struct {
	Proto   int    `json:"proto"`
	Version string `json:"version"`
	Stdout  string `json:"stdout"`
	Stderr  string `json:"stderr"`
	Code    int    `json:"code"`
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	parse(fs, args)
	srv := os.Getenv("REX_SERVER")
	if srv == "" {
		srv = fmt.Sprintf("unix:///run/user/%d/rex/server.sock", os.Getuid())
	}
	c, err := rex.Connect(srv)
	if err != nil {
		return fmt.Errorf("serve: connect to Rex at %s: %w", srv, err)
	}
	defer c.Close()
	k := rex.NewCaller(c)
	exe, _ := os.Executable()
	s := &server{rex: k, events: k.Events, done: k.Done(), maxMsg: c.MaxMessage, run: runSelf(exe), exe: exe}
	s.watch = newRepoWatch(s.bump)
	if st, err := os.Stat(exe); err == nil {
		s.exeMtime = st.ModTime()
	}
	first, err := s.claim()
	if err != nil {
		return err
	}
	if !first {
		return nil // another service runs here
	}
	s.session, s.block = os.Getenv("REX_SESSION"), os.Getenv("REX_BLOCK")
	s.detachSelf(s.session, s.block)
	s.sweep()
	err = s.loop()
	if err == errUpgraded {
		s.leave() // ends this process too
		return nil
	}
	return err
}

// detachSelf takes the service's pane out of its window when it runs in
// one (started by the Mac through rex new), so no window shows it.
// It retries for a while: the service can connect before rex new has
// finished placing its pane.
func (s *server) detachSelf(session, block string) {
	if session == "" || block == "" {
		return
	}
	var err error
	for wait := 50 * time.Millisecond; wait < 3*time.Second; wait *= 2 {
		if err = s.rex.DetachBlock(session, block); err == nil {
			return
		}
		time.Sleep(wait)
	}
	fmt.Fprintln(os.Stderr, "rexmod serve: detach from window:", err)
}

// claim makes this the host's service unless one already runs, and says
// which. Two started at once both label themselves; the older client (IDs
// sort by time) keeps the name and the other gives up.
func (s *server) claim() (bool, error) {
	if s.other() != "" {
		return false, nil
	}
	nonce := fmt.Sprintf("%s#starting-%08x", serveLabel, rand.Uint32())
	if err := s.rex.SetLabel(nonce); err != nil {
		return false, fmt.Errorf("serve: set label: %w", err)
	}
	cs, err := s.rex.Clients()
	if err != nil {
		return false, fmt.Errorf("serve: list clients: %w", err)
	}
	for _, c := range cs {
		if c.Label == nonce {
			s.id = c.ID
		}
	}
	if o := s.other(); o != "" && o < s.id {
		return false, nil
	}
	if err := s.rex.DeclareActions(rex.Action{Name: serveAction, Title: "rexmod"}); err != nil {
		return false, fmt.Errorf("serve: declare %s: %w", serveAction, err)
	}
	return true, s.rex.SetLabel(s.label())
}

// other returns the oldest other rexmod service on this server, running or
// starting, or "". Client IDs sort by time, so the oldest is the one that
// started first.
func (s *server) other() string {
	cs, err := s.rex.Clients()
	if err != nil {
		return ""
	}
	best := ""
	for _, c := range cs {
		if c.ID != s.id && !c.Lost && c.Name == "rexmod" && strings.HasPrefix(c.Label, serveLabel+"#") && (best == "" || c.ID < best) {
			best = c.ID
		}
	}
	return best
}

func (s *server) label() string { return fmt.Sprintf("%s#%d", serveLabel, s.seq) }

// bump tells subscribers something changed: a new label is an event.
func (s *server) bump() {
	s.seq++
	_ = s.rex.SetLabel(s.label())
}

func (s *server) loop() error {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	scan := time.NewTicker(repoScanEvery)
	defer scan.Stop()
	for {
		select {
		case <-s.done:
			return nil
		case e := <-s.events:
			if r, ok := rex.ParsePerform(e); ok {
				s.handle(r)
			}
		case <-scan.C:
			s.watch.scan(time.Now())
		case <-tick.C:
			if s.upgraded() {
				return errUpgraded // the next request starts the new binary
			}
		}
	}
}

var errUpgraded = errors.New("rexmod was upgraded")

// sweep destroys sessions left behind by services that ended (a detached
// pane's session outlives it): ours by label, with no block left in them.
func (s *server) sweep() {
	ss, err := s.rex.Sessions()
	if err != nil {
		return
	}
	for _, x := range ss {
		if x.Label != serveSession || x.ID == s.session {
			continue
		}
		if bs, err := s.rex.SessionBlocks(x.ID); err == nil && len(bs) == 0 {
			_ = s.rex.DestroySession(x.ID)
		}
	}
}

// leave destroys the service's own session as it goes, so none piles up,
// if nothing but the service runs in it.
func (s *server) leave() {
	if s.session == "" || s.block == "" {
		return
	}
	bs, err := s.rex.SessionBlocks(s.session)
	if err != nil || len(bs) != 1 || bs[0] != s.block {
		return
	}
	_ = s.rex.DestroySession(s.session)
}

// upgraded says whether our binary was replaced since we started.
func (s *server) upgraded() bool {
	if s.exe == "" || s.exeMtime.IsZero() {
		return false
	}
	st, err := os.Stat(s.exe)
	return err == nil && !st.ModTime().Equal(s.exeMtime)
}

// serveAllowed says whether argv is a command the service runs: those that
// read the repository or act on Rex panes, never hooks or the editor.
func serveAllowed(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	switch argv[0] {
	case "target", "state", "info", "files", "follow":
		return true
	case "comments":
		return len(argv) > 1 && argv[1] == "send"
	}
	return false
}

func (s *server) handle(r rex.PerformRequest) {
	if r.Action != serveAction {
		_ = s.rex.ReplyError(r.CallID, "not_supported", "rexmod serve has no action "+r.Action)
		return
	}
	if p, _ := rex.GetInt(r.Args, "proto"); p != serveProto {
		_ = s.rex.ReplyError(r.CallID, "not_supported", fmt.Sprintf("rexmod serve speaks proto %d, not %d", serveProto, p))
		return
	}
	raw, _ := rex.Get(r.Args, "argv").([]any)
	argv := make([]string, 0, len(raw))
	for _, a := range raw {
		str, ok := rex.Str(a)
		if !ok {
			_ = s.rex.ReplyError(r.CallID, "refused", "argv must be strings")
			return
		}
		argv = append(argv, str)
	}
	if !serveAllowed(argv) {
		_ = s.rex.ReplyError(r.CallID, "refused", fmt.Sprintf("rexmod serve doesn't run %q", strings.Join(argv, " ")))
		return
	}
	stdout, stderr, code, err := s.run(argv)
	if err != nil {
		_ = s.rex.ReplyError(r.CallID, "internal", err.Error())
		return
	}
	s.track(argv, stdout)
	reply := serveReply{Proto: serveProto, Version: rexmodVersion(), Stdout: stdout, Stderr: stderr, Code: code}
	if b, _ := json.Marshal(reply); s.maxMsg > 0 && len(b) > s.maxMsg-serveEnvelope {
		_ = s.rex.ReplyError(r.CallID, "internal", fmt.Sprintf("too_large: %d bytes, over Rex's %d", len(b), s.maxMsg))
		return
	}
	_ = s.rex.Reply(r.CallID, reply)
}

// track watches the repository a request was about: state names it in its
// target, files in --repo.
func (s *server) track(argv []string, stdout string) {
	repo := ""
	switch argv[0] {
	case "state", "target":
		var out struct {
			Repo   string `json:"repo"`
			Target *struct {
				Repo string `json:"repo"`
			} `json:"target"`
		}
		if json.Unmarshal([]byte(stdout), &out) == nil {
			repo = out.Repo
			if out.Target != nil {
				repo = out.Target.Repo
			}
		}
	case "files":
		repo = flagValue(argv[1:], "repo")
	}
	if repo != "" {
		s.watch.track(repo, time.Now())
	}
}

// runSelf runs this binary with argv, as a command of its own, so every
// command is the one rexmod already has. The service's pane variables are
// left out: a command without --session must not take the service's.
func runSelf(exe string) func(argv []string) (string, string, int, error) {
	return func(argv []string) (string, string, int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), serveRunTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, argv...)
		cmd.Env = withoutPaneEnv(os.Environ())
		var out, eb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &eb
		err := cmd.Run()
		if ctx.Err() == context.DeadlineExceeded {
			return "", "", 0, fmt.Errorf("rexmod %s took longer than %s", argv[0], serveRunTimeout)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.String(), eb.String(), ee.ExitCode(), nil
		}
		if err != nil {
			return "", "", 0, err
		}
		return out.String(), eb.String(), 0, nil
	}
}

func withoutPaneEnv(env []string) []string {
	out := env[:0:0]
	for _, e := range env {
		if strings.HasPrefix(e, "REX_SESSION=") || strings.HasPrefix(e, "REX_BLOCK=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// rexmodVersion is the commit rexmod was built from, from the build info.
func rexmodVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "+"
	}
	return rev
}
