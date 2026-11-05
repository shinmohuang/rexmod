package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// routeRemote runs a command on the Rex host its session lives on. It
// returns handled=false for a session of this machine, which then runs as it
// always has. --host names the host outright (for commands given a repo,
// not a session) and isn't passed on.
func routeRemote(cmd string, args []string) (handled bool, err error) {
	if !routable(cmd, args) {
		return false, nil
	}
	args, host := takeFlag(args, "host")
	session := ""
	if host == "" {
		session = flagValue(args, "session")
		if session == "" {
			session = os.Getenv("REX_SESSION")
		}
		if host, err = sessionHost(session); err != nil {
			return true, err
		}
		if host == "" {
			return false, nil
		}
	}
	if cmd == "open" || cmd == "pick" {
		// The editor runs on the host, its colors from the Mac's Rex.
		if theme := rexThemeArg(); theme != "" {
			args = append(args, "--rex-theme", theme)
		}
	}
	out, err := runHost(host, append([]string{cmd}, args...))
	if session != "" && sessionMissing(session, out, err) {
		// The remembered host no longer has it (sessionHost doesn't ask):
		// look again, and run there, or here, once.
		if host, err = rescanSessionHost(session); err != nil {
			return true, err
		}
		if host == "" {
			return false, nil
		}
		out, err = runHost(host, append([]string{cmd}, args...))
	}
	var re *remoteExit
	if err != nil && cmd == "state" && !errors.As(err, &re) {
		// As for a local session the panel can't follow: the reason goes in
		// the state, so the panel shows it instead of keeping stale changes.
		return true, printJSON(State{Error: err.Error(), Time: time.Now().Format(time.RFC3339)})
	}
	if err != nil {
		return true, err
	}
	switch cmd {
	case "target":
		out, err = withHost(out, host, "")
	case "state":
		out, err = withHost(out, host, "target")
	}
	if err != nil {
		return true, err
	}
	_, err = os.Stdout.Write(out)
	return true, err
}

// runHost runs a command on a Rex host; tests stand in for it.
var runHost = runOnHost

// sessionMissing says whether a remote rexmod failed because its Rex server
// has no such session: in its stderr, or in the state's error.
func sessionMissing(session string, out []byte, err error) bool {
	msg := fmt.Sprintf("session target %q not found", session)
	var re *remoteExit
	if errors.As(err, &re) && strings.Contains(re.Stderr, msg) {
		return true
	}
	if err != nil {
		return false
	}
	var st struct {
		Target *json.RawMessage `json:"target"`
		Error  string           `json:"error"`
	}
	return json.Unmarshal(out, &st) == nil && st.Target == nil && strings.Contains(st.Error, msg)
}

// routable says whether a command acts on a session's repository or panes
// and so must run where the session is. Hooks, the editor's own commands
// and comments the editor adds run where they're started: already there.
func routable(cmd string, args []string) bool {
	switch cmd {
	case "target", "state", "info", "follow", "files", "open":
		return true
	case "pick":
		// --inner and --here run the picker in the pane they're started in
		return !hasFlag(args, "inner") && !hasFlag(args, "here")
	case "comments":
		return len(args) > 0 && args[0] == "send"
	}
	return false
}

// hasFlag says whether args hold the boolean flag --name or -name.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		k := strings.TrimLeft(a, "-")
		if strings.HasPrefix(a, "-") && (k == name || strings.HasPrefix(k, name+"=")) {
			return true
		}
	}
	return false
}

// flagValue returns the value of --name or -name in args ("--name v" or
// "--name=v"), the last one winning as with package flag.
func flagValue(args []string, name string) string {
	v := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		k := strings.TrimLeft(a, "-")
		if k == name && i+1 < len(args) {
			v = args[i+1]
			i++
		} else if val, ok := strings.CutPrefix(k, name+"="); ok {
			v = val
		}
	}
	return v
}

// takeFlag removes --name (and its value) from args and returns its value.
func takeFlag(args []string, name string) ([]string, string) {
	v := ""
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		k := strings.TrimLeft(a, "-")
		switch {
		case strings.HasPrefix(a, "-") && k == name && i+1 < len(args):
			v = args[i+1]
			i++
		case strings.HasPrefix(a, "-") && strings.HasPrefix(k, name+"="):
			v = strings.TrimPrefix(k, name+"=")
		default:
			out = append(out, a)
		}
	}
	return out, v
}

// withHost adds "host" to a JSON object from the remote rexmod, or to its
// field obj ("target" of state). Output without that object passes as is.
func withHost(out []byte, host, obj string) ([]byte, error) {
	var top map[string]json.RawMessage
	if json.Unmarshal(out, &top) != nil {
		return out, nil
	}
	if obj == "" {
		top["host"], _ = json.Marshal(host)
	} else {
		var inner map[string]json.RawMessage
		if raw, ok := top[obj]; !ok || json.Unmarshal(raw, &inner) != nil || inner == nil {
			return out, nil
		}
		inner["host"], _ = json.Marshal(host)
		b, err := marshalNoEscape(inner)
		if err != nil {
			return nil, err
		}
		top[obj] = b
	}
	return marshalNoEscape(top)
}

func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// exitRemote ends rexmod as the remote one ended, its stderr verbatim, and
// reports whether err was such a failure.
func exitRemote(err error) bool {
	var re *remoteExit
	if !errors.As(err, &re) {
		return false
	}
	os.Stderr.WriteString(re.Stderr)
	os.Exit(re.Code)
	return true
}
