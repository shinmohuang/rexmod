package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServeHost stands in for rex -S gpu2 with a rexmod service on it (or
// not): client.list, client.perform, new and kill.
type fakeServeHost struct {
	mu        sync.Mutex
	serviceID string // "" = no service running
	startable bool   // rex new brings one up (a rexmod with serve)
	perform   func(argv []string) (string, error)
	calls     []string
	killed    []string
	started   int
	paneEnded bool   // the started pane's program exited (no serve)
	lostID    string // a service that ended, still listed as lost
}

func (f *fakeServeHost) call(args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	if len(args) < 3 || args[0] != "-S" {
		return nil, errors.New("unexpected: " + strings.Join(args, " "))
	}
	rest := args[2:]
	if rest[0] == "--timeout" {
		rest = rest[2:]
	}
	switch {
	case rest[0] == "api" && rest[2] == "client.list":
		cs := `{"client_id":"client:01","info":{"name":"Rex Beta","label":"Mac mini"},"principal":{"kind":"tailscale"}}`
		if f.lostID != "" {
			cs += `,{"client_id":"` + f.lostID + `","info":{"name":"rexmod","label":"rexmod-serve#9"},"principal":{"kind":"peer","uid":3842},"connection_state":"lost"}`
		}
		if f.serviceID != "" {
			cs += `,{"client_id":"` + f.serviceID + `","info":{"name":"rexmod","label":"rexmod-serve#3"},"principal":{"kind":"peer","uid":3842}}`
		}
		return []byte(`{"clients":[` + cs + `]}`), nil
	case rest[0] == "api" && rest[2] == "client.perform":
		var p struct {
			ClientID string `json:"client_id"`
			Args     struct {
				Proto int      `json:"proto"`
				Argv  []string `json:"argv"`
			} `json:"args"`
		}
		json.Unmarshal([]byte(rest[3]), &p)
		if p.ClientID != "" && p.ClientID == f.lostID {
			return nil, fmt.Errorf("rex: exit status 1: Error: \"rexmod-serve#9\" is waiting to reconnect (status=409 code=client_unavailable)")
		}
		if p.ClientID != f.serviceID || f.serviceID == "" {
			return nil, fmt.Errorf("rex: exit status 1: Error: client %s not found (status=404 code=not_found)", p.ClientID)
		}
		out, err := f.perform(p.Args.Argv)
		if err != nil {
			return nil, err
		}
		return []byte(`{"result":` + out + `,"performed_by":"` + f.serviceID + `"}`), nil
	case rest[0] == "new":
		f.started++
		if f.startable {
			f.serviceID = fmt.Sprintf("client:0%d", 4+f.started)
		}
		return []byte(`{"session_id":"session:svc","initial_windows":[{"block_ids":["block:svc"]}]}`), nil
	case rest[0] == "block" && rest[len(rest)-1] == "process":
		if f.paneEnded {
			return []byte(`{"last_exit":{"exit_code":2,"error":"exit status 2"}}`), nil
		}
		return []byte(`{"foreground":{"name":"rexmod"}}`), nil
	case rest[0] == "kill":
		f.killed = append(f.killed, rest[1])
		return nil, nil
	}
	return nil, errors.New("unexpected: " + strings.Join(args, " "))
}

func (f *fakeServeHost) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, prefix) {
			n++
		}
	}
	return n
}

func reply(stdout, stderr string, code int) string {
	b, _ := json.Marshal(serveReply{Proto: serveProto, Version: "test", Stdout: stdout, Stderr: stderr, Code: code})
	return string(b)
}

func withServeHost(t *testing.T, f *fakeServeHost) {
	t.Helper()
	old := rexCall
	rexCall = f.call
	t.Cleanup(func() { rexCall = old })
	t.Setenv("HOME", t.TempDir())
	oldWait := serveStartWait
	serveStartWait = 300 * time.Millisecond
	t.Cleanup(func() { serveStartWait = oldWait })
}

func TestServiceRunning(t *testing.T) {
	f := &fakeServeHost{serviceID: "client:03", perform: func(argv []string) (string, error) {
		return reply(`{"argv":"`+strings.Join(argv, " ")+`"}`, "", 0), nil
	}}
	withServeHost(t, f)
	out, err := runViaService("gpu2", []string{"state", "--session", "s"})
	if err != nil || string(out) != `{"argv":"state --session s"}` {
		t.Fatalf("got %s %v", out, err)
	}
	// The second call uses the cached ID: no client.list.
	runViaService("gpu2", []string{"target"})
	if n := f.count("client.list"); n != 1 {
		t.Errorf("client.list %d times, want 1 (cached after)", n)
	}
	if f.started != 0 {
		t.Error("started a service while one runs")
	}
}

func TestServiceRemoteFailure(t *testing.T) {
	f := &fakeServeHost{serviceID: "client:03", perform: func([]string) (string, error) {
		return reply("", "rexmod: window not found\n", 1), nil
	}}
	withServeHost(t, f)
	_, err := runViaService("gpu2", []string{"state"})
	var re *remoteExit
	if !errors.As(err, &re) || re.Code != 1 || !strings.Contains(re.Stderr, "window not found") {
		t.Fatalf("want the command's failure, got %v", err)
	}
}

func TestServiceStartsWhenMissing(t *testing.T) {
	f := &fakeServeHost{startable: true, perform: func([]string) (string, error) { return reply("ok", "", 0), nil }}
	withServeHost(t, f)
	out, err := runViaService("gpu2", []string{"state"})
	if err != nil || string(out) != "ok" || f.started != 1 {
		t.Fatalf("got %s %v, started %d", out, err, f.started)
	}
	// Started through /bin/sh with $HOME for the default ~/ path.
	for _, c := range f.calls {
		if strings.Contains(c, " new ") && !strings.Contains(c, `exec "$HOME"/'.local/bin/rexmod' serve`) {
			t.Errorf("start command: %s", c)
		}
	}
}

func TestServiceRestartedSinceCached(t *testing.T) {
	f := &fakeServeHost{serviceID: "client:03", perform: func([]string) (string, error) { return reply("ok", "", 0), nil }}
	withServeHost(t, f)
	runViaService("gpu2", []string{"state"})
	f.serviceID = "client:07" // upgraded: a new client
	if out, err := runViaService("gpu2", []string{"state"}); err != nil || string(out) != "ok" {
		t.Fatalf("after a restart: %s %v", out, err)
	}
	if loadServeClients()["gpu2"] != "client:07" {
		t.Errorf("cache: %v", loadServeClients())
	}
}

func TestServiceLostAfterUpgrade(t *testing.T) {
	// The cached service ended (upgraded); Rex lists it as lost. The call
	// fails client_unavailable, the lost one is skipped, a new one starts.
	f := &fakeServeHost{lostID: "client:03", startable: true, perform: func([]string) (string, error) { return reply("new", "", 0), nil }}
	withServeHost(t, f)
	saveServeClients(map[string]string{"gpu2": "client:03"})
	out, err := runViaService("gpu2", []string{"state"})
	if err != nil || string(out) != "new" || f.started != 1 {
		t.Fatalf("got %s %v, started %d", out, err, f.started)
	}
	if id := loadServeClients()["gpu2"]; id == "client:03" {
		t.Errorf("still cached the lost client: %s", id)
	}
}

func TestServiceOldRexmod(t *testing.T) {
	// rexmod there has no serve: its pane ends at once, which is noticed
	// before the wait runs out; the session is killed, and the caller is
	// told to use ssh.
	f := &fakeServeHost{startable: false, paneEnded: true}
	withServeHost(t, f)
	serveStartWait = 5 * time.Second
	t0 := time.Now()
	_, err := runViaService("gpu2", []string{"state"})
	if d := time.Since(t0); d > time.Second {
		t.Errorf("waited %s for a pane that had ended", d)
	}
	var useSSH *errUseSSH
	if !errors.As(err, &useSSH) || !strings.Contains(useSSH.Why, "didn't start") {
		t.Fatalf("want use-ssh, got %v", err)
	}
	if len(f.killed) != 1 || f.killed[0] != "session:svc" {
		t.Errorf("left the session behind: %v", f.killed)
	}
	// The next calls go straight to ssh: no second start, no client.list.
	f.calls = nil
	for i := 0; i < 3; i++ {
		_, err := runViaService("gpu2", []string{"state"})
		if !errors.As(err, &useSSH) || !strings.Contains(useSSH.Why, "isn't available") {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if f.started != 1 || len(f.calls) != 0 {
		t.Errorf("retried starting: started %d, calls %v", f.started, f.calls)
	}
	// After the wait, it tries again (an upgraded rexmod there now).
	m := loadServeClients()
	m["gpu2"] = fmt.Sprintf("!%d", time.Now().Add(-time.Minute).Unix())
	saveServeClients(m)
	f.startable = true
	f.perform = func([]string) (string, error) { return reply("ok", "", 0), nil }
	if out, err := runViaService("gpu2", []string{"state"}); err != nil || string(out) != "ok" {
		t.Errorf("after the wait: %s %v", out, err)
	}
}

func TestServiceFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  string
		why  string
	}{
		{"too large", "rex: exit status 1: Error: too_large: 2000000 bytes, over Rex's 1048576 (status=500 code=internal)", "larger than a Rex message"},
		{"other proto", "rex: exit status 1: Error: rexmod serve speaks proto 2, not 1 (status=400 code=not_supported)", "another protocol"},
		{"rex trouble", "rex: exit status 1: Error: host \"gpu2\" could not be reached", "Rex call failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeServeHost{serviceID: "client:03", perform: func([]string) (string, error) { return "", errors.New(tc.err) }}
			withServeHost(t, f)
			_, err := runViaService("gpu2", []string{"state"})
			var useSSH *errUseSSH
			if !errors.As(err, &useSSH) || !strings.Contains(useSSH.Why, tc.why) {
				t.Errorf("got %v, want use-ssh with %q", err, tc.why)
			}
		})
	}
}

func TestRunOnHostFallsBackToSSH(t *testing.T) {
	f := &fakeServeHost{startable: false}
	withServeHost(t, f)
	fakeSSH(t)
	t.Setenv("HOME", os.Getenv("HOME")) // keep the fake host's empty home
	out, err := runOnHost("gpu2", []string{"state", "--session", "s"})
	if err != nil || !strings.Contains(string(out), "gpu2") {
		t.Fatalf("ssh fallback: %s %v", out, err)
	}
}

func TestRemoteChannelSSHSwitch(t *testing.T) {
	f := &fakeServeHost{serviceID: "client:03", perform: func([]string) (string, error) { return reply("via rex", "", 0), nil }}
	withServeHost(t, f)
	fakeSSH(t)
	home, _ := os.UserHomeDir()
	os.MkdirAll(home+"/.config/rexmod", 0o755)
	os.WriteFile(home+"/.config/rexmod/config.json", []byte(`{"remote_channel":"ssh"}`), 0o644)
	out, err := runOnHost("gpu2", []string{"state"})
	if err != nil || strings.Contains(string(out), "via rex") || len(f.calls) != 0 {
		t.Fatalf("remote_channel ssh still used Rex: %s %v %v", out, err, f.calls)
	}
}

func TestLockServeStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	unlock, ok := lockServeStart("gpu2")
	if !ok {
		t.Fatal("first lock")
	}
	if _, ok := lockServeStart("gpu2"); ok {
		t.Error("second lock while held")
	}
	unlock()
	if _, ok := lockServeStart("gpu2"); !ok {
		t.Error("lock after unlock")
	}
}

// From the command line in: a session on gpu3 found through rex hosts, its
// state through the service, host added, no ssh.
func TestRouteRemoteViaService(t *testing.T) {
	hosts := gpuHosts()
	withFakeRex(t, hosts)
	svc := &fakeServeHost{serviceID: "client:03", perform: func(argv []string) (string, error) {
		return reply(`{"target":{"repo":"/home/u/paper"},"time":"t"}`, "", 0), nil
	}}
	rexCall = func(args ...string) ([]byte, error) {
		j := strings.Join(args, " ")
		if strings.Contains(j, "client.list") || strings.Contains(j, "client.perform") {
			return svc.call(args...)
		}
		return hosts.call(args...)
	}
	sshBin = "/nonexistent/ssh" // any ssh use fails the test
	t.Cleanup(func() { sshBin = "ssh" })

	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	handled, err := routeRemote("state", []string{"--session", "paper"})
	w.Close()
	os.Stdout = stdout
	out := make([]byte, 4096)
	n, _ := r.Read(out)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	var st State
	if json.Unmarshal(out[:n], &st) != nil || st.Target == nil || st.Target.Host != "gpu3" || st.Target.Repo != "/home/u/paper" {
		t.Errorf("state: %s", out[:n])
	}
}
