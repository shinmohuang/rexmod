//go:build darwin || linux

package main

import (
	"os"
	"testing"
)

func TestEditorPreviewArgs(t *testing.T) {
	old := rexThemeArg
	rexThemeArg = func() string { return "" }
	t.Cleanup(func() { rexThemeArg = old })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")

	p := editorPreview([]string{"--listen", "/tmp/s.sock", "+12", "/r/a.go"})
	if p == nil || p.File != "/r/a.go" || p.Line != 12 {
		t.Errorf("open's args: %+v", p)
	}
	if p := editorPreview([]string{"--listen", "/tmp/s.sock", "/r/a.go"}); p == nil || p.Line != 0 {
		t.Errorf("no line: %+v", p)
	}
	for _, args := range [][]string{{"--listen", "/tmp/s.sock"}, {"a.go", "b.go"}, {"-u", "init.lua"}} {
		if p := editorPreview(args); p != nil {
			t.Errorf("%v: %+v, want none", args, p)
		}
	}
}

func TestRexThemeColors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
	snap := `{"appearance":"light","dark":{"id":"d","source":{"background":"#000000","foreground":"#ffffff","ansi":["0","1","2","3","4","5","6","7","#555555"]}},` +
		`"light":{"id":"l","source":{"background":"#fafafa","foreground":"#111111","ansi":["0","1","2","3","4","5","6","7","#999999"]}}}`
	old := rexThemeArg
	rexThemeArg = func() string { return snap }
	t.Cleanup(func() { rexThemeArg = old })
	os.WriteFile(rexThemePath(), []byte(snap), 0o644) // as a host keeps it
	if bg, fg, dim := rexThemeColors(); bg != "#fafafa" || fg != "#111111" || dim != "#999999" {
		t.Errorf("light: %q %q %q", bg, fg, dim)
	}
	rexThemeArg = func() string { return "" }
	os.Remove(rexThemePath())
	if bg, fg, dim := rexThemeColors(); bg != "" || fg != "" || dim != "" {
		t.Errorf("no theme: %q %q %q", bg, fg, dim)
	}
}
