// Package daemon runs the editor: one headless Neovim behind every Rex pane
// it shows. Each Neovim window gets a Rex pane, and each group of floats
// that takes focus a Rex floating pane. Ported from neorex (MIT):
// src/daemon/Daemon.zig. The kqueue loop became goroutines feeding one
// channel; everything else runs on the loop's goroutine.
package daemon

import (
	"encoding/json"
	"log"
	"math"
	"net"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/alpcanaydin/rexmod/internal/editor/nvim"
	"github.com/alpcanaydin/rexmod/internal/editor/panehelper"
	"github.com/alpcanaydin/rexmod/internal/editor/rex"
	"github.com/alpcanaydin/rexmod/internal/editor/screen"
	"github.com/alpcanaydin/rexmod/internal/editor/term"
)

const (
	startupPaneID = 0
	// Losing focus waits this long: a move between panes is an out then an in.
	focusOutDelay = 80 * time.Millisecond
	// How long a new split waits for the app to size it.
	layoutWait = time.Second
	// The label every pane of ours gets in Rex.
	Label = "editor"
)

// trace logs what the editor does, for debugging.
var trace = os.Getenv("REXMOD_EDITOR_TRACE") != ""

// A new Rex terminal's size until the app lays it out.
var rexDefaultSize = [2]uint32{80, 24}

// About how many cells a Rex floating pane's frame takes, across and down.
var layerFrame = [2]int32{3, 2}

type StopReason int

const (
	running StopReason = iota
	NvimExited
	PaneGone
	Signal
)

func (r StopReason) String() string {
	return [...]string{"running", "nvim exited", "pane gone", "signal"}[r]
}

type event any

type (
	evNvim    struct{ data []byte }
	evNvimEOF struct{}
	evRex     struct{ ok bool }
	evAccept  struct{ h panehelper.Hello }
	evInput   struct {
		id   uint32
		data []byte
		eof  bool
	}
	evHelper struct {
		id   uint32
		data []byte
		eof  bool
	}
	evSignal      struct{}
	evFocusTimer  struct{}
	evLayoutTimer struct{}
	evPreview     struct{}
)

// Daemon is the running editor.
type Daemon struct {
	Exe, Sock string // for new panes' command
	// Extra args to every new pane's command, after Exe.
	PaneArgs []string
	// Preview is shown in the startup pane if Neovim hasn't drawn it
	// PreviewDelay after the start; nil for none.
	Preview *Preview

	events  chan event
	grids   *screen.Grids
	panes   Panes
	rex     *rex.Session
	nv      nvimState
	changes *changes
	stopped StopReason
}

func New(exe, sock string, paneArgs []string, nc *nvim.Client, rs *rex.Session) *Daemon {
	return &Daemon{
		Exe: exe, Sock: sock, PaneArgs: paneArgs,
		events:  make(chan event, 256),
		grids:   screen.NewGrids(),
		rex:     rs,
		nv:      nvimState{c: nc, uiFocused: true},
		changes: newChanges(),
	}
}

func (d *Daemon) send(e event) { d.events <- e }

// Run adds the launcher's pane and runs until Neovim exits or the last pane goes.
func (d *Daemon) Run(launch panehelper.Hello, block string, l *net.UnixListener, sigs <-chan os.Signal) (StopReason, error) {
	s, err := newSurface(launch.Conn, launch.TTY)
	if err != nil {
		return 0, err
	}
	p := d.panes.add(&Pane{id: startupPaneID, block: block, surface: s})
	d.watch(p)
	defer d.shutdown()

	go func() { // Neovim
		buf := make([]byte, 64*1024)
		r := d.nv.c.Reader()
		for {
			n, err := r.Read(buf)
			if n > 0 {
				d.send(evNvim{slices.Clone(buf[:n])})
			}
			if err != nil {
				d.send(evNvimEOF{})
				return
			}
		}
	}()
	go func() { // Rex
		for {
			ok, _ := d.rex.Read()
			d.send(evRex{ok})
			if !ok {
				return
			}
		}
	}()
	go func() { // helpers of new panes
		for {
			h, err := panehelper.AcceptHello(l)
			if err != nil {
				if ne, ok := err.(net.Error); ok && !ne.Timeout() {
					return
				}
				if _, ok := err.(*net.OpError); ok {
					return
				}
				continue
			}
			d.send(evAccept{h})
		}
	}()
	go func() {
		for range sigs {
			d.send(evSignal{})
		}
	}()
	if d.Preview != nil {
		t := time.AfterFunc(PreviewDelay, func() {
			select {
			case d.events <- evPreview{}:
			default: // busy: Neovim is drawing anyway
			}
		})
		defer t.Stop()
	}

	v := s.view
	if err := d.nv.c.UIAttach(max(v.Cols, 1), max(v.Rows, 1), nvim.UIOptions{ExtTermcolors: true, ExtMultigrid: true, StdoutTTY: true}); err != nil {
		return 0, err
	}
	d.nv.cell = [2]uint16{s.cellW, s.cellH}
	if d.nv.channelReq, err = d.nv.c.AskChannel(); err != nil {
		return 0, err
	}
	if err := d.nv.c.Flush(); err != nil {
		return 0, err
	}
	if err := d.rex.Attach(); err != nil {
		log.Printf("rexmod editor: attach: %v", err)
	}

	for d.stopped == running {
		e := <-d.events
		if err := d.handle(e); err != nil {
			log.Printf("rexmod editor: %v", err)
		}
		// drain what's queued before telling Neovim about focus
		for d.stopped == running && len(d.events) > 0 {
			if err := d.handle(<-d.events); err != nil {
				log.Printf("rexmod editor: %v", err)
			}
		}
		if d.stopped == running {
			if err := d.sendFocus(false); err != nil {
				log.Printf("rexmod editor: %v", err)
			}
		}
	}
	return d.stopped, nil
}

// previewDue: the startup pane is there and Neovim hasn't drawn in it.
func previewDue(p *Pane) bool { return p != nil && p.surface != nil && !p.framed }

// showPreview draws the preview in the startup pane, unless Neovim has
// drawn there already.
func (d *Daemon) showPreview() error {
	p := d.panes.byID(startupPaneID)
	if !previewDue(p) {
		return nil
	}
	s := p.surface
	b := d.Preview.Render(int(s.view.Cols), int(s.view.Rows))
	if b == nil {
		return nil
	}
	if _, err := s.term.Write(b); err != nil {
		return err
	}
	p.previewing = true
	return s.term.Flush()
}

func (d *Daemon) shutdown() {
	for _, p := range slices.Clone(d.panes.list) {
		if p.closing == notClosing {
			p.closing = shutdown
		}
		d.panes.remove(p)
	}
}

// watch reads a pane's terminal and helper socket in goroutines.
func (d *Daemon) watch(p *Pane) {
	s, id := p.surface, p.id
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := s.file.Read(buf)
			if n > 0 {
				d.send(evInput{id: id, data: slices.Clone(buf[:n])})
			}
			if err != nil {
				d.send(evInput{id: id, eof: true})
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := s.helper.Read(buf)
			if n > 0 {
				d.send(evHelper{id: id, data: slices.Clone(buf[:n])})
			}
			if err != nil {
				d.send(evHelper{id: id, eof: true})
				return
			}
		}
	}()
}

func (d *Daemon) stop(r StopReason) { d.stopped = r }

func (d *Daemon) handle(e event) error {
	switch e := e.(type) {
	case evSignal:
		d.stop(Signal)
	case evPreview:
		return d.showPreview()
	case evNvim:
		return d.onNvim(e.data)
	case evNvimEOF:
		d.stop(NvimExited)
	case evRex:
		return d.onRex()
	case evAccept:
		return d.accept(e.h)
	case evInput:
		p := d.panes.byID(e.id)
		if p == nil || p.surface == nil {
			return nil
		}
		if e.eof {
			return d.paneGone(p)
		}
		return d.onInput(p, e.data)
	case evHelper:
		p := d.panes.byID(e.id)
		if p == nil || p.surface == nil {
			return nil
		}
		if e.eof {
			return d.paneGone(p)
		}
		if slices.Contains(e.data, panehelper.MsgResized) {
			return d.handleResize(p)
		}
	case evFocusTimer:
		return d.sendFocus(true)
	case evLayoutTimer:
		return d.layoutWaitOver()
	}
	return nil
}

func (d *Daemon) after(dur time.Duration, e event) {
	time.AfterFunc(dur, func() { d.send(e) })
}

// sendFocus tells Neovim whether any pane has focus; losing it waits a moment.
func (d *Daemon) sendFocus(waited bool) error {
	known, any := false, false
	for _, p := range d.panes.list {
		if p.surface != nil && p.surface.fwd.Focused != nil {
			known = true
			any = any || *p.surface.fwd.Focused
		}
	}
	if !known || any == d.nv.uiFocused {
		return nil
	}
	if !any && !waited {
		d.after(focusOutDelay, evFocusTimer{})
		return nil
	}
	d.nv.uiFocused = any
	if _, err := d.nv.c.Request("nvim_ui_set_focus", any); err != nil {
		return err
	}
	return d.nv.c.Flush()
}

// panes

func (d *Daemon) accept(h panehelper.Hello) error {
	var p *Pane
	if h.HasID {
		p = d.panes.byID(h.ID)
	}
	if p == nil || p.surface != nil {
		h.Close()
		return nil
	}
	s, err := newSurface(h.Conn, h.TTY)
	if err != nil {
		h.Conn.Close()
		return err
	}
	p.surface = s
	d.watch(p)
	if p.awaiting {
		if s.view.Cols != rexDefaultSize[0] || s.view.Rows != rexDefaultSize[1] {
			p.awaiting = false
		} else {
			d.after(layoutWait, evLayoutTimer{})
		}
	}
	if err := d.sizeWindow(p); err != nil {
		return err
	}
	if p.layer != nil {
		if id, ok, err := d.followLayer(p); err != nil {
			return err
		} else if ok {
			p.layer.fit, p.layer.fitReq = fitAsked, id
		}
	}
	if err := d.nv.c.Flush(); err != nil {
		return err
	}
	d.draw()
	return nil
}

func (d *Daemon) layoutWaitOver() error {
	any := false
	for _, p := range d.panes.list {
		if p.awaiting && p.surface != nil {
			p.awaiting = false
			any = true
			if err := d.sizeWindow(p); err != nil {
				return err
			}
		}
	}
	if !any {
		return nil
	}
	if err := d.nv.c.Flush(); err != nil {
		return err
	}
	d.draw()
	return nil
}

func (d *Daemon) handleResize(p *Pane) error {
	if !p.surface.readSize() {
		return d.paneGone(p)
	}
	p.awaiting = false
	if err := d.sizeWindow(p); err != nil {
		return err
	}
	if _, _, err := d.followLayer(p); err != nil {
		return err
	}
	if p == d.focusedPane() {
		if err := d.nv.fitUITo(p); err != nil {
			return err
		}
	}
	if err := d.nv.c.Flush(); err != nil {
		return err
	}
	d.draw()
	return nil
}

func (d *Daemon) onInput(p *Pane, b []byte) error {
	if trace {
		log.Printf("input pane %d: %q", p.id, b)
	}
	if p.win != 0 && p.win != d.nv.currentWin && !d.rex.HasFocused(p.block) && !term.OnlyFocusReports(b) {
		// typed into a pane Rex hasn't focused, as `rex send` can
		if err := d.makeCurrent(p); err != nil {
			return err
		}
		_ = d.rex.Focus(p.block)
	}
	if p.layer == nil {
		p.surface.fwd.Grid = d.gridOfPane(p)
	}
	_, err := p.surface.fwd.Feed(d.nv.c, b)
	return err
}

func (d *Daemon) paneCommand(id uint32) []string {
	cmd := append([]string{d.Exe}, d.PaneArgs...)
	return append(cmd, d.Sock, strconv.FormatUint(uint64(id), 10))
}

func (d *Daemon) newPane(win int64) error {
	if d.rex.State() == rex.Closed {
		return nil // no Rex: the window stays in Neovim
	}
	sp := d.changes.splitOf(win)
	anchor := ""
	if f := d.splitFrom(sp); f != nil {
		anchor = f.block
	}
	if anchor == "" {
		for _, q := range d.panes.list {
			if q.layer == nil && q.block != "" {
				anchor = q.block
				break
			}
		}
	}
	if anchor == "" {
		return nil // next flush
	}
	size := d.newPaneSize(win)
	p := d.panes.addForWindow(win)
	p.awaiting = true
	delete(d.changes.splits, win)
	dir, side := "vertical", "after"
	if sp.SideBySide {
		dir = "horizontal"
	}
	if sp.Before {
		side = "before"
	}
	return d.rex.NewSplit(p.id, rex.SplitSpec{AnchorID: anchor, Direction: dir, Side: side, Layout: map[string]any{
		"block": map[string]any{
			"flavor": "com.superlogical.terminal.shell",
			"label":  Label,
			"options": map[string]any{
				"shell":   "none",
				"command": d.paneCommand(p.id),
				"size":    map[string]any{"columns": size[0], "rows": size[1]},
			},
		},
	}})
}

// paneGone: Rex closed the pane. Close its window, or end if it was the last.
func (d *Daemon) paneGone(p *Pane) error {
	p.closing = rexPaneClosed
	if p.layer != nil {
		d.panes.remove(p)
		for _, top := range p.layer.group.TopGrids() {
			if w, ok := d.grids.WinOn(top); ok {
				if err := d.nv.c.CloseWindow(w.Win); err != nil {
					return err
				}
			}
		}
		return d.nv.c.Flush()
	}
	others := false
	for _, q := range d.panes.list {
		if q != p && q.closing != requestFailed && q.layer == nil {
			others = true
		}
	}
	if !others {
		d.stop(PaneGone)
		return nil
	}
	win := p.win
	d.panes.remove(p)
	if win == 0 {
		return nil
	}
	d.changes.closing[win] = true
	if err := d.nv.c.CloseWindow(win); err != nil {
		return err
	}
	return d.nv.c.Flush()
}

// Neovim

func (d *Daemon) onNvim(b []byte) error {
	d.nv.c.Feed(b)
	for {
		m, err := d.nv.c.Next()
		if err != nil {
			return err
		}
		if m == nil {
			return nil
		}
		switch m := m.(type) {
		case nvim.Notification:
			if m.Method == "redraw" {
				d.uiSend(m.Params)
				flushed, err := d.grids.ApplyRedraw(m.Params)
				if err != nil {
					log.Printf("rexmod editor: redraw: %v", err)
				}
				if flushed {
					if err := d.reconcile(); err != nil {
						return err
					}
					d.draw()
				}
				continue
			}
			n, ok, err := nvim.ParseNote(m.Method, m.Params)
			if err != nil || !ok {
				continue
			}
			if err := d.onNote(n); err != nil {
				return err
			}
		case nvim.Request:
			_ = d.nv.c.RespondError(m.ID, "rexmod editor serves no requests")
		case nvim.Response:
			if err := d.onResponse(m); err != nil {
				return err
			}
		}
	}
}

// uiSend writes nvim_ui_send data, such as kitty graphics from image
// plugins, to every pane's terminal: an image sent once can then show in
// any of them, placed by the placeholder cells drawn with the grids.
func (d *Daemon) uiSend(params []any) {
	for _, ev := range params {
		a, ok := ev.([]any)
		if !ok || len(a) < 2 || a[0] != "ui_send" {
			continue
		}
		for _, args := range a[1:] {
			t, ok := args.([]any)
			if !ok || len(t) < 1 {
				continue
			}
			var data []byte
			switch v := t[0].(type) {
			case string:
				data = []byte(v)
			case []byte:
				data = v
			}
			for _, p := range d.panes.list {
				if p.surface != nil && p.closing == notClosing {
					_, _ = p.surface.term.Write(data)
				}
			}
		}
	}
}

func (d *Daemon) onNote(n nvim.Note) error {
	switch n.Kind {
	case nvim.NoteCurrent:
		return d.onCurrent(n.Win)
	case nvim.NoteFloat:
		d.nv.currentFloat = n.Win
		if p := d.layerOfWin(n.Win); p != nil {
			return d.rex.Focus(p.block)
		}
	case nvim.NoteEntered:
		d.nv.noteEntered(n.Win)
		if _, ok := d.grids.GridOf(n.Win); !d.nv.ready || !ok || d.layerOfWin(n.Win) != nil {
			return nil
		}
		if err := d.reconcileLayers(); err != nil {
			return err
		}
		if err := d.nv.c.Flush(); err != nil {
			return err
		}
		d.draw()
	case nvim.NoteReopen:
		if d.panes.byWin(n.Win) != nil {
			d.changes.reopening[n.Win] = true
		}
	case nvim.NoteSplit:
		if d.panes.byWin(n.Split.Win) == nil {
			d.changes.splits[n.Split.Win] = n.Split
		}
	}
	return nil
}

func (d *Daemon) onResponse(r nvim.Response) error {
	if r.ID == d.nv.currentReq {
		d.nv.currentReq = 0
	}
	showNow := false
	for _, p := range d.panes.list {
		if l := p.layer; l != nil && l.fit == fitAsked && l.fitReq == r.ID {
			moved, _ := r.Result.(bool)
			moved = moved && r.Err == nil
			if moved {
				l.fit = fitAnswered
			} else {
				l.fit = fitNone
			}
			showNow = !moved
		}
	}
	if showNow {
		d.draw()
	}
	if r.Err != nil {
		log.Printf("neovim error: %s", r.ErrMessage())
		return nil
	}
	switch r.ID {
	case d.nv.channelReq:
		ch, err := nvim.ParseChannel(r.Result)
		if err != nil {
			return err
		}
		size := [2]uint32{1, 1}
		if p := d.panes.byID(startupPaneID); p != nil && p.surface != nil {
			size = p.surface.windowSize()
		}
		if d.nv.cell[0] > 0 {
			// snacks.image can't ask a headless Neovim's terminal its cell size
			_ = d.nv.c.Notify("nvim_set_var", "rexmod_cell", []any{int64(d.nv.cell[0]), int64(d.nv.cell[1])})
		}
		if d.nv.setupReq, err = d.nv.c.Setup(ch, size[0], size[1]); err != nil {
			return err
		}
		return d.nv.c.Flush()
	case d.nv.setupReq:
		wins, err := nvim.ParseSetup(r.Result)
		if err != nil {
			return err
		}
		if p := d.panes.byID(startupPaneID); p != nil {
			p.win = wins[0]
		}
		d.nv.ready = true
		if err := d.reconcile(); err != nil {
			return err
		}
		d.draw()
	}
	return nil
}

// onCurrent: Neovim's current window changed; focus its pane in Rex.
func (d *Daemon) onCurrent(win int64) error {
	if d.nv.currentReq != 0 {
		return nil // sent before our own change ran
	}
	d.nv.currentFloat = 0
	d.nv.currentWin = win
	p := d.panes.byWin(win)
	if p == nil {
		return nil
	}
	if err := d.nv.fitUITo(p); err != nil {
		return err
	}
	d.draw()
	return d.rex.Focus(p.block)
}

// reconcile matches panes to windows: new external windows get panes,
// ordinary ones are made external, and panes whose windows closed hang up.
func (d *Daemon) reconcile() error {
	if !d.nv.ready {
		return nil
	}
	for _, p := range d.panes.list {
		if p.layer != nil && p.layer.fit == fitAnswered {
			p.layer.fit = fitNone
		}
	}
	for _, p := range slices.Clone(d.panes.list) {
		if p.win == 0 {
			continue
		}
		if _, ok := d.grids.GridOf(p.win); ok {
			continue
		}
		if d.changes.takeReopening(p.win) {
			p.win = 0
			continue
		}
		p.closing = windowClosed
		if p.surface == nil && p.block != "" {
			_ = d.rex.CloseBlock(p.block)
		}
		d.panes.remove(p)
	}
	if len(d.panes.list) == 0 {
		d.stop(PaneGone)
		return nil
	}
	d.changes.forgetGone(d.grids)

	for _, g := range d.grids.WinGrids() {
		w, ok := d.grids.WinOn(g)
		if !ok {
			continue
		}
		switch w.Kind {
		case screen.WinOrdinary:
			if !d.changes.shouldAdopt(w.Win) {
				continue
			}
			var size [2]uint32
			if p := d.panes.byWin(w.Win); p != nil {
				size = windowSize(p)
			} else {
				ps := d.newPaneSize(w.Win)
				size = [2]uint32{ps[0], max(ps[1]-1, 1)}
			}
			if err := d.nv.c.Adopt(w.Win, size[0], size[1]); err != nil {
				return err
			}
		case screen.WinExternal:
			delete(d.changes.asked, w.Win)
			if d.panes.byWin(w.Win) != nil || d.changes.closing[w.Win] {
				continue
			}
			if p := d.waitingPane(); p != nil {
				p.win = w.Win
			} else if err := d.newPane(w.Win); err != nil {
				return err
			}
		}
	}
	for _, p := range d.panes.list {
		if err := d.sizeWindow(p); err != nil {
			return err
		}
	}
	if err := d.reconcileLayers(); err != nil {
		return err
	}
	return d.nv.c.Flush()
}

// waitingPane is a pane kept for a reopened window.
func (d *Daemon) waitingPane() *Pane {
	for _, p := range d.panes.list {
		if p.layer == nil && p.win == 0 && p.surface != nil {
			return p
		}
	}
	return nil
}

// newPaneSize guesses a new split's terminal size: half the pane it splits.
func (d *Daemon) newPaneSize(win int64) [2]uint32 {
	sp := d.changes.splitOf(win)
	p := d.splitFrom(sp)
	if p == nil || p.surface == nil {
		return rexDefaultSize
	}
	cols, rows := max(p.surface.view.Cols, 1), max(p.surface.view.Rows, 2)
	if sp.SideBySide {
		return [2]uint32{max(cols/2, 1), rows}
	}
	return [2]uint32{cols, max(rows/2, 2)}
}

func (d *Daemon) splitFrom(sp nvim.Split) *Pane {
	if sp.From != 0 {
		if p := d.panes.byWin(sp.From); p != nil {
			return p
		}
	}
	return d.focusedPane()
}

// Rex

func (d *Daemon) onRex() error {
	for {
		u, err := d.rex.Next()
		if err != nil {
			return err
		}
		if u == nil {
			return nil
		}
		switch u := u.(type) {
		case rex.SplitMade:
			p := d.panes.byID(u.Pane)
			if p == nil {
				_ = d.rex.CloseBlock(u.Block)
				continue
			}
			p.block = u.Block
			_ = d.rex.Disown(u.Block)
			if p.win == d.nv.currentWin {
				_ = d.rex.Focus(p.block)
			}
		case rex.SplitFailed:
			if p := d.panes.byID(u.Pane); p != nil {
				p.closing = requestFailed
			}
		case rex.LayerFailed:
			if trace {
				log.Printf("layer failed for pane %d", u.Pane)
			}
			if p := d.panes.byID(u.Pane); p != nil {
				p.closing = requestFailed
			}
		case rex.LayerMade:
			p := d.panes.byID(u.Pane)
			if p == nil {
				if u.Layer != "" {
					_ = d.rex.CloseLayer(u.Layer)
				} else {
					_ = d.rex.CloseBlock(u.Block)
				}
				continue
			}
			p.block = u.Block
			_ = d.rex.Disown(u.Block)
			p.layer.id = u.Layer
			if d.nv.currentFloat != 0 && d.layerOfWin(d.nv.currentFloat) == p {
				_ = d.rex.Focus(p.block)
			}
		case rex.Layout:
			d.readLayout(u.Windows)
			if f := d.focusedPane(); f != nil {
				if err := d.nv.fitUITo(f); err != nil {
					return err
				}
			}
			if err := d.nv.c.Flush(); err != nil {
				return err
			}
			d.draw()
		case rex.FocusMoved:
			if err := d.onRexFocus(u.Block); err != nil {
				return err
			}
		}
	}
}

// onRexFocus: the user focused one of our panes in Rex; make its window current.
func (d *Daemon) onRexFocus(block string) error {
	p := d.panes.byBlock(block)
	if p == nil {
		return nil
	}
	if p.layer != nil {
		if d.nv.currentFloat != 0 && d.layerOfWin(d.nv.currentFloat) == p {
			return nil
		}
		win := d.focusableIn(&p.layer.group)
		if win == 0 {
			return nil
		}
		d.nv.currentFloat = win
		if _, err := d.nv.c.SetCurrentWin(win); err != nil {
			return err
		}
		return d.nv.c.Flush()
	}
	if p.win == 0 || (p.win == d.nv.currentWin && d.nv.currentFloat == 0) {
		return nil
	}
	d.nv.currentFloat = 0
	if err := d.makeCurrent(p); err != nil {
		return err
	}
	if err := d.nv.c.Flush(); err != nil {
		return err
	}
	d.draw()
	return nil
}

func get(v any, k string) any {
	if m, ok := v.(map[string]any); ok {
		return m[k]
	}
	return nil
}

func toF(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

func rect4(v any) (*[4]float64, bool) {
	a, ok := v.([]any)
	if !ok || len(a) != 4 {
		return nil, false
	}
	var r [4]float64
	for i, x := range a {
		f, ok := toF(x)
		if !ok {
			return nil, false
		}
		r[i] = f
	}
	return &r, true
}

// readLayout remembers which Rex window holds our panes, and where each sits.
func (d *Daemon) readLayout(wins []any) {
	for _, w := range wins {
		layers, ok := get(w, "layers").([]any)
		if !ok {
			continue
		}
		found := false
		for _, layer := range layers {
			if k, _ := get(layer, "kind").(string); k != "tiled" {
				d.readLayerBounds(layer)
				continue
			}
			if l := get(layer, "layout"); l != nil {
				found = d.readRects(l) || found
			}
		}
		if id, _ := get(w, "window_id").(string); found && id != "" {
			d.rex.SetWindow(id)
		}
	}
}

func (d *Daemon) readLayerBounds(layer any) {
	b, ok := rect4(get(layer, "bounds"))
	if !ok {
		return
	}
	blocks, ok := get(layer, "blocks").([]any)
	if !ok || len(blocks) == 0 {
		return
	}
	id, _ := get(blocks[0], "block_id").(string)
	p := d.panes.byBlock(id)
	if p == nil || p.layer == nil || d.sameBounds(*b, p.layer.bounds) {
		return
	}
	p.layer.bounds = *b
	_, _, _ = d.followLayer(p)
}

func (d *Daemon) readRects(node any) bool {
	found := false
	switch n := node.(type) {
	case map[string]any:
		if b, ok := n["block_id"].(string); ok {
			if p := d.panes.byBlock(b); p != nil {
				if r, ok := rect4(n["rect"]); ok {
					p.rect = r
					found = true
				}
			}
		}
		for _, v := range n {
			found = d.readRects(v) || found
		}
	case []any:
		for _, x := range n {
			found = d.readRects(x) || found
		}
	}
	return found
}

// floating panes

// reconcileLayers matches floating panes to groups of floats. A floating
// pane's size is Rex's: its floats follow it, never the other way.
func (d *Daemon) reconcileLayers() error {
	var groups []screen.Group
	if d.rex.State() != rex.Closed {
		groups = screen.FindGroups(d.grids, d.nv.entered)
	}
	for _, p := range slices.Clone(d.panes.list) {
		if p.layer == nil {
			continue
		}
		i := slices.IndexFunc(groups, func(g screen.Group) bool { return g.Key == p.layer.group.Key })
		if i < 0 {
			if p.layer.id != "" {
				_ = d.rex.CloseLayer(p.layer.id)
			}
			if p.closing == notClosing {
				p.closing = windowClosed
			}
			d.panes.remove(p)
			continue
		}
		p.layer.group = groups[i]
	}
	if trace {
		log.Printf("groups %+v entered %v", groups, d.nv.entered)
	}
	for _, g := range groups {
		if d.panes.byLayerKey(g.Key) == nil {
			if err := d.newLayer(g); err != nil {
				return err
			}
		}
	}
	return nil
}

// layerBounds is a floating pane's bounds around g, as fractions of the
// window, leaving room for Rex's frame.
func (d *Daemon) layerBounds(g screen.Group) (*[4]float64, bool) {
	s := d.grids.Get(1)
	if s == nil || s.Width == 0 || s.Height == 0 {
		return nil, false
	}
	sw, sh := float64(s.Width), float64(s.Height)
	w := math.Min(float64(int64(g.Width)+int64(layerFrame[0]))/sw, 1)
	h := math.Min(float64(int64(g.Height)+int64(layerFrame[1]))/sh, 1)
	x := clamp(float64(g.Col-floorDiv(layerFrame[0], 2))/sw, 0, 1-w)
	y := clamp(float64(g.Row-floorDiv(layerFrame[1], 2))/sh, 0, 1-h)
	return &[4]float64{x, y, w, h}, true
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(v, hi)) }
func floorDiv(a, b int32) int32 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func (d *Daemon) newLayer(g screen.Group) error {
	b, ok := d.layerBounds(g)
	if !ok {
		return nil
	}
	p := d.panes.addForGroup(g)
	p.layer.bounds = *b
	if trace {
		log.Printf("new layer pane %d bounds %v window %q", p.id, *b, d.rex.Window())
	}
	return d.rex.NewLayer(p.id, *b, map[string]any{"block": map[string]any{
		"flavor":  "com.superlogical.terminal.shell",
		"label":   Label,
		"options": map[string]any{"shell": "none", "command": d.paneCommand(p.id)},
	}})
}

// sameBounds: within half a cell of each other on grid 1.
func (d *Daemon) sameBounds(a, b [4]float64) bool {
	s := d.grids.Get(1)
	if s == nil {
		return true
	}
	half := [2]float64{0.5 / float64(max(s.Width, 1)), 0.5 / float64(max(s.Height, 1))}
	for i := range a {
		if math.Abs(a[i]-b[i]) >= half[i%2] {
			return false
		}
	}
	return true
}

// followLayer fits a floating pane's floats to where it is and its size.
func (d *Daemon) followLayer(p *Pane) (uint32, bool, error) {
	l, s, scr := p.layer, p.surface, d.grids.Get(1)
	if l == nil || s == nil || scr == nil {
		return 0, false, nil
	}
	row := int32(math.Round(l.bounds[1]*float64(scr.Height))) + floorDiv(layerFrame[1], 2)
	col := int32(math.Round(l.bounds[0]*float64(scr.Width))) + floorDiv(layerFrame[0], 2)
	g := l.group
	if row == g.Row && col == g.Col && s.view.Cols == g.Width && s.view.Rows == g.Height {
		return 0, false, nil
	}
	var wins []int64
	for _, top := range g.TopGrids() {
		if w, ok := d.grids.WinOn(top); ok {
			wins = append(wins, w.Win)
		}
	}
	if len(wins) == 0 {
		return 0, false, nil
	}
	id, err := d.nv.c.PlaceGroup(wins, row, col, s.view.Cols, s.view.Rows, [4]int64{int64(g.Row), int64(g.Col), int64(g.Width), int64(g.Height)})
	return id, err == nil, err
}

func (d *Daemon) layerOfWin(win int64) *Pane {
	grid, ok := d.grids.GridOf(win)
	if !ok {
		return nil
	}
	pl, ok := d.grids.Place(grid)
	if !ok || pl.Top == 0 {
		return nil
	}
	for _, p := range d.panes.list {
		if p.layer != nil && p.layer.group.HasTop(pl.Top) {
			return p
		}
	}
	return nil
}

// focusableIn is the float in g that takes focus: a picker's input, stacked above its box.
func (d *Daemon) focusableIn(g *screen.Group) int64 {
	var best *screen.Win
	for _, id := range d.grids.WinGrids() {
		w, ok := d.grids.WinOn(id)
		if !ok || w.Kind != screen.WinFloat || !w.Float.Focusable {
			continue
		}
		pl, ok := d.grids.Place(id)
		if !ok || pl.Top == 0 || !g.HasTop(pl.Top) {
			continue
		}
		if best == nil || w.Float.ZIndex > best.Float.ZIndex {
			wc := w
			best = &wc
		}
	}
	if best == nil {
		return 0
	}
	return best.Win
}

// shared

// draw draws every pane that changed.
func (d *Daemon) draw() {
	focused := d.focusedPane()
	var skip []uint32
	for _, p := range d.panes.list {
		if p.layerHides() {
			skip = append(skip, p.layer.group.TopGrids()...)
		}
	}
	for _, p := range slices.Clone(d.panes.list) {
		s := p.surface
		if s == nil || (p.layer != nil && !p.layerShown()) {
			continue
		}
		// No window of Neovim's in it yet: its frame would be a blank one,
		// which would wipe the preview; the first real frame draws it all.
		if p.previewing && p.layer == nil && d.gridOfPane(p) == 0 {
			continue
		}
		var drawn bool
		var err error
		if p.layer != nil {
			members := screen.GroupMembers(d.grids, &p.layer.group)
			s.fwd.Grid = 1
			if len(members) > 0 {
				s.fwd.Grid = members[0].Grid
			}
			drawn, err = s.view.FrameFloats(d.grids, members, [2]int32{p.layer.group.Row, p.layer.group.Col}, s.term)
		} else {
			drawn, err = s.view.Frame(d.grids, d.gridOfPane(p), screen.Opts{Focused: p == focused, Origin: d.paneOrigin(p), Skip: skip}, s.term)
		}
		if trace && p.layer == nil {
			g := d.gridOfPane(p)
			log.Printf("draw pane %d grid %d drawn=%v err=%v floats=%+v over=%+v", p.id, g, drawn, err, s.view.DrawnFloats(), d.grids.FloatsOver(g, false, skip))
		}
		var ts []nvim.Target
		for _, r := range s.view.DrawnFloats() {
			ts = append(ts, nvim.Target{Grid: r.Grid, Row: r.Row, Col: r.Col, Width: r.Width, Height: r.Height})
		}
		s.fwd.SetTargets(ts)
		if err == nil && drawn {
			err = s.term.Flush()
			if err == nil && (p.layer != nil || d.gridOfPane(p) != 0) {
				p.framed, p.previewing = true, false
			}
		}
		if err != nil {
			_ = d.paneGone(p)
		}
	}
	// drawn into one pane at most; done with them
	if d.grids.MsgGrid != 0 {
		if m := d.grids.Get(d.grids.MsgGrid); m != nil {
			m.ClearDirty()
			m.ClearScrolls()
		}
	}
	for _, id := range d.grids.WinGrids() {
		if w, ok := d.grids.WinOn(id); ok && w.Kind == screen.WinFloat {
			if f := d.grids.Get(id); f != nil {
				f.ClearDirty()
				f.ClearScrolls()
			}
		}
	}
}

// paneOrigin is where p's top left sits on grid 1: row, col.
func (d *Daemon) paneOrigin(p *Pane) [2]int32 {
	s := d.grids.Get(1)
	if p.rect == nil || s == nil {
		return [2]int32{}
	}
	return [2]int32{int32(math.Round(p.rect[1] * float64(s.Height))), int32(math.Round(p.rect[0] * float64(s.Width)))}
}

// sizeWindow asks Neovim for the window to fit the pane, unless it already was.
func (d *Daemon) sizeWindow(p *Pane) error {
	if p.surface == nil || p.awaiting || p.win == 0 {
		return nil
	}
	grid, ok := d.grids.GridOf(p.win)
	if !ok {
		return nil
	}
	size := p.surface.windowSize()
	if p.lastAsk.grid == grid && p.lastAsk.cols == size[0] && p.lastAsk.rows == size[1] {
		return nil
	}
	p.lastAsk.grid, p.lastAsk.cols, p.lastAsk.rows = grid, size[0], size[1]
	return d.nv.c.SizeWindow(p.win, size[0], size[1])
}

func (d *Daemon) makeCurrent(p *Pane) error {
	d.nv.currentWin = p.win
	id, err := d.nv.c.SetCurrentWin(p.win)
	if err != nil {
		return err
	}
	d.nv.currentReq = id
	return d.nv.fitUITo(p)
}

func (d *Daemon) focusedPane() *Pane {
	if d.nv.currentWin == 0 {
		return d.panes.byID(startupPaneID)
	}
	return d.panes.byWin(d.nv.currentWin)
}

func (d *Daemon) gridOfPane(p *Pane) uint32 {
	if p.win == 0 {
		return 0
	}
	g, _ := d.grids.GridOf(p.win)
	return g
}

func windowSize(p *Pane) [2]uint32 {
	if p.surface == nil {
		return [2]uint32{1, 1}
	}
	return p.surface.windowSize()
}
