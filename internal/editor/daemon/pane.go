package daemon

// Ported from neorex (MIT): src/daemon/Pane.zig, Panes.zig, Surface.zig,
// NvimSession.zig and WindowChanges.zig.

import (
	"bufio"
	"net"
	"os"
	"slices"
	"syscall"

	"github.com/alpcanaydin/rexmod/internal/editor/nvim"
	"github.com/alpcanaydin/rexmod/internal/editor/screen"
	"github.com/alpcanaydin/rexmod/internal/editor/term"
)

// Surface is one pane's terminal.
type Surface struct {
	helper *net.UnixConn // closing it makes the helper exit, and Rex close the pane
	file   *os.File      // the terminal, non-blocking, read in a goroutine
	term   *term.Terminal
	view   *screen.PaneView
	fwd    *nvim.Forwarder
	// Pixels per cell, from the terminal; 0 when it doesn't say.
	cellW, cellH uint16
}

func newSurface(helper *net.UnixConn, tty *os.File) (*Surface, error) {
	fd, err := syscall.Dup(int(tty.Fd()))
	tty.Close()
	if err != nil {
		return nil, err
	}
	t, err := term.Open(fd, 32*1024)
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	_ = syscall.SetNonblock(fd, true)
	f := os.NewFile(uintptr(fd), "tty")
	// through the poller, so a full terminal doesn't fail a write
	t.Writer = bufio.NewWriterSize(f, 32*1024)
	s := &Surface{helper: helper, file: f, term: t, view: screen.NewPaneView(0, 0), fwd: nvim.NewForwarder()}
	s.fwd.SendFocus = false
	_, _ = t.WriteString(term.EditorOn)
	s.readSize()
	return s, nil
}

func (s *Surface) readSize() bool {
	ws, err := s.term.Size()
	if err != nil {
		return false
	}
	s.view.Resize(uint32(ws.Cols), uint32(ws.Rows))
	if ws.Cols > 0 && ws.Rows > 0 && ws.XPixel > 0 {
		s.cellW, s.cellH = ws.XPixel/ws.Cols, ws.YPixel/ws.Rows
	}
	return true
}

// destroy hangs up; restore is false when the terminal is already gone.
func (s *Surface) destroy(restore bool) {
	if restore {
		_, _ = s.term.WriteString(term.EditorOff)
		s.term.Close()
	}
	s.file.Close()
	s.helper.Close()
}

// windowSize is the pane less its message line, and never empty.
func (s *Surface) windowSize() [2]uint32 {
	c, r := s.view.Cols, s.view.Rows
	if c < 1 {
		c = 1
	}
	if r > 1 {
		r--
	} else {
		r = 1
	}
	return [2]uint32{c, r}
}

type closeReason int

const (
	notClosing closeReason = iota
	rexPaneClosed
	windowClosed
	requestFailed
	shutdown
)

type fitState int

const (
	fitNone fitState = iota
	fitAsked
	fitAnswered
)

// Layer is a Rex floating pane showing a group of floats, not a window.
type Layer struct {
	group screen.Group
	id    string // Rex's id, once made
	// Its floats being refitted to its first size: not drawn until Neovim
	// has answered and redrawn them.
	fit    fitState
	fitReq uint32
	// x, y, w, h as fractions of the window.
	bounds [4]float64
}

// Pane links a Rex pane to a Neovim window; the parts arrive in any order.
type Pane struct {
	id         uint32 // 0 is the startup pane
	block      string
	win        int64 // 0 while a replacement window opens
	surface    *Surface
	closing    closeReason
	layer      *Layer
	rect       *[4]float64 // where Rex put it: x, y, w, h
	awaiting   bool        // a new split at Rex's default size until the first resize
	framed     bool        // Neovim has drawn a window of its in it
	previewing bool        // the preview is up, until then
	lastAsk    struct{ grid, cols, rows uint32 }
}

func (p *Pane) layerShown() bool {
	return p.layer != nil && p.surface != nil && p.closing == notClosing && p.layer.fit == fitNone
}
func (p *Pane) layerHides() bool { return p.layer != nil && p.closing == notClosing }

type Panes struct {
	list   []*Pane
	nextID uint32
}

func (ps *Panes) add(p *Pane) *Pane { ps.list = append(ps.list, p); return p }
func (ps *Panes) addForWindow(win int64) *Pane {
	ps.nextID++
	return ps.add(&Pane{id: ps.nextID, win: win})
}
func (ps *Panes) addForGroup(g screen.Group) *Pane {
	ps.nextID++
	return ps.add(&Pane{id: ps.nextID, layer: &Layer{group: g}})
}

// remove hangs up p and forgets it. Set its closing first.
func (ps *Panes) remove(p *Pane) {
	if i := slices.Index(ps.list, p); i >= 0 {
		ps.list = slices.Delete(ps.list, i, i+1)
	}
	if p.surface != nil {
		p.surface.destroy(p.closing != rexPaneClosed)
	}
}
func (ps *Panes) byID(id uint32) *Pane {
	for _, p := range ps.list {
		if p.id == id {
			return p
		}
	}
	return nil
}
func (ps *Panes) byWin(win int64) *Pane {
	if win == 0 {
		return nil
	}
	for _, p := range ps.list {
		if p.win == win {
			return p
		}
	}
	return nil
}
func (ps *Panes) byBlock(b string) *Pane {
	for _, p := range ps.list {
		if b != "" && p.block == b {
			return p
		}
	}
	return nil
}
func (ps *Panes) byLayerKey(k int64) *Pane {
	for _, p := range ps.list {
		if p.layer != nil && p.layer.group.Key == k {
			return p
		}
	}
	return nil
}

// changes tracks windows partway through a change.
type changes struct {
	asked, closing, reopening map[int64]bool
	splits                    map[int64]nvim.Split
}

func newChanges() *changes {
	return &changes{map[int64]bool{}, map[int64]bool{}, map[int64]bool{}, map[int64]nvim.Split{}}
}

func (c *changes) forgetGone(g *screen.Grids) {
	gone := func(w int64) bool { _, ok := g.GridOf(w); return !ok }
	for _, m := range []map[int64]bool{c.asked, c.closing, c.reopening} {
		for w := range m {
			if gone(w) {
				delete(m, w)
			}
		}
	}
	for w := range c.splits {
		if gone(w) {
			delete(c.splits, w)
		}
	}
}

func (c *changes) shouldAdopt(win int64) bool {
	if c.asked[win] {
		return false
	}
	c.asked[win] = true
	return true
}

func (c *changes) takeReopening(win int64) bool {
	ok := c.reopening[win]
	delete(c.reopening, win)
	return ok
}

// splitOf is where Neovim put win; beside the current window if it didn't say.
func (c *changes) splitOf(win int64) nvim.Split {
	if s, ok := c.splits[win]; ok {
		return s
	}
	return nvim.Split{Win: win, SideBySide: true}
}

// nvimState is our side of the Neovim connection.
type nvimState struct {
	c          *nvim.Client
	currentWin int64
	// An nvim_set_current_win of ours not answered yet: reports before it are stale.
	currentReq   uint32
	channelReq   uint32
	setupReq     uint32
	ready        bool
	uiFocused    bool
	currentFloat int64
	editorSize   [2]uint32
	entered      []int64
	cell         [2]uint16
}

func (n *nvimState) noteEntered(win int64) {
	if slices.Contains(n.entered, win) {
		return
	}
	if len(n.entered) == 32 {
		n.entered = n.entered[1:]
	}
	n.entered = append(n.entered, win)
}

// fitUITo spans the editor over the Rex window, judged from p's share of it.
func (n *nvimState) fitUITo(p *Pane) error {
	if p.surface == nil {
		return nil
	}
	cols, rows := p.surface.view.Cols, p.surface.view.Rows
	if r := p.rect; r != nil && r[2] > 0 && r[3] > 0 {
		cols = max(cols, uint32(float64(cols)/r[2]+0.5))
		rows = max(rows, uint32(float64(rows)/r[3]+0.5))
	}
	size := [2]uint32{cols, rows}
	if size == n.editorSize {
		return nil
	}
	n.editorSize = size
	return n.c.UITryResize(cols, rows)
}
