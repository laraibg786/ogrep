// Package output provides ports.OutputSink implementations: an
// rg-style colorized terminal sink and a JSON-lines sink for tool
// consumption.
package output

import (
	"os"

	"golang.org/x/term"
)

// ColorMode is the tri-state color setting shared by both the CLI flag
// and the XDG config file.
type ColorMode string

const (
	ColorAuto   ColorMode = "auto"
	ColorAlways ColorMode = "always"
	ColorNever  ColorMode = "never"
)

// isTerminal reports whether f looks like a real terminal, as opposed
// to a pipe, redirect, or nil (no backing file at all, e.g. a test
// writing into a buffer). A package-level var, not a plain func, so
// tests can substitute it to simulate a real terminal without needing
// to allocate an actual pty.
var isTerminal = func(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

// shouldColor resolves a ColorMode against whether f looks like a
// terminal.
func shouldColor(mode ColorMode, f *os.File) bool {
	switch mode {
	case ColorAlways:
		return true
	case ColorNever:
		return false
	default: // auto
		return isTerminal(f)
	}
}

// ANSI SGR codes used for rg-style highlighting.
const (
	ansiReset     = "\x1b[0m"
	ansiPathColor = "\x1b[1;35m" // bold magenta
	ansiLocColor  = "\x1b[32m"   // green
	ansiMatch     = "\x1b[1;31m" // bold red
)
