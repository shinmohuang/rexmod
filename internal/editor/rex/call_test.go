package rex

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// callConn2 is an in-memory control connection: requests go to reqs, and
// whatever the test puts in inbox comes back from Read/Next.
type callConn2 struct {
	mu      sync.Mutex
	next    uint64
	reqs    chan callReq
	inbox   chan *ClientMessage
	pending []*ClientMessage
}

type callReq struct {
	id      uint64
	method  string
	payload any
}

func newCallConn() *callConn2 {
	return &callConn2{reqs: make(chan callReq, 16), inbox: make(chan *ClientMessage, 16)}
}

func (f *callConn2) Request(method string, payload any) (uint64, error) {
	f.mu.Lock()
	f.next++
	id := f.next
	f.mu.Unlock()
	// Round-trip through JSON, as the wire would.
	b, _ := json.Marshal(payload)
	v, _ := decode(b)
	f.reqs <- callReq{id, method, v}
	return id, nil
}

func (f *callConn2) Read() (bool, error) {
	m, ok := <-f.inbox
	if !ok {
		return false, nil
	}
	f.mu.Lock()
	f.pending = append(f.pending, m)
	f.mu.Unlock()
	return true, nil
}

func (f *callConn2) Next() (*ClientMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return nil, nil
	}
	m := f.pending[0]
	f.pending = f.pending[1:]
	return m, nil
}

func (f *callConn2) respond(id uint64, ok any, err *Error) {
	f.inbox <- &ClientMessage{Kind: KindResponse, Response: &Response{ID: id, Result: Result{Ok: ok, Err: err}}}
}

// serve answers every request with reply(method, payload).
func (f *callConn2) serve(reply func(r callReq) (any, *Error)) {
	go func() {
		for r := range f.reqs {
			ok, err := reply(r)
			f.respond(r.id, ok, err)
		}
	}()
}

func TestCallerCall(t *testing.T) {
	f := newCallConn()
	var got []callReq
	var mu sync.Mutex
	f.serve(func(r callReq) (any, *Error) {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
		if r.method == "client.reply" && Get(r.payload, "error") != nil {
			return nil, &Error{Code: "bad_request", Message: "nope"}
		}
		return map[string]any{"ok": r.method}, nil
	})
	k := NewCaller(f)
	v, err := k.Call("server.version", nil, time.Second)
	if s, _ := GetStr(v, "ok"); err != nil || s != "server.version" {
		t.Fatalf("call: %v %v", v, err)
	}
	if err := k.SetLabel("rexmod-serve#1"); err != nil {
		t.Fatal(err)
	}
	if err := k.DeclareActions(Action{Name: "rexmod.run", Title: "rexmod"}); err != nil {
		t.Fatal(err)
	}
	if err := k.Reply("call:1", map[string]any{"code": 0}); err != nil {
		t.Fatal(err)
	}
	var re *Error
	if err := k.ReplyError("call:2", "refused", "no"); !errors.As(err, &re) || re.Code != "bad_request" {
		t.Fatalf("server error not passed on: %v", err)
	}
	if err := k.DetachBlock("session:s", "block:b"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string]func(any) bool{
		"client.set_label": func(p any) bool { s, _ := GetStr(p, "label"); return s == "rexmod-serve#1" },
		"client.keymap": func(p any) bool {
			a, _ := Get(p, "actions").([]any)
			n, _ := GetStr(a[0], "name")
			return n == "rexmod.run"
		},
		"session.detach_block": func(p any) bool { s, _ := GetStr(p, "block_id"); return s == "block:b" },
	}
	// detach_block goes between an attach and a detach of its session.
	var order []string
	for _, r := range got {
		if strings.HasPrefix(r.method, "session.") {
			order = append(order, r.method)
		}
	}
	if strings.Join(order, ",") != "session.attach,session.detach_block,session.detach" {
		t.Errorf("session calls: %v", order)
	}
	for _, r := range got {
		if check, ok := want[r.method]; ok && !check(r.payload) {
			t.Errorf("%s payload: %v", r.method, r.payload)
		}
		if r.method == "client.reply" && Get(r.payload, "error") == nil {
			if id, _ := GetStr(r.payload, "call_id"); id != "call:1" || Get(r.payload, "result") == nil {
				t.Errorf("reply payload: %v", r.payload)
			}
		}
	}
}

func TestCallerTimeoutAndClose(t *testing.T) {
	f := newCallConn()
	k := NewCaller(f)
	go func() { <-f.reqs }() // swallow, never answer
	if _, err := k.Call("x", nil, 50*time.Millisecond); err != ErrCallTimeout {
		t.Fatalf("want timeout, got %v", err)
	}
	// A call waiting when the connection ends gets ErrCallerDone.
	errc := make(chan error, 1)
	go func() { _, err := k.Call("y", nil, 5*time.Second); errc <- err }()
	<-f.reqs
	close(f.inbox)
	select {
	case err := <-errc:
		if err != ErrCallerDone {
			t.Fatalf("want ErrCallerDone, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting call not released")
	}
	<-k.Done()
	if _, err := k.Call("z", nil, time.Second); err != ErrCallerDone {
		t.Fatalf("after close: %v", err)
	}
}

func TestCallerEventsAndPerform(t *testing.T) {
	f := newCallConn()
	k := NewCaller(f)
	m, _ := ParseMessage(sample(t, `{"type":"event","seq":4,"payload":{"type":"client_perform_requested","payload":{"call_id":"call:9","action":"rexmod.run","args":{"proto":1,"argv":["state"]}}}}`))
	f.inbox <- m
	select {
	case e := <-k.Events:
		r, ok := ParsePerform(e)
		if !ok || r.CallID != "call:9" || r.Action != "rexmod.run" {
			t.Fatalf("perform: %+v %v", r, ok)
		}
		if p, _ := GetInt(r.Args, "proto"); p != 1 {
			t.Errorf("args: %v", r.Args)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	if _, ok := ParsePerform(&Event{Name: "client_changed"}); ok {
		t.Error("other events aren't perform requests")
	}
}

func TestParseClients(t *testing.T) {
	cs := ParseClients(sample(t, `{"clients":[
		{"client_id":"client:a","info":{"kind":"other","name":"rexmod","label":"rexmod-serve#3"},"principal":{"kind":"peer","uid":3842}},
		{"client_id":"client:b","info":{"name":"Rex Beta","label":"Mac mini"},"principal":{"kind":"tailscale","login_name":"x@y"},"connection_state":"lost"}]}`))
	if len(cs) != 2 || cs[0].Label != "rexmod-serve#3" || cs[0].Name != "rexmod" || cs[0].Kind != "peer" || cs[0].UID != 3842 {
		t.Fatalf("%+v", cs)
	}
	if cs[0].Lost || !cs[1].Lost {
		t.Errorf("connection state: %+v", cs)
	}
	if cs[1].Kind != "tailscale" || cs[1].UID != -1 {
		t.Errorf("%+v", cs[1])
	}
}
