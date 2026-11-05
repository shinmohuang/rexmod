package rex

import (
	"errors"
	"sync"
	"time"
)

// Caller makes request/response calls over a control connection: one
// goroutine reads, hands each response to the call waiting for its ID and
// queues events. It's what a long-running client such as rexmod serve
// needs; the editor's Session is a state machine of its own.

// callConn is the part of Client a Caller uses; tests stand in for it.
type callConn interface {
	Request(method string, payload any) (uint64, error)
	Read() (bool, error)
	Next() (*ClientMessage, error)
}

var (
	ErrCallTimeout = errors.New("rex: call timed out")
	ErrCallerDone  = errors.New("rex: connection closed")
)

type Caller struct {
	c      callConn
	mu     sync.Mutex
	waits  map[uint64]chan Result
	done   chan struct{}
	err    error
	Events chan *Event // server and session events, in order; full = dropped
}

// NewCaller starts reading c. Events gets every event until the connection
// ends, when Done closes.
func NewCaller(c callConn) *Caller {
	k := &Caller{c: c, waits: map[uint64]chan Result{}, done: make(chan struct{}), Events: make(chan *Event, 256)}
	go k.loop()
	return k
}

// Done is closed when the connection ends; Err then says why (nil: closed).
func (k *Caller) Done() <-chan struct{} { return k.done }

func (k *Caller) Err() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.err
}

func (k *Caller) loop() {
	var err error
	defer func() {
		k.mu.Lock()
		k.err = err
		for id, w := range k.waits {
			close(w)
			delete(k.waits, id)
		}
		k.mu.Unlock()
		close(k.done)
	}()
	for {
		for {
			m, e := k.c.Next()
			if e != nil {
				err = e
				return
			}
			if m == nil {
				break
			}
			switch m.Kind {
			case KindResponse:
				k.mu.Lock()
				if w, ok := k.waits[m.Response.ID]; ok {
					w <- m.Response.Result
					delete(k.waits, m.Response.ID)
				}
				k.mu.Unlock()
			case KindEvent:
				select {
				case k.Events <- m.Event:
				default: // a reader that fell behind loses events, not the connection
				}
			case KindClosed:
				return
			}
		}
		ok, e := k.c.Read()
		if e != nil || !ok {
			err = e
			return
		}
	}
}

// Call sends method and waits up to timeout for its response: the result,
// or the server's *Error.
func (k *Caller) Call(method string, payload any, timeout time.Duration) (any, error) {
	w := make(chan Result, 1)
	// Hold the lock across the write so the reader can't see the response
	// before the wait is registered.
	k.mu.Lock()
	select {
	case <-k.done:
		k.mu.Unlock()
		return nil, ErrCallerDone
	default:
	}
	id, err := k.c.Request(method, payload)
	if err != nil {
		k.mu.Unlock()
		return nil, err
	}
	k.waits[id] = w
	k.mu.Unlock()

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r, ok := <-w:
		if !ok {
			return nil, ErrCallerDone
		}
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Ok, nil
	case <-t.C:
		k.mu.Lock()
		delete(k.waits, id)
		k.mu.Unlock()
		return nil, ErrCallTimeout
	}
}

const callTimeout = 5 * time.Second

// Action is one action a client declares for client.perform.
type Action struct {
	Name  string `json:"name"`
	Title string `json:"title,omitempty"`
}

// DeclareActions lists this client's actions (client.keymap), so others can
// client.perform them.
func (k *Caller) DeclareActions(actions ...Action) error {
	_, err := k.Call("client.keymap", map[string]any{"actions": actions}, callTimeout)
	return err
}

// SetLabel sets this client's label; every change is a client_changed event.
func (k *Caller) SetLabel(label string) error {
	_, err := k.Call("client.set_label", map[string]any{"label": label}, callTimeout)
	return err
}

// Reply answers a client_perform_requested event with result.
func (k *Caller) Reply(callID string, result any) error {
	_, err := k.Call("client.reply", map[string]any{"call_id": callID, "result": result}, callTimeout)
	return err
}

// ReplyError answers a client_perform_requested event with a failure: code
// is not_supported, refused or internal.
func (k *Caller) ReplyError(callID, code, message string) error {
	_, err := k.Call("client.reply", map[string]any{"call_id": callID, "error": map[string]any{"code": code, "message": message}}, callTimeout)
	return err
}

// DetachBlock takes a block out of its window; it keeps running. Rex wants
// the connection attached to the session for that, so it attaches for the
// call and detaches after (no events of that session keep coming).
func (k *Caller) DetachBlock(session, block string) error {
	sid := map[string]any{"session_id": session}
	if _, err := k.Call("session.attach", sid, callTimeout); err != nil {
		return err
	}
	_, err := k.Call("session.detach_block", map[string]any{"session_id": session, "block_id": block}, callTimeout)
	_, _ = k.Call("session.detach", sid, callTimeout)
	return err
}

// SessionInfo is a session as session.list gives it.
type SessionInfo struct {
	ID    string
	Label string
}

// Sessions lists the server's sessions.
func (k *Caller) Sessions() ([]SessionInfo, error) {
	v, err := k.Call("session.list", map[string]any{}, callTimeout)
	if err != nil {
		return nil, err
	}
	list, _ := Get(v, "sessions").([]any)
	out := make([]SessionInfo, 0, len(list))
	for _, x := range list {
		var si SessionInfo
		si.ID, _ = GetStr(x, "session_id")
		si.Label, _ = GetStr(x, "label")
		out = append(out, si)
	}
	return out, nil
}

// SessionBlocks lists the IDs of every block in a session, detached ones
// included.
func (k *Caller) SessionBlocks(session string) ([]string, error) {
	sid := map[string]any{"session_id": session}
	if _, err := k.Call("session.attach", sid, callTimeout); err != nil {
		return nil, err
	}
	defer k.Call("session.detach", sid, callTimeout)
	v, err := k.Call("session.list_blocks", sid, callTimeout)
	if err != nil {
		return nil, err
	}
	list, _ := Get(v, "blocks").([]any)
	out := make([]string, 0, len(list))
	for _, b := range list {
		if id, ok := GetStr(b, "block_id"); ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// DestroySession closes a session and every block in it.
func (k *Caller) DestroySession(session string) error {
	sid := map[string]any{"session_id": session}
	if _, err := k.Call("session.attach", sid, callTimeout); err != nil {
		return err
	}
	_, err := k.Call("session.destroy", sid, callTimeout)
	return err
}

// ClientInfo is what rexmod needs of a connected client.
type ClientInfo struct {
	ID    string
	Label string
	Name  string
	Kind  string // principal kind: peer, tailscale, …
	UID   int64  // for peer, else -1
	// Lost: the client's connection dropped and Rex waits for it to come
	// back (a process that ended shows like this for a while).
	Lost bool
}

// Clients lists the connected clients (client.list).
func (k *Caller) Clients() ([]ClientInfo, error) {
	v, err := k.Call("client.list", map[string]any{}, callTimeout)
	if err != nil {
		return nil, err
	}
	return ParseClients(v), nil
}

// ParseClients reads a client.list result.
func ParseClients(v any) []ClientInfo {
	list, _ := Get(v, "clients").([]any)
	out := make([]ClientInfo, 0, len(list))
	for _, c := range list {
		ci := ClientInfo{UID: -1}
		ci.ID, _ = GetStr(c, "client_id")
		info := Get(c, "info")
		ci.Label, _ = GetStr(info, "label")
		ci.Name, _ = GetStr(info, "name")
		p := Get(c, "principal")
		ci.Kind, _ = GetStr(p, "kind")
		if uid, ok := GetInt(p, "uid"); ok {
			ci.UID = uid
		}
		st, _ := GetStr(c, "connection_state")
		ci.Lost = st == "lost"
		out = append(out, ci)
	}
	return out
}

// PerformRequest is a client_perform_requested event: another client asks
// this one to perform action.
type PerformRequest struct {
	CallID string
	Action string
	Args   any
}

// ParsePerform reads a client_perform_requested event, or returns false.
func ParsePerform(e *Event) (PerformRequest, bool) {
	if e == nil || e.Name != "client_perform_requested" {
		return PerformRequest{}, false
	}
	var r PerformRequest
	r.CallID, _ = GetStr(e.Payload, "call_id")
	r.Action, _ = GetStr(e.Payload, "action")
	r.Args = Get(e.Payload, "args")
	return r, r.CallID != ""
}
