package main

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

// fakeRex stands in for the rex CLI: a local server and Rex hosts, each with
// a server ID and sessions ("id=label"). It counts the calls to hosts.
type fakeRex struct {
	mu     sync.Mutex
	local  []string            // sessions on this machine
	hosts  map[string]string   // label -> host_id ("" = unreachable)
	onHost map[string][]string // host_id -> sessions "id=label"
	calls  []string
}

func (f *fakeRex) has(sessions []string, s string) bool {
	for _, x := range sessions {
		id, label, _ := strings.Cut(x, "=")
		if s == id || s == label {
			return true
		}
	}
	return false
}

func (f *fakeRex) call(args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(args, " "))
	f.mu.Unlock()
	host := ""
	if len(args) > 1 && args[0] == "-S" {
		host, args = args[1], args[2:]
		if len(args) > 1 && args[0] == "--timeout" {
			args = args[2:]
		}
	}
	// No rexmod service on any host, and none can be started: the Rex
	// channel gives up and commands go over ssh (see remote_rex_test.go).
	if host != "" {
		switch {
		case len(args) > 2 && args[0] == "api" && args[2] == "client.list":
			return []byte(`{"clients":[]}`), nil
		case args[0] == "new":
			return nil, errors.New("rex new: not here")
		}
	}
	switch {
	case host == "" && args[0] == "api":
		if f.has(f.local, args[3]) {
			return []byte(`{}`), nil
		}
		return nil, errors.New("not found")
	case host == "" && args[0] == "hosts":
		var b strings.Builder
		b.WriteString(`{"hosts":[`)
		i := 0
		for l := range f.hosts {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"host":{"label":"` + l + `"}}`)
			i++
		}
		b.WriteString("]}")
		return []byte(b.String()), nil
	}
	id := f.hosts[host]
	if id == "" {
		return nil, errors.New("unreachable")
	}
	switch args[0] {
	case "ls":
		var b strings.Builder
		b.WriteString(`{"sessions":[`)
		for i, x := range f.onHost[id] {
			sid, label, _ := strings.Cut(x, "=")
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"session_id":"` + sid + `","label":"` + label + `"}`)
		}
		b.WriteString("]}")
		return []byte(b.String()), nil
	case "whoami":
		return []byte(`{"host_id":"` + id + `"}`), nil
	case "api":
		if f.has(f.onHost[id], args[3]) {
			return []byte(`{}`), nil
		}
		return nil, errors.New("not found")
	}
	return nil, errors.New("unexpected " + strings.Join(args, " "))
}

func (f *fakeRex) remoteCalls() int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "-S ") || strings.HasPrefix(c, "hosts ") {
			n++
		}
	}
	return n
}

func withFakeRex(t *testing.T, f *fakeRex) {
	t.Helper()
	old := rexCall
	rexCall = f.call
	t.Cleanup(func() { rexCall = old })
	t.Setenv("HOME", t.TempDir()) // fresh cache and config
	t.Setenv("XDG_CACHE_HOME", "")
}

func gpuHosts() *fakeRex {
	return &fakeRex{
		local:  []string{"session:local=work"},
		hosts:  map[string]string{"gpu2": "H2", "rex-gpu2": "H2", "gpu3": "H3", "away": ""},
		onHost: map[string][]string{"H2": {"session:a=rexmod-test", "session:b=work"}, "H3": {"session:c=paper", "session:d=dup"}},
	}
}

func TestSessionHostLocalFirst(t *testing.T) {
	f := gpuHosts()
	withFakeRex(t, f)
	// "work" is also a session label on gpu2: here wins, and no host is asked.
	for _, s := range []string{"session:local", "work"} {
		if h, err := sessionHost(s); h != "" || err != nil {
			t.Errorf("sessionHost(%q) = %q, %v; want local", s, h, err)
		}
	}
	if n := f.remoteCalls(); n != 0 {
		t.Errorf("local sessions asked Rex hosts %d times", n)
	}
}

func TestSessionHostRemote(t *testing.T) {
	f := gpuHosts()
	withFakeRex(t, f)
	for _, s := range []string{"session:c", "paper"} {
		if h, err := sessionHost(s); h != "gpu3" || err != nil {
			t.Errorf("sessionHost(%q) = %q, %v; want gpu3", s, h, err)
		}
	}
	// Not anywhere: no error here, the local lookup reports it.
	if h, err := sessionHost("session:nowhere"); h != "" || err != nil {
		t.Errorf("unknown session: %q, %v", h, err)
	}
}

func TestSessionHostSameServerTwoLabels(t *testing.T) {
	f := gpuHosts()
	withFakeRex(t, f)
	// gpu2 and rex-gpu2 are one server: no ambiguity, and without a mapping
	// the first label in order.
	if h, err := sessionHost("rexmod-test"); h != "gpu2" || err != nil {
		t.Errorf("got %q, %v; want gpu2", h, err)
	}
	// With an ssh mapping for rex-gpu2 only, that label is the one to use.
	os.Remove(sessionHostsPath())
	home, _ := os.UserHomeDir()
	os.MkdirAll(home+"/.config/rexmod", 0o755)
	os.WriteFile(home+"/.config/rexmod/config.json", []byte(`{"remote_hosts":{"rex-gpu2":{"ssh":"acps-gpu2"}}}`), 0o644)
	if h, err := sessionHost("rexmod-test"); h != "rex-gpu2" || err != nil {
		t.Errorf("got %q, %v; want rex-gpu2", h, err)
	}
}

func TestSessionHostAmbiguous(t *testing.T) {
	f := gpuHosts()
	f.onHost["H2"] = append(f.onHost["H2"], "session:e=dup")
	withFakeRex(t, f)
	_, err := sessionHost("dup")
	if err == nil || !strings.Contains(err.Error(), "gpu2") || !strings.Contains(err.Error(), "gpu3") {
		t.Errorf("want an ambiguity naming both hosts, got %v", err)
	}
}

func TestSessionHostCache(t *testing.T) {
	f := gpuHosts()
	withFakeRex(t, f)
	if h, _ := sessionHost("paper"); h != "gpu3" {
		t.Fatalf("first lookup: %q", h)
	}
	// Hit: trusted as it is, no question to any host.
	f.calls = nil
	if h, _ := sessionHost("paper"); h != "gpu3" {
		t.Errorf("cached lookup: %q", h)
	}
	if n := f.remoteCalls(); n != 0 {
		t.Errorf("cache hit asked Rex hosts %d times: %v", n, f.calls)
	}
	// Stale: the session moved to gpu2. sessionHost still says gpu3 (the
	// command run there finds out); a rescan finds gpu2.
	f.onHost["H3"] = nil
	f.onHost["H2"] = append(f.onHost["H2"], "session:c=paper")
	if h, _ := sessionHost("paper"); h != "gpu3" {
		t.Errorf("stale cache: %q, want gpu3 as remembered", h)
	}
	if h, _ := rescanSessionHost("paper"); h != "gpu2" {
		t.Errorf("after a move: %q, want gpu2", h)
	}
	if m := loadSessionHosts(); m["paper"] != "gpu2" {
		t.Errorf("cache not updated: %v", m)
	}
	// Broken file: as if empty.
	os.WriteFile(sessionHostsPath(), []byte("{not json"), 0o644)
	if h, _ := sessionHost("paper"); h != "gpu2" {
		t.Errorf("broken cache: %q", h)
	}
}
