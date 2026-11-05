package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// rexCall runs the rex CLI; host lookup goes through it so tests can stand
// in for Rex.
var rexCall = rexRun

// hostTimeout bounds each question to a Rex host, so one that's away can't
// hold up the panel.
const hostTimeout = "3s"

// sessionHost returns the Rex host label of the server that has session (an
// ID or a label), or "" when this machine's own Rex server has it. A session
// here always wins, and finding it here never asks another host.
func sessionHost(session string) (string, error) {
	if session == "" {
		return "", nil
	}
	if out, err := rexCall("api", "call", "-s", session, "session.view"); err == nil {
		primeSessionView(session, out) // resolveTarget asks the same next
		return "", nil
	}
	cache := loadSessionHosts()
	if h, ok := cache[session]; ok {
		if _, err := rexCall("-S", h, "--timeout", hostTimeout, "api", "call", "-s", session, "session.view"); err == nil {
			return h, nil
		}
	}
	h, err := findSessionHost(session)
	if err != nil {
		return "", err
	}
	if h == "" {
		// Not anywhere we can reach: as before, the local lookup reports it.
		return "", nil
	}
	if cache[session] != h {
		cache[session] = h
		saveSessionHosts(cache)
	}
	return h, nil
}

type hostSessions struct {
	label  string
	hostID string
	match  bool
}

// findSessionHost asks every Rex host for its sessions. Labels that reach
// the same server (one host_id) count once; matches on more than one server
// are ambiguous.
func findSessionHost(session string) (string, error) {
	var list struct {
		Hosts []struct {
			Host struct {
				Label string `json:"label"`
			} `json:"host"`
		} `json:"hosts"`
	}
	out, err := rexCall("hosts", "list", "--json")
	if err != nil || json.Unmarshal(out, &list) != nil {
		return "", nil
	}
	res := make([]hostSessions, len(list.Hosts))
	var wg sync.WaitGroup
	for i, h := range list.Hosts {
		if h.Host.Label == "" {
			continue
		}
		wg.Add(1)
		go func(i int, label string) {
			defer wg.Done()
			res[i] = askHost(label, session)
		}(i, h.Host.Label)
	}
	wg.Wait()

	cfg := loadConfig()
	byServer := map[string][]string{}
	for _, r := range res {
		if r.match {
			byServer[r.hostID] = append(byServer[r.hostID], r.label)
		}
	}
	if len(byServer) > 1 {
		var all []string
		for _, ls := range byServer {
			all = append(all, ls...)
		}
		sort.Strings(all)
		return "", fmt.Errorf("session %q is on more than one Rex host (%s): give its ID", session, strings.Join(all, ", "))
	}
	for _, labels := range byServer {
		sort.Strings(labels)
		for _, l := range labels {
			if cfg.hasRemote(l) {
				return l, nil
			}
		}
		return labels[0], nil
	}
	return "", nil
}

// askHost reports whether the Rex host label has session, and which server
// it is.
func askHost(label, session string) hostSessions {
	r := hostSessions{label: label}
	var ls struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
			Label     string `json:"label"`
		} `json:"sessions"`
	}
	out, err := rexCall("-S", label, "--timeout", hostTimeout, "ls", "--json")
	if err != nil || json.Unmarshal(out, &ls) != nil {
		return r
	}
	for _, s := range ls.Sessions {
		if s.SessionID == session || s.Label == session {
			r.match = true
			break
		}
	}
	if !r.match {
		return r
	}
	var who struct {
		HostID     string `json:"host_id"`
		InstanceID string `json:"instance_id"`
	}
	if out, err := rexCall("-S", label, "--timeout", hostTimeout, "whoami", "--json"); err == nil && json.Unmarshal(out, &who) == nil {
		r.hostID = who.HostID
		if r.hostID == "" {
			r.hostID = who.InstanceID
		}
	}
	if r.hostID == "" {
		r.hostID = "label:" + label // can't tell: count it on its own
	}
	return r
}

// session-hosts.json remembers which host each remote session was found on,
// so a refresh asks one host, not all of them.
func sessionHostsPath() string { return filepath.Join(cacheDir(), "session-hosts.json") }

func loadSessionHosts() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(sessionHostsPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	if m == nil {
		m = map[string]string{}
	}
	return m
}

func saveSessionHosts(m map[string]string) {
	_ = writeJSONAtomic(sessionHostsPath(), m)
}

// primed is a session.view answer already in hand: the host check asks
// the local server for it, and resolveTarget would ask again. It serves the
// next sessionView of that session once.
var primed struct {
	sync.Mutex
	session string
	view    *rexView
}

func primeSessionView(session string, out []byte) {
	var v rexView
	if json.Unmarshal(out, &v) != nil {
		return
	}
	primed.Lock()
	primed.session, primed.view = session, &v
	primed.Unlock()
}

func takePrimedView(session string) *rexView {
	primed.Lock()
	defer primed.Unlock()
	if primed.view == nil || primed.session != session {
		return nil
	}
	v := primed.view
	primed.session, primed.view = "", nil
	return v
}
