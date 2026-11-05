package daemon

// The preview: what the editor's first pane shows while Neovim starts, when
// that takes longer than PreviewDelay (a cold start on a slow disk can take
// a second): the file around the line it opens at, with line numbers, in
// the theme's colors. Neovim's first frame clears the pane and draws all of
// it (PaneView starts Full), so nothing of the preview stays.

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// PreviewDelay: a Neovim that draws within this never shows a preview.
const PreviewDelay = 150 * time.Millisecond

// Preview is the file to show, and colors as "#rrggbb" ("" for the
// terminal's own).
type Preview struct {
	File        string
	Line        int // 1-based; 0 for the top
	Bg, Fg, Dim string
}

const previewTab = 4

// Render draws the preview for a terminal of cols × rows, or nil when there
// is nothing to show (no file, unreadable, binary).
func (p *Preview) Render(cols, rows int) []byte {
	if p == nil || p.File == "" || cols < 8 || rows < 1 {
		return nil
	}
	// what Neovim leaves for text: the status line and the command line
	text := max(rows-2, 1)
	first := 1
	if p.Line > text/2 {
		first = p.Line - text/2
	}
	lines, ok := readLines(p.File, first, text)
	if !ok {
		return nil
	}
	last := first + len(lines) - 1
	gutter := max(len(strconv.Itoa(max(last, 1))), 3) + 1

	var b bytes.Buffer
	b.WriteString("\x1b[0m")
	bg, fg, dim := sgr(48, p.Bg), sgr(38, p.Fg), sgr(38, p.Dim)
	b.WriteString(bg + fg + "\x1b[2J\x1b[H")
	for i, line := range lines {
		n := first + i
		fmt.Fprintf(&b, "\x1b[%d;1H", i+1)
		if n == p.Line {
			b.WriteString(fg)
		} else {
			b.WriteString(dim)
		}
		fmt.Fprintf(&b, "%*d ", gutter-1, n)
		b.WriteString(fg)
		b.WriteString(fitWidth(line, cols-gutter))
	}
	b.WriteString("\x1b[0m")
	return b.Bytes()
}

// readLines reads count lines of path from line first; ok=false for a file
// that can't be read or is binary.
func readLines(path string, first, count int) ([]string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	head := make([]byte, 8000)
	n, _ := f.Read(head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, false
	}
	_, _ = f.Seek(0, 0)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var out []string
	for i := 1; sc.Scan() && len(out) < count; i++ {
		if i >= first {
			out = append(out, sc.Text())
		}
	}
	return out, true
}

// fitWidth makes line safe for the terminal and at most width cells wide:
// tabs become spaces, control characters (escapes in the file must not
// reach the terminal) and invalid UTF-8 become '?'.
func fitWidth(line string, width int) string {
	var b strings.Builder
	w := 0
	for i := 0; i < len(line); {
		r, size := utf8.DecodeRuneInString(line[i:])
		i += size
		switch {
		case r == '\t':
			n := previewTab - w%previewTab
			if w+n > width {
				return b.String()
			}
			b.WriteString(strings.Repeat(" ", n))
			w += n
			continue
		case r == utf8.RuneError && size == 1, r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0:
			r = '?'
		}
		cw := runeWidth(r)
		if w+cw > width {
			break
		}
		b.WriteRune(r)
		w += cw
	}
	return b.String()
}

// runeWidth: 2 for the wide ranges (CJK, Hangul, fullwidth forms, emoji), 1
// otherwise. Close enough for a preview; Neovim redraws it all.
func runeWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1faff, r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

// sgr is the escape for a "#rrggbb" color as foreground (38) or background
// (48); "" for anything else.
func sgr(kind int, hex string) string {
	if len(hex) != 7 || hex[0] != '#' {
		return ""
	}
	v, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("\x1b[%d;2;%d;%d;%dm", kind, v>>16, v>>8&0xff, v&0xff)
}
