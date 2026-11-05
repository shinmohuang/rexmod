package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is ~/.config/rexmod/config.json, written by install.sh.
type Config struct {
	// NvimAppName runs the editor pane with NVIM_APPNAME set, for machines
	// where the bundled editor config lives next to the user's own.
	NvimAppName string `json:"nvim_appname,omitempty"`
	// RemoteHosts says how to reach the Rex hosts (rex hosts list) the panel
	// follows agents on, by host label. A host without an entry is reached
	// over ssh at its label.
	RemoteHosts map[string]RemoteHost `json:"remote_hosts,omitempty"`
}

// RemoteHost is how rexmod runs itself on a Rex host.
type RemoteHost struct {
	SSH    string `json:"ssh,omitempty"`    // ssh destination, such as an alias of ~/.ssh/config
	Rexmod string `json:"rexmod,omitempty"` // rexmod on the host; ~/ is its home
}

// defaultRemoteRexmod is where rexmod lives on a host whose entry doesn't say.
const defaultRemoteRexmod = "~/.local/bin/rexmod"

// remote returns the ssh destination and the rexmod path for a Rex host.
func (c Config) remote(label string) (dest, bin string) {
	h := c.RemoteHosts[label]
	dest, bin = h.SSH, h.Rexmod
	if dest == "" {
		dest = label
	}
	if bin == "" {
		bin = defaultRemoteRexmod
	}
	return dest, bin
}

// hasRemote tells whether the config names an ssh destination for label.
func (c Config) hasRemote(label string) bool { return c.RemoteHosts[label].SSH != "" }

func loadConfig() Config {
	var c Config
	home, err := os.UserHomeDir()
	if err != nil {
		return c
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "rexmod", "config.json"))
	if err == nil {
		c = parseConfig(b)
	}
	return c
}

func parseConfig(b []byte) Config {
	var c Config
	_ = json.Unmarshal(b, &c)
	return c
}

// nvimEnv is extra environment for every Neovim rexmod starts.
func nvimEnv() map[string]string {
	env := map[string]string{}
	if app := loadConfig().NvimAppName; app != "" {
		env["NVIM_APPNAME"] = app
	}
	return env
}
