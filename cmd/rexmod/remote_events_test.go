package main

import "testing"

func TestServiceChanged(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{`{"type":"client_changed","payload":{"client":{"client_id":"client:a","info":{"kind":"other","name":"rexmod","label":"rexmod-serve#12"}}}}`, true},
		{`{"type":"client_changed","payload":{"client":{"info":{"name":"rexmod","label":"rexmod-serve#starting-0a1b"}}}}`, false},
		{`{"type":"client_changed","payload":{"client":{"info":{"name":"Rex Beta","label":"Mac mini"}}}}`, false},
		{`{"type":"client_changed","payload":{"client":{"info":{"name":"rex","label":"rexmod-serve#1"}}}}`, false}, // not the service
		{`{"type":"client_joined","payload":{"client":{"info":{"name":"rexmod","label":"rexmod-serve#0"}}}}`, false},
		{`{"type":"session_created"}`, false},
		{`not json`, false},
	} {
		if got := serviceChanged([]byte(tc.line)); got != tc.want {
			t.Errorf("%s: got %v", tc.line, got)
		}
	}
}
