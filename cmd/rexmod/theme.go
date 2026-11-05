package main

// The editor's colors follow Rex's theme, which lives in the Mac's settings
// (the app's defaults and the themes.json in its bundle). A remote host has
// neither, so the Mac reads the theme and hands it over with open and pick;
// the host keeps it in a file the editor's colorscheme (rex_theme.lua) and
// the picker's preview read.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const rexThemesGlob = "/Applications/Rex*.app/Contents/Resources/SuperUI_SuperDesignTokens.bundle/Contents/Resources/themes.json"

// rexThemeEntry is a themes.json entry: its ID and colors (source).
type rexThemeEntry struct {
	ID     string          `json:"id"`
	Source json.RawMessage `json:"source"`
}

// rexThemeSnapshot is what the editor needs of Rex's theme: the appearance
// in effect ("dark" or "light", never "system") and the theme of each.
type rexThemeSnapshot struct {
	Appearance string         `json:"appearance"`
	Dark       *rexThemeEntry `json:"dark,omitempty"`
	Light      *rexThemeEntry `json:"light,omitempty"`
}

// rexThemePath is where a host keeps the theme the Mac handed over.
func rexThemePath() string { return filepath.Join(cacheDir(), "rex-theme.json") }

func rexDefault(key string) string {
	out, _ := exec.Command("defaults", "read", "com.superlogical.rex", key).Output()
	return strings.TrimSpace(string(out))
}

// readRexTheme reads Rex's theme on the Mac.
func readRexTheme() (*rexThemeSnapshot, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("Rex's theme is read on macOS")
	}
	snap := &rexThemeSnapshot{Appearance: rexBackground()}
	ids := map[string]string{"dark": rexDefault("Workspace.darkThemeID"), "light": rexDefault("Workspace.lightThemeID")}
	themes := bundledThemes()
	custom := customThemes()
	for bg, id := range ids {
		if id == "" {
			continue
		}
		var found *rexThemeEntry
		for i := range themes {
			if themes[i].ID == id {
				found = &themes[i]
				break
			}
		}
		if found == nil {
			found = custom[id]
		}
		if bg == "dark" {
			snap.Dark = found
		} else {
			snap.Light = found
		}
	}
	if snap.Dark == nil && snap.Light == nil {
		return nil, fmt.Errorf("can't find Rex's themes")
	}
	return snap, nil
}

func bundledThemes() []rexThemeEntry {
	paths, _ := filepath.Glob(rexThemesGlob)
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var themes []rexThemeEntry
		if json.Unmarshal(b, &themes) == nil {
			return themes
		}
	}
	return nil
}

// customThemes are themes imported into Rex: JSON kept as plist data in
// the defaults key Workspace.customThemes, colors as sRGB channels 0–1.
func customThemes() map[string]*rexThemeEntry {
	plist, err := exec.Command("defaults", "export", "com.superlogical.rex", "-").Output()
	if err != nil {
		return nil
	}
	cmd := exec.Command("plutil", "-extract", `Workspace\.customThemes`, "raw", "-o", "-", "-")
	cmd.Stdin = bytes.NewReader(plist)
	raw, err := cmd.Output()
	if err != nil {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil
	}
	return parseCustomThemes(data)
}

type rgb struct{ Red, Green, Blue float64 }

func (c rgb) hex() string {
	ch := func(v float64) int { return int(v*255 + 0.5) }
	return fmt.Sprintf("#%02x%02x%02x", ch(c.Red), ch(c.Green), ch(c.Blue))
}

// parseCustomThemes gives each custom theme the shape of a themes.json entry.
func parseCustomThemes(data []byte) map[string]*rexThemeEntry {
	var list []struct {
		ID      string `json:"id"`
		Palette struct {
			Background          rgb   `json:"background"`
			Foreground          rgb   `json:"foreground"`
			SelectionBackground rgb   `json:"selectionBackground"`
			SelectionForeground rgb   `json:"selectionForeground"`
			Ansi                []rgb `json:"ansi"`
		} `json:"palette"`
	}
	if json.Unmarshal(data, &list) != nil {
		return nil
	}
	out := map[string]*rexThemeEntry{}
	for _, t := range list {
		p := t.Palette
		ansi := make([]string, len(p.Ansi))
		for i, c := range p.Ansi {
			ansi[i] = c.hex()
		}
		src, _ := json.Marshal(map[string]any{
			"background":          p.Background.hex(),
			"foreground":          p.Foreground.hex(),
			"selectionBackground": p.SelectionBackground.hex(),
			"selectionForeground": p.SelectionForeground.hex(),
			"ansi":                ansi,
		})
		out[t.ID] = &rexThemeEntry{ID: t.ID, Source: src}
	}
	return out
}

// rexThemeArg is the Mac's theme as an argument for a host's open or pick,
// or "" when it can't be read (the host keeps what it has).
var rexThemeArg = func() string {
	key := rexThemeKey()
	if key != "" {
		if b, err := os.ReadFile(rexThemeCachePath()); err == nil {
			var c rexThemeCache
			if json.Unmarshal(b, &c) == nil && c.Key == key && c.Arg != "" {
				return c.Arg
			}
		}
	}
	snap, err := readRexTheme()
	if err != nil {
		return ""
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return ""
	}
	if key != "" {
		_ = writeJSONAtomic(rexThemeCachePath(), rexThemeCache{Key: key, Arg: string(b)})
	}
	return string(b)
}

// rexThemeCache is the theme read last, with what it was read from.
type rexThemeCache struct {
	Key string `json:"key"`
	Arg string `json:"arg"`
}

func rexThemeCachePath() string { return filepath.Join(cacheDir(), "rex-theme-local.json") }

// rexThemePlist is Rex's settings file; a variable for tests.
var rexThemePlist = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Preferences", "com.superlogical.rex.plist")
}

// rexThemeKey names what the theme is read from, by modification time:
// Rex's settings, its themes.json, and the global settings that hold the
// system appearance (Rex's "system" follows it). "" when there's nothing
// to go by.
func rexThemeKey() string {
	st, err := os.Stat(rexThemePlist())
	if err != nil {
		return ""
	}
	key := fmt.Sprintf("%d", st.ModTime().UnixNano())
	paths, _ := filepath.Glob(rexThemesGlob)
	paths = append(paths, filepath.Join(filepath.Dir(rexThemePlist()), ".GlobalPreferences.plist"))
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil {
			key += fmt.Sprintf(":%s@%d", p, st.ModTime().UnixNano())
		}
	}
	return key
}

// saveRexTheme keeps a theme the Mac handed over, if it is one.
func saveRexTheme(arg string) {
	var snap rexThemeSnapshot
	if arg == "" || json.Unmarshal([]byte(arg), &snap) != nil || snap.Appearance == "" {
		return
	}
	tmp := rexThemePath() + ".new"
	if os.WriteFile(tmp, []byte(arg), 0o644) == nil {
		_ = os.Rename(tmp, rexThemePath())
	}
}

// rexThemeColors are the background, foreground and dim (bright black)
// colors of Rex's theme in effect: read here on the Mac, or as the Mac
// handed it over. "" each when there's no theme.
func rexThemeColors() (bg, fg, dim string) {
	var raw []byte
	if runtime.GOOS == "darwin" {
		raw = []byte(rexThemeArg())
	} else {
		raw, _ = os.ReadFile(rexThemePath())
	}
	var snap rexThemeSnapshot
	if json.Unmarshal(raw, &snap) != nil {
		return "", "", ""
	}
	t := snap.Dark
	if snap.Appearance == "light" || t == nil {
		t = snap.Light
	}
	if t == nil {
		t = snap.Dark
	}
	if t == nil {
		return "", "", ""
	}
	var src struct {
		Background string   `json:"background"`
		Foreground string   `json:"foreground"`
		Ansi       []string `json:"ansi"`
	}
	_ = json.Unmarshal(t.Source, &src)
	if len(src.Ansi) > 8 {
		dim = src.Ansi[8]
	}
	return src.Background, src.Foreground, dim
}

// savedAppearance is the appearance in the theme the Mac handed over.
func savedAppearance() string {
	b, err := os.ReadFile(rexThemePath())
	if err != nil {
		return ""
	}
	var snap rexThemeSnapshot
	if json.Unmarshal(b, &snap) != nil {
		return ""
	}
	return snap.Appearance
}
