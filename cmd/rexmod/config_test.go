package main

import "testing"

func TestRemoteHostConfig(t *testing.T) {
	c := parseConfig([]byte(`{"remote_hosts": {
		"gpu2": {"ssh": "acps-gpu2"},
		"lab": {"ssh": "lab-box", "rexmod": "/opt/rexmod/bin/rexmod"}
	}}`))
	for _, tc := range []struct{ label, dest, bin string }{
		{"gpu2", "acps-gpu2", defaultRemoteRexmod},   // mapped, default path
		{"lab", "lab-box", "/opt/rexmod/bin/rexmod"}, // mapped, own path
		{"studio", "studio", defaultRemoteRexmod},    // not configured: the label
	} {
		dest, bin := c.remote(tc.label)
		if dest != tc.dest || bin != tc.bin {
			t.Errorf("remote(%q) = %q, %q; want %q, %q", tc.label, dest, bin, tc.dest, tc.bin)
		}
	}
	if !c.hasRemote("gpu2") || c.hasRemote("studio") {
		t.Error("hasRemote: want gpu2 mapped, studio not")
	}
	// An empty or broken file is no config, not an error.
	if d, _ := parseConfig([]byte(`{`)).remote("x"); d != "x" {
		t.Errorf("broken config: dest %q, want the label", d)
	}
}
