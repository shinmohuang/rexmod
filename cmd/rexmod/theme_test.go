package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestParseCustomThemes(t *testing.T) {
	data := []byte(`[{"id":"mine","palette":{"background":{"red":0,"green":0,"blue":0},
	  "foreground":{"red":1,"green":1,"blue":1},"selectionBackground":{"red":0.5,"green":0,"blue":1},
	  "selectionForeground":{"red":1,"green":0.5,"blue":0},"ansi":[{"red":1,"green":0,"blue":0}]}}]`)
	got := parseCustomThemes(data)["mine"]
	if got == nil {
		t.Fatal("no theme")
	}
	var src struct {
		Background, Foreground, SelectionBackground string
		Ansi                                        []string
	}
	if json.Unmarshal(got.Source, &src) != nil || src.Background != "#000000" || src.Foreground != "#ffffff" ||
		src.SelectionBackground != "#8000ff" || len(src.Ansi) != 1 || src.Ansi[0] != "#ff0000" {
		t.Errorf("source: %s", got.Source)
	}
}

func TestSaveRexTheme(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
	saveRexTheme("")
	saveRexTheme("not json")
	if _, err := os.Stat(rexThemePath()); err == nil {
		t.Fatal("saved something that isn't a theme")
	}
	saveRexTheme(`{"appearance":"light","light":{"id":"x","source":{}}}`)
	if a := savedAppearance(); a != "light" {
		t.Errorf("appearance %q", a)
	}
}

// The theme is read again only when what it's read from changes.
func TestRexThemeArgCached(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Rex's theme is read on macOS")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	plist := filepath.Join(home, "rex.plist")
	os.WriteFile(plist, []byte("x"), 0o644)
	old := rexThemePlist
	rexThemePlist = func() string { return plist }
	t.Cleanup(func() { rexThemePlist = old })

	os.WriteFile(rexThemeCachePath(), []byte(`{"key":"`+rexThemeKey()+`","arg":"{\"appearance\":\"cached\"}"}`), 0o644)
	if got := rexThemeArg(); got != `{"appearance":"cached"}` {
		t.Errorf("same key: %q, want the cached theme", got)
	}
	later := time.Now().Add(time.Minute)
	os.Chtimes(plist, later, later)
	if got := rexThemeArg(); got == `{"appearance":"cached"}` {
		t.Error("settings changed, still the cached theme")
	}
}
