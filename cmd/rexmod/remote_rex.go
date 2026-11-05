package main

// The Rex channel to a remote host: its rexmod service (rexmod serve) runs
// a command for us through client.perform, over the connection rex -S
// already has (Tailscale), so no ssh and no shell start per call. The
// service is found by its client label, started through rex new when
// missing, and anything that goes wrong sends the caller to ssh instead.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/alpcanaydin/rexmod/internal/editor/rex"
)

// errUseSSH means the Rex channel can't serve this call; Why says why.
type errUseSSH struct{ Why string }

func (e *errUseSSH) Error() string { return "use ssh: " + e.Why }

// serveStartWait bounds waiting for a service we started; a variable for tests.
var serveStartWait = 3 * time.Second

// serveRetryAfter is how long a host where the service didn't start goes
// straight to ssh.
const serveRetryAfter = 10 * time.Minute

// unavailableUntil reads a cache entry "!<unix time>": no service until then.
func unavailableUntil(entry string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(entry, "!")
	if !ok {
		return time.Time{}, false
	}
	var sec int64
	if _, err := fmt.Sscanf(rest, "%d", &sec); err != nil {
		return time.Time{}, true // unreadable: as if expired
	}
	return time.Unix(sec, 0), true
}

// runViaService runs rexmod args on host through its service. Like
// runRemote it returns stdout, or a *remoteExit for a command that failed
// there; an *errUseSSH means try ssh.
func runViaService(host string, args []string) ([]byte, error) {
	if interactive(args) {
		// not behind the panel's refreshes on the main client
		if ui := uiServiceClient(host); ui != "" {
			out, err := performRun(host, ui, args)
			var gone *errServiceGone
			if !errors.As(err, &gone) {
				return out, err
			}
			forgetServiceClient(host) // and on to the main client, looked up afresh
		}
	}
	id, err := serviceClient(host)
	if err != nil {
		return nil, err
	}
	out, err := performRun(host, id, args)
	var gone *errServiceGone
	if errors.As(err, &gone) {
		// It went away since we looked (restarted, upgraded): look again.
		forgetServiceClient(host)
		if id, err = serviceClient(host); err != nil {
			return nil, err
		}
		out, err = performRun(host, id, args)
	}
	return out, err
}

// errServiceGone: the client ID we had names no client now.
type errServiceGone struct{}

func (errServiceGone) Error() string { return "rexmod service gone" }

// rexErrCode is the code at the end of a rex CLI error, as in
// "… (status=404 code=not_found)".
var rexErrCode = regexp.MustCompile(`\(status=\d+ code=([a-z_]+)\)`)

func performRun(host, clientID string, args []string) ([]byte, error) {
	payload, _ := json.Marshal(map[string]any{
		"client_id":  clientID,
		"action":     serveAction,
		"args":       map[string]any{"proto": serveProto, "argv": args},
		"timeout_ms": 25000,
	})
	out, err := rexCall("-S", host, "--timeout", "30s", "api", "call", "client.perform", string(payload))
	if err != nil {
		msg := err.Error()
		code := ""
		if m := rexErrCode.FindStringSubmatch(msg); m != nil {
			code = m[1]
		}
		switch {
		case code == "not_found" || code == "client_unavailable":
			// Gone, or ended and lingering as a lost client: look again.
			return nil, &errServiceGone{}
		case strings.Contains(msg, "too_large:"):
			return nil, &errUseSSH{"the result is larger than a Rex message"}
		case code == "not_supported":
			return nil, &errUseSSH{"the service on " + host + " speaks another protocol"}
		}
		return nil, &errUseSSH{"Rex call failed: " + lastLine(msg)}
	}
	var resp struct {
		Result serveReply `json:"result"`
	}
	if json.Unmarshal(out, &resp) != nil || resp.Result.Proto != serveProto {
		return nil, &errUseSSH{"unexpected reply from the service on " + host}
	}
	r := resp.Result
	if r.Stderr != "" && r.Code == 0 {
		os.Stderr.WriteString(r.Stderr)
	}
	if r.Code != 0 {
		return []byte(r.Stdout), &remoteExit{Code: r.Code, Stderr: r.Stderr}
	}
	return []byte(r.Stdout), nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// serviceClient returns the client ID of host's rexmod service, starting it
// if none runs.
func serviceClient(host string) (string, error) {
	if id := loadServeClients()[host]; id != "" {
		if until, ok := unavailableUntil(id); ok {
			if time.Now().Before(until) {
				return "", &errUseSSH{fmt.Sprintf("rexmod serve isn't available on %s (trying again after %s)", host, until.Format("15:04"))}
			}
		} else {
			return id, nil
		}
	}
	id, ui, err := findServices(host)
	if err != nil {
		return "", err
	}
	if id != "" {
		rememberServices(host, id, ui)
		return id, nil
	}
	if id, err = startService(host); err != nil {
		var useSSH *errUseSSH
		if errors.As(err, &useSSH) && strings.Contains(useSSH.Why, "didn't start") {
			// Most likely an older rexmod there: don't try on every call.
			m := loadServeClients()
			m[host] = fmt.Sprintf("!%d", time.Now().Add(serveRetryAfter).Unix())
			saveServeClients(m)
		}
		return "", err
	}
	m := loadServeClients()
	m[host] = id
	delete(m, uiKey(host)) // started just now: its interactive client may not be up yet
	saveServeClients(m)
	return id, nil
}

// findService looks for a running service among host's clients, or "".
func findService(host string) (string, error) {
	main, _, err := findServices(host)
	return main, err
}

// findServices finds host's service and its interactive client ("" when
// that service has none: an older rexmod). The interactive client is the
// first one connected after the main one (IDs sort by time); one left over
// from an earlier service would be older.
func findServices(host string) (main, ui string, err error) {
	out, err := rexCall("-S", host, "--timeout", hostTimeout, "api", "call", "client.list")
	if err != nil {
		return "", "", &errUseSSH{"can't list clients on " + host + ": " + lastLine(err.Error())}
	}
	v, err := rex.DecodeJSON(out)
	if err != nil {
		return "", "", &errUseSSH{"bad client list from " + host}
	}
	cs := rex.ParseClients(v)
	for _, c := range cs {
		if c.Name == "rexmod" && !c.Lost && strings.HasPrefix(c.Label, serveLabel+"#") && !strings.Contains(c.Label, "#starting-") && c.Kind == "peer" {
			if main == "" || c.ID < main {
				main = c.ID
			}
		}
	}
	if main == "" {
		return "", "", nil
	}
	for _, c := range cs {
		if c.Name == "rexmod" && !c.Lost && c.Label == serveUILabel && c.Kind == "peer" && c.ID > main {
			if ui == "" || c.ID < ui {
				ui = c.ID
			}
		}
	}
	return main, ui, nil
}

// noUIClient in the cache: the service has no interactive client.
const noUIClient = "none"

func uiKey(host string) string { return host + "#ui" }

// interactive says whether a command is one a person starts, which goes to
// the service's interactive client.
func interactive(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "open", "pick", "follow":
		return true
	case "comments":
		return len(args) > 1 && args[1] == "send"
	}
	return false
}

// uiServiceClient is host's interactive client, or "" to use the main one.
// Not cached yet (a main client found before there were two), it lists the
// clients once.
func uiServiceClient(host string) string {
	m := loadServeClients()
	if id, ok := m[uiKey(host)]; ok {
		if id == noUIClient {
			return ""
		}
		return id
	}
	main, ui, err := findServices(host)
	if err != nil || main == "" {
		return ""
	}
	rememberServices(host, main, ui)
	return ui
}

// rememberServices caches host's main and interactive clients.
func rememberServices(host, main, ui string) {
	m := loadServeClients()
	m[host] = main
	if ui == "" {
		ui = noUIClient
	}
	m[uiKey(host)] = ui
	saveServeClients(m)
}

// startService starts rexmod serve on host in a pane of its own (it leaves
// the window itself) and waits for it to show up. /bin/sh expands $HOME
// for a ~/ path without reading any shell startup file.
func startService(host string) (string, error) {
	unlock, ok := lockServeStart(host)
	if !ok {
		// Another rexmod here is starting it: wait for that one.
		return waitService(host, "", "")
	}
	defer unlock()
	_, bin := loadConfig().remote(host)
	if rest, ok := strings.CutPrefix(bin, "~/"); ok {
		bin = `"$HOME"/` + shQuote(rest)
	} else {
		bin = shQuote(bin)
	}
	out, err := rexCall("-S", host, "new", "rexmod", "--shell", "none", "--json", "--", "/bin/sh", "-c", "exec "+bin+" serve")
	if err != nil {
		return "", &errUseSSH{"can't start rexmod serve on " + host + ": " + lastLine(err.Error())}
	}
	var created struct {
		SessionID string `json:"session_id"`
		Windows   []struct {
			BlockIDs []string `json:"block_ids"`
		} `json:"initial_windows"`
	}
	_ = json.Unmarshal(out, &created)
	block := ""
	if len(created.Windows) > 0 && len(created.Windows[0].BlockIDs) > 0 {
		block = created.Windows[0].BlockIDs[0]
	}
	id, err := waitService(host, created.SessionID, block)
	if err != nil && created.SessionID != "" {
		// Most likely an older rexmod without serve: leave no session behind.
		_, _ = rexCall("-S", host, "kill", created.SessionID)
	}
	return id, err
}

// waitService waits for host's service to show up. With the pane we
// started it in, one whose program already ended (an older rexmod without
// serve prints its usage and exits) fails at once instead of at the
// deadline.
func waitService(host, session, block string) (string, error) {
	deadline := time.Now().Add(serveStartWait)
	didnt := &errUseSSH{"rexmod serve didn't start on " + host + " (an older rexmod there?)"}
	for {
		id, err := findService(host)
		if err != nil {
			return "", err
		}
		if id != "" {
			return id, nil
		}
		if block != "" && paneEnded(host, session, block) {
			return "", didnt
		}
		if time.Now().After(deadline) {
			return "", didnt
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// paneEnded says whether the program of a pane on host has exited: Rex
// keeps a pane whose program failed, and reports how it exited. Unknown
// counts as still running.
func paneEnded(host, session, block string) bool {
	out, err := rexCall("-S", host, "--timeout", hostTimeout, "block", "call", "-s", session, "-b", block, "process")
	if err != nil {
		return false
	}
	var p struct {
		LastExit json.RawMessage `json:"last_exit"`
	}
	return json.Unmarshal(out, &p) == nil && len(p.LastExit) > 0 && string(p.LastExit) != "null"
}

// lockServeStart keeps two rexmods of this machine from starting a service
// on host at once. A lock older than 10s is a leftover.
func lockServeStart(host string) (unlock func(), ok bool) {
	p := filepath.Join(cacheDir(), "serve-start-"+sanitize(host)+".lock")
	if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > 10*time.Second {
		os.Remove(p)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, false
	}
	f.Close()
	return func() { os.Remove(p) }, true
}

// serve-clients.json remembers each host's service client ID.
func serveClientsPath() string { return filepath.Join(cacheDir(), "serve-clients.json") }

func loadServeClients() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(serveClientsPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	if m == nil {
		m = map[string]string{}
	}
	return m
}

func saveServeClients(m map[string]string) { _ = writeJSONAtomic(serveClientsPath(), m) }

func forgetServiceClient(host string) {
	m := loadServeClients()
	delete(m, host)
	delete(m, uiKey(host))
	saveServeClients(m)
}

// remoteChannelSSH says whether the config forces the ssh channel.
func remoteChannelSSH() bool { return loadConfig().RemoteChannel == "ssh" }

// runOnHost runs rexmod args on host: through its service over Rex, or over
// ssh when the config says so or Rex can't serve the call.
func runOnHost(host string, args []string) ([]byte, error) {
	if !remoteChannelSSH() {
		out, err := runViaService(host, args)
		var useSSH *errUseSSH
		if !errors.As(err, &useSSH) {
			return out, err
		}
		fmt.Fprintf(os.Stderr, "rexmod: %s: using ssh (%s)\n", host, useSSH.Why)
	}
	return runRemote(host, args)
}
