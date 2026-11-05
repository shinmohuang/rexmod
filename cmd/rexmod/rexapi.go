package main

// Rex through its API: when REX_SERVER is this machine's unix socket, the rex
// commands rexmod runs most (session.view, a block's process, the session
// list, focus, close) go over one connection the process keeps, instead of a
// rex process each. On a busy host a rex process costs ~60 ms, and finding a
// window's agent asks every pane of it. Anything else, or any call the API
// answers with an error, runs the rex CLI as before, so failures read the
// same.

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/alpcanaydin/rexmod/internal/editor/rex"
)

// rexAPITimeout matches the CLI's default --timeout.
const rexAPITimeout = 10 * time.Second

// rexAPIConn is the connection, made on first use.
type rexAPIConn struct {
	c        rexCaller
	attached map[string]bool
	sessions []rex.SessionInfo // session.list, once per process
	listed   bool
}

var (
	rexAPIOnce sync.Once
	rexAPIMu   sync.Mutex
	rexAPIc    *rexAPIConn
	// rexAPIDial connects to the server; tests stand in a fake.
	rexAPIDial = func(url string) (rexCaller, error) {
		c, err := rex.Connect(url)
		if err != nil {
			return nil, err
		}
		return rex.NewCaller(c), nil
	}
)

// rexCaller is what rexmod uses of a rex.Caller.
type rexCaller interface {
	Call(method string, payload any, timeout time.Duration) (any, error)
}

func rexAPIConnection() *rexAPIConn {
	rexAPIOnce.Do(func() {
		url := os.Getenv("REX_SERVER")
		if !strings.HasPrefix(url, "unix://") {
			return
		}
		c, err := rexAPIDial(url)
		if err != nil {
			return
		}
		rexAPIc = &rexAPIConn{c: c, attached: map[string]bool{}}
	})
	return rexAPIc
}

// resetRexAPI forgets the connection, for tests.
func resetRexAPI() {
	rexAPIOnce = sync.Once{}
	rexAPIc = nil
}

func (a *rexAPIConn) do(method string, payload any) (any, error) {
	return a.c.Call(method, payload, rexAPITimeout)
}

// rexViaAPI runs args over the API when it knows them; ok=false means run
// the CLI instead (unknown command, another server, or an error to report
// in the CLI's words).
func rexViaAPI(args []string) (out []byte, ok bool) {
	if len(args) == 0 || args[0] == "-S" {
		return nil, false
	}
	a := rexAPIConnection()
	if a == nil {
		return nil, false
	}
	rexAPIMu.Lock()
	defer rexAPIMu.Unlock()
	var v any
	var err error
	switch {
	case len(args) == 5 && args[0] == "api" && args[1] == "call" && args[2] == "-s":
		// rex api call -s S METHOD
		sid, found := a.session(args[3])
		if !found {
			return nil, false
		}
		if err = a.attach(sid); err == nil {
			v, err = a.do(args[4], map[string]any{"session_id": sid})
		}
	case len(args) == 7 && args[0] == "block" && args[1] == "call" && args[2] == "-s" && args[4] == "-b" && isBlockID(args[5]):
		// rex block call -s S -b B METHOD
		sid, found := a.session(args[3])
		if !found {
			return nil, false
		}
		if err = a.attach(sid); err == nil {
			v, err = a.do("block.method", map[string]any{"session_id": sid, "block_id": args[5], "method": args[6]})
		}
	case len(args) == 2 && args[0] == "ls" && args[1] == "--json":
		v, err = a.do("session.list", map[string]any{})
	case len(args) == 4 && args[0] == "focus" && args[1] == "-s" && isBlockID(args[3]):
		sid, found := a.session(args[2])
		if !found {
			return nil, false
		}
		if err = a.attach(sid); err == nil {
			_, err = a.do("session.focus_block", map[string]any{"session_id": sid, "block_id": args[3]})
		}
		if err == nil {
			return nil, true // the CLI prints nothing rexmod reads
		}
	case len(args) == 5 && args[0] == "block" && args[1] == "close" && args[2] == "-s" && isBlockID(args[4]):
		sid, found := a.session(args[3])
		if !found {
			return nil, false
		}
		if err = a.attach(sid); err == nil {
			_, err = a.do("block.close", map[string]any{"session_id": sid, "block_id": args[4]})
		}
		if err == nil {
			return nil, true
		}
	default:
		return nil, false
	}
	if err != nil {
		return nil, false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return append(b, '\n'), true
}

func isBlockID(s string) bool { return strings.HasPrefix(s, "block:") }

// session resolves a session ID or label the way the CLI's -s takes them:
// a full ID, or a label only one session has. Anything else (an ID suffix,
// an ambiguous label) is left to the CLI.
func (a *rexAPIConn) session(s string) (string, bool) {
	if strings.HasPrefix(s, "session:") {
		return s, true
	}
	if !a.listed {
		v, err := a.do("session.list", map[string]any{})
		if err != nil {
			return "", false
		}
		a.sessions = parseSessionList(v)
		a.listed = true
	}
	id := ""
	for _, si := range a.sessions {
		if si.Label == s {
			if id != "" {
				return "", false
			}
			id = si.ID
		}
	}
	return id, id != ""
}

func parseSessionList(v any) []rex.SessionInfo {
	list, _ := rex.Get(v, "sessions").([]any)
	out := make([]rex.SessionInfo, 0, len(list))
	for _, x := range list {
		var si rex.SessionInfo
		si.ID, _ = rex.GetStr(x, "session_id")
		si.Label, _ = rex.GetStr(x, "label")
		out = append(out, si)
	}
	return out
}

// attach attaches the connection to a session once: Rex wants that for
// methods that take a session_id.
func (a *rexAPIConn) attach(sid string) error {
	if a.attached[sid] {
		return nil
	}
	if _, err := a.do("session.attach", map[string]any{"session_id": sid}); err != nil {
		return err
	}
	a.attached[sid] = true
	return nil
}
