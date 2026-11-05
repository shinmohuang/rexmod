package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var csi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// screenText is the rendered rows, escapes taken out.
func screenText(b []byte) []string {
	parts := regexp.MustCompile(`\x1b\[\d+;1H`).Split(string(b), -1)
	var rows []string
	for _, p := range parts[1:] {
		rows = append(rows, csi.ReplaceAllString(p, ""))
	}
	return rows
}

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	os.WriteFile(p, []byte(body), 0o644)
	return p
}

func TestPreviewCentersTheLine(t *testing.T) {
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	p := &Preview{File: writeFile(t, "a.txt", strings.Join(lines, "\n")), Line: 50, Bg: "#101010", Fg: "#e0e0e0", Dim: "#808080"}
	rows := screenText(p.Render(40, 12)) // 10 rows of text
	if len(rows) != 10 || rows[0] != " 45 line 45" || rows[5] != " 50 line 50" || rows[9] != " 54 line 54" {
		t.Errorf("rows %q", rows)
	}
	b := string(p.Render(40, 12))
	if !strings.Contains(b, "\x1b[48;2;16;16;16m") || !strings.Contains(b, "\x1b[38;2;224;224;224m") || !strings.Contains(b, "\x1b[2J") {
		t.Errorf("colors or clear missing: %q", b[:60])
	}
	// near the top: from line 1
	p.Line = 2
	if rows := screenText(p.Render(40, 12)); rows[0] != "  1 line 1" {
		t.Errorf("top: %q", rows[0])
	}
}

func TestPreviewFitsAndIsSafe(t *testing.T) {
	body := "\tx\n" + strings.Repeat("y", 100) + "\n" + "evil\x1b]52;c;aGk=\x07end\n" + "中文中文中文中文\n"
	p := &Preview{File: writeFile(t, "b.txt", body)}
	out := p.Render(20, 10)
	rows := screenText(out)
	if rows[0] != "  1     x" {
		t.Errorf("tab: %q", rows[0])
	}
	if len(rows[1]) != 4+16 {
		t.Errorf("long line not cut to the width: %q", rows[1])
	}
	if strings.Contains(string(out), "\x1b]52") || strings.Contains(string(out), "\x07") {
		t.Error("an escape from the file reached the terminal")
	}
	if rows[3] != "  4 中文中文中文中文" {
		t.Errorf("wide: %q", rows[3])
	}
	p.File = writeFile(t, "c.txt", strings.Repeat("中", 20))
	if rows := screenText(p.Render(20, 10)); rows[0] != "  1 中中中中中中中中" { // 16 cells
		t.Errorf("wide cut: %q", rows[0])
	}
}

func TestPreviewNothingToShow(t *testing.T) {
	for name, p := range map[string]*Preview{
		"nil":     nil,
		"no file": {},
		"missing": {File: "/no/such/file"},
		"binary":  {File: writeFile(t, "bin", "ELF\x00\x01\x02")},
	} {
		if b := p.Render(80, 24); b != nil {
			t.Errorf("%s: rendered %q", name, b)
		}
	}
	if b := (&Preview{File: writeFile(t, "d", "x")}).Render(4, 24); b != nil {
		t.Error("rendered into 4 columns")
	}
}

func TestPreviewDue(t *testing.T) {
	if previewDue(nil) || previewDue(&Pane{}) {
		t.Error("due without a pane or a terminal")
	}
	p := &Pane{surface: &Surface{}}
	if !previewDue(p) {
		t.Error("not due before Neovim drew")
	}
	p.framed = true
	if previewDue(p) {
		t.Error("due after Neovim drew")
	}
}
