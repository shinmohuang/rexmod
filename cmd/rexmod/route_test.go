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

func TestRouteRemoteEditorRefused(t *testing.T) {
	f := gpuHosts()
	withFakeRex(t, f)
	fakeSSH(t)
	for _, cmd := range []string{"open", "pick"} {
		handled, err := routeRemote(cmd, []string{"--session", "paper", "/r/a.go"})
		if !handled || err == nil || err.Error() != "the editor isn't supported on Rex hosts yet (gpu3)" {
			t.Errorf("%s: handled=%v err=%v", cmd, handled, err)
		}
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
