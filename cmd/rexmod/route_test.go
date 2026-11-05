package main

import (
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestRoutable(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		args []string
		want bool
	}{
		{"target", nil, true}, {"state", nil, true}, {"info", nil, true}, {"follow", nil, true},
		{"files", nil, true}, {"open", nil, true}, {"pick", nil, true},
		{"pick", []string{"--inner", "--repo", "/r"}, false}, {"pick", []string{"--here"}, false},
		{"comments", []string{"send"}, true},
		{"comments", []string{"add", "--file", "f"}, false}, // the editor adds them where it runs
		{"hook", []string{"prompt"}, false}, {"editor", nil, false}, {"preview", nil, false}, {"warm", nil, false},
	} {
		if got := routable(tc.cmd, tc.args); got != tc.want {
			t.Errorf("routable(%s %v) = %v", tc.cmd, tc.args, got)
		}
	}
}

func TestFlags(t *testing.T) {
	args := []string{"--session", "s1", "--window", "w", "-host=gpu2", "--repo", "/r"}
	if v := flagValue(args, "session"); v != "s1" {
		t.Errorf("session %q", v)
	}
	rest, h := takeFlag(args, "host")
	if h != "gpu2" || !reflect.DeepEqual(rest, []string{"--session", "s1", "--window", "w", "--repo", "/r"}) {
		t.Errorf("takeFlag: %q %v", h, rest)
	}
	rest, h = takeFlag([]string{"--host", "gpu3", "--repo", "/r"}, "host")
	if h != "gpu3" || !reflect.DeepEqual(rest, []string{"--repo", "/r"}) {
		t.Errorf("takeFlag spaced: %q %v", h, rest)
	}
	// A positional "session" isn't the flag.
	if v := flagValue([]string{"session", "x"}, "session"); v != "" {
		t.Errorf("positional taken as flag: %q", v)
	}
}

func TestWithHost(t *testing.T) {
	out, err := withHost([]byte(`{"session_id":"s","repo":"/home/u/r<x>"}`+"\n"), "gpu2", "")
	if err != nil {
		t.Fatal(err)
	}
	var tg Target
	if json.Unmarshal(out, &tg) != nil || tg.Host != "gpu2" || tg.Repo != "/home/u/r<x>" {
		t.Errorf("target: %s", out)
	}
	out, _ = withHost([]byte(`{"target":{"repo":"/r"},"pending_comments":2,"time":"t"}`), "gpu2", "target")
	var st State
	if json.Unmarshal(out, &st) != nil || st.Target == nil || st.Target.Host != "gpu2" || st.Comments != 2 {
		t.Errorf("state: %s", out)
	}
	// state without a target (an error) passes as it came.
	in := []byte(`{"error":"no session","time":"t"}`)
	if out, _ = withHost(in, "gpu2", "target"); string(out) != string(in) {
		t.Errorf("state without target changed: %s", out)
	}
}

func TestRouteRemoteLocalSession(t *testing.T) {
	f := gpuHosts()
	withFakeRex(t, f)
	for _, cmd := range []string{"target", "state", "open"} {
		if handled, err := routeRemote(cmd, []string{"--session", "work"}); handled || err != nil {
			t.Errorf("%s of a local session: handled=%v err=%v", cmd, handled, err)
		}
	}
	if handled, _ := routeRemote("hook", []string{"prompt"}); handled {
		t.Error("hook routed")
	}
}

// open and pick run on the host, with the Mac's Rex theme for the editor.
func TestRouteRemoteEditor(t *testing.T) {
	f := gpuHosts()
	withFakeRex(t, f)
	fakeSSH(t)
	home, _ := os.UserHomeDir()
	os.MkdirAll(home+"/.config/rexmod", 0o755)
	os.WriteFile(home+"/.config/rexmod/config.json", []byte(`{"remote_channel":"ssh"}`), 0o644)
	old := rexThemeArg
	rexThemeArg = func() string { return `{"appearance":"dark"}` }
	t.Cleanup(func() { rexThemeArg = old })
	for _, cmd := range []string{"open", "pick"} {
		r, w, _ := os.Pipe()
		stdout := os.Stdout
		os.Stdout = w
		handled, err := routeRemote(cmd, []string{"--session", "paper", "/r/a.go"})
		w.Close()
		os.Stdout = stdout
		out, _ := io.ReadAll(r)
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		want := remoteCommand("~/.local/bin/rexmod", []string{cmd, "--session", "paper", "/r/a.go", "--rex-theme", `{"appearance":"dark"}`})
		if !handled || err != nil || lines[len(lines)-1] != want {
			t.Errorf("%s: handled=%v err=%v ran %q", cmd, handled, err, lines[len(lines)-1])
		}
	}
}

func TestHasFlag(t *testing.T) {
	if !hasFlag([]string{"--session", "s", "-inner"}, "inner") || !hasFlag([]string{"--here=true"}, "here") {
		t.Error("flag not found")
	}
	if hasFlag([]string{"inner"}, "inner") || hasFlag([]string{"--", "--inner"}, "inner") || hasFlag([]string{"--innerx"}, "inner") {
		t.Error("not a flag, found")
	}
}

// A host that can't be reached is the state's error, as for a local session
// the panel can't follow, so the panel shows why.
func TestRouteRemoteStateUnreachable(t *testing.T) {
	f := gpuHosts()
	withFakeRex(t, f)
	fakeSSH(t)
	t.Setenv("FAKE_SSH_EXIT", "255")
	t.Setenv("FAKE_SSH_STDERR", "Permission denied (publickey,password).")
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	handled, err := routeRemote("state", []string{"--session", "paper"})
	w.Close()
	os.Stdout = stdout
	out, _ := io.ReadAll(r)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	var st State
	if json.Unmarshal(out, &st) != nil || !strings.Contains(st.Error, "can't reach Rex host gpu3") || st.Target != nil {
		t.Errorf("state: %s", out)
	}
	// Other commands still fail outright.
	if _, err := routeRemote("target", []string{"--session", "paper"}); err == nil {
		t.Error("target: want an error")
	}
}

func TestSessionMissing(t *testing.T) {
	stderr := "rexmod: rex api call -s paper session.view: exit status 3: Error: session target \"paper\" not found\n"
	if !sessionMissing("paper", nil, &remoteExit{Code: 1, Stderr: stderr}) {
		t.Error("target's error not recognised")
	}
	st := []byte(`{"target":null,"error":"rex api call -s paper session.view: exit status 3: Error: session target \"paper\" not found","time":"t"}`)
	if !sessionMissing("paper", st, nil) {
		t.Error("state's error not recognised")
	}
	if sessionMissing("paper", []byte(`{"target":{"repo":"/r"}}`), nil) || sessionMissing("other", st, nil) ||
		sessionMissing("paper", nil, &remoteExit{Code: 1, Stderr: "rexmod: boom"}) {
		t.Error("false positive")
	}
}

// A session that moved: the remembered host says it's not there; the route
// looks again and runs on the host that has it, once.
func TestRouteRemoteSessionMoved(t *testing.T) {
	f := gpuHosts()
	withFakeRex(t, f)
	saveSessionHosts(map[string]string{"paper": "gpu2"}) // stale: it's on gpu3
	var ran []string
	old := runHost
	runHost = func(host string, args []string) ([]byte, error) {
		ran = append(ran, host)
		if host == "gpu2" {
			return nil, &remoteExit{Code: 1, Stderr: "rexmod: Error: session target \"paper\" not found\n"}
		}
		return []byte(`{"session_id":"session:c","repo":"/r"}` + "\n"), nil
	}
	t.Cleanup(func() { runHost = old })
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	handled, err := routeRemote("target", []string{"--session", "paper"})
	w.Close()
	os.Stdout = stdout
	out, _ := io.ReadAll(r)
	if !handled || err != nil || !reflect.DeepEqual(ran, []string{"gpu2", "gpu3"}) || !strings.Contains(string(out), `"host":"gpu3"`) {
		t.Errorf("handled=%v err=%v ran=%v out=%s", handled, err, ran, out)
	}
	if m := loadSessionHosts(); m["paper"] != "gpu3" {
		t.Errorf("cache: %v", m)
	}
}
