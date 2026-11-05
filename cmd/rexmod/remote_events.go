package main

// rexmod remote-events --host H: one line, "changed", each time the rexmod
// service on Rex host H says a repository it watches changed (it bumps its
// client label, which Rex sends as client_changed). The panel runs it for
// a remote target and refreshes on each line; it ends when the connection
// does, and the panel starts it again.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

func cmdRemoteEvents(args []string) error {
	fs := flag.NewFlagSet("remote-events", flag.ExitOnError)
	host := fs.String("host", "", "Rex host")
	parse(fs, args)
	if *host == "" {
		return fmt.Errorf("remote-events: --host is required")
	}
	cmd := exec.Command(rexBin(), "-S", *host, "events", "--all", "--json")
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Don't leave rex streaming when we go: on a signal, or when stdin
	// closes (the panel gives us a pipe and dies with it).
	stop := func() {
		_ = cmd.Process.Kill()
		os.Exit(0)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { <-sigs; stop() }()
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		stop()
	}()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	w := bufio.NewWriter(os.Stdout)
	for sc.Scan() {
		if serviceChanged(sc.Bytes()) {
			w.WriteString("changed\n")
			w.Flush()
		}
	}
	return cmd.Wait()
}

// serviceChanged says whether an event line from rex events --json is the
// rexmod service announcing a change: a client_changed whose label is
// rexmod-serve#N (not a service still starting).
func serviceChanged(line []byte) bool {
	var e struct {
		Type    string `json:"type"`
		Payload struct {
			Client struct {
				Info struct {
					Name  string `json:"name"`
					Label string `json:"label"`
				} `json:"info"`
			} `json:"client"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &e) != nil || e.Type != "client_changed" {
		return false
	}
	l := e.Payload.Client.Info.Label
	return e.Payload.Client.Info.Name == "rexmod" && strings.HasPrefix(l, serveLabel+"#") && !strings.Contains(l, "#starting-")
}
