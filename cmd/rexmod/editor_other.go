//go:build !darwin && !linux

package main

import "errors"

// The editor (a headless Neovim behind Rex panes) needs macOS. Elsewhere,
// such as a remote host that runs rexmod for the panel, it isn't built.
var errEditorUnsupported = errors.New("the editor isn't supported on this platform")

func cmdEditor(args []string) error       { return errEditorUnsupported }
func cmdEditorPane(args []string) error   { return errEditorUnsupported }
func cmdEditorDaemon(args []string) error { return errEditorUnsupported }
