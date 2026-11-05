package main

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

type fakeCaller struct {
	calls []string
	fail  map[string]bool
}

func (f *fakeCaller) Call(method string, payload any, _ time.Duration) (any, error) {
	f.calls = append(f.calls, method)
	if f.fail[method] {
		return nil, errors.New("rex: not_found")
	}
	switch method {
	case "session.list":
		return map[string]any{"sessions": []any{
			map[string]any{"session_id": "session:a", "label": "work"},
			map[string]any{"session_id": "session:b", "label": "dup"},
			map[string]any{"session_id": "session:c", "label": "dup"},
		}}, nil
	case "session.view":
		return map[string]any{"session_id": payload.(map[string]any)["session_id"], "windows": []any{}}, nil
	case "block.method":
		p := payload.(map[string]any)
		return map[string]any{"method": p["method"], "block": p["block_id"]}, nil
	}
	return map[string]any{}, nil
}

func withFakeCaller(t *testing.T, f *fakeCaller) {
	t.Helper()
	old := rexAPIDial
	rexAPIDial = func(string) (rexCaller, error) { return f, nil }
	t.Setenv("REX_SERVER", "unix:///tmp/fake.sock")
	resetRexAPI()
	t.Cleanup(func() { rexAPIDial = old; resetRexAPI() })
}

func TestRexViaAPI(t *testing.T) {
	f := &fakeCaller{}
	withFakeCaller(t, f)

	out, ok := rexViaAPI([]string{"api", "call", "-s", "work", "session.view"})
	var v map[string]any
	if !ok || json.Unmarshal(out, &v) != nil || v["session_id"] != "session:a" {
		t.Fatalf("view by label: %q %v", out, ok)
	}
	out, ok = rexViaAPI([]string{"block", "call", "-s", "session:a", "-b", "block:1", "process"})
	if !ok || json.Unmarshal(out, &v) != nil || v["method"] != "process" || v["block"] != "block:1" {
		t.Fatalf("block call: %q %v", out, ok)
	}
	if out, ok = rexViaAPI([]string{"focus", "-s", "session:a", "block:1"}); !ok || out != nil {
		t.Errorf("focus: %q %v", out, ok)
	}
	if _, ok = rexViaAPI([]string{"block", "close", "-s", "session:a", "block:1"}); !ok {
		t.Error("block close not over the API")
	}
	if _, ok = rexViaAPI([]string{"ls", "--json"}); !ok {
		t.Error("ls --json not over the API")
	}
	// one session.list to resolve the label (ls is its own), one attach
	want := []string{"session.list", "session.attach", "session.view", "block.method", "session.focus_block", "block.close", "session.list"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls %v, want %v", f.calls, want)
	}
}

// Whatever the API path doesn't know, or can't answer, the CLI runs.
func TestRexViaAPIFallsBack(t *testing.T) {
	f := &fakeCaller{fail: map[string]bool{"session.view": true}}
	withFakeCaller(t, f)
	for _, args := range [][]string{
		{"api", "call", "-s", "dup", "session.view"},                    // ambiguous label
		{"api", "call", "-s", "01a1", "session.view"},                   // ID suffix
		{"api", "call", "-s", "session:a", "session.view"},              // the API's error: the CLI's words
		{"-S", "gpu2", "api", "call", "-s", "x", "session.view"},        // another server
		{"block", "call", "-s", "session:a", "-b", "editor", "process"}, // a block label
		{"split", "-s", "session:a", "--", "true"},                      // not mapped
		{"block", "call", "-s", "session:a", "-b", "block:1", "write", "@f"},
	} {
		if _, ok := rexViaAPI(args); ok {
			t.Errorf("%v went over the API", args)
		}
	}
}

func TestRexViaAPINeedsUnixSocket(t *testing.T) {
	f := &fakeCaller{}
	withFakeCaller(t, f)
	t.Setenv("REX_SERVER", "")
	resetRexAPI()
	if _, ok := rexViaAPI([]string{"ls", "--json"}); ok || len(f.calls) != 0 {
		t.Error("used the API without REX_SERVER")
	}
}

// In a Rex pane, the API and the CLI give the same answers.
func TestRexViaAPIMatchesCLI(t *testing.T) {
	sess, blk := os.Getenv("REX_SESSION"), os.Getenv("REX_BLOCK")
	if sess == "" || blk == "" || os.Getenv("REX_SERVER") == "" {
		t.Skip("not in a Rex pane")
	}
	resetRexAPI()
	t.Cleanup(resetRexAPI)
	for _, args := range [][]string{
		{"api", "call", "-s", sess, "session.view"},
		{"block", "call", "-s", sess, "-b", blk, "process"},
		{"ls", "--json"},
	} {
		api, ok := rexViaAPI(args)
		if !ok {
			t.Fatalf("%v: not over the API", args)
		}
		cli, err := rexCLI(args...)
		if err != nil {
			t.Fatal(err)
		}
		var a, c any
		_ = json.Unmarshal(api, &a)
		_ = json.Unmarshal(cli, &c)
		if !reflect.DeepEqual(a, c) {
			t.Errorf("%v: API %s, CLI %s", args, api, cli)
		}
	}
}
