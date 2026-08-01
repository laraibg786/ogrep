package output

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// SummaryMode selects how Terminal.WriteFileSummary renders a matching
// file's summary line, driven by the -l/--files-with-matches and
// -c/--count flags. The default mode renders nothing because per-match
// output (WriteMatch) already showed the file's matches.
type SummaryMode int

const (
	// SummaryModeOff renders nothing; used for the default output mode,
	// where each match was already printed individually.
	SummaryModeOff SummaryMode = iota
	// SummaryModePathOnly renders just the file path, for -l/--files-with-matches.
	SummaryModePathOnly
	// SummaryModeCount renders "path:count", for -c/--count.
	SummaryModeCount
)

// Terminal is an rg-style OutputSink: the file path in one color, the
// human-readable location, and the matched span(s) highlighted within
// the surrounding text. It is safe for concurrent use from multiple
// goroutines — the orchestrator's per-file workers may call WriteMatch
// concurrently, and Terminal serializes writes with a mutex so results
// for a given call are never interleaved mid-line.
type Terminal struct {
	mu      sync.Mutex
	w       *bufio.Writer
	color   bool
	isTTY   bool
	summary SummaryMode
}

// NewTerminal builds a Terminal sink writing to w. tty is the *os.File
// actually behind w (typically os.Stdout); pass nil if w isn't backed
// by a real file (e.g. in tests capturing to a buffer), in which case
// ColorAuto behaves as "never colorize". summary controls how
// WriteFileSummary renders (see SummaryMode); pass SummaryModeOff for
// the default per-match output mode.
//
// isTTY is tracked separately from color: color can be forced on/off
// via mode regardless of what's actually backing w (e.g. `--color=always
// | less -R`), but hyperlinks and the multi-line-match rendering below
// are only ever appropriate for a real terminal — see WriteMatch.
func NewTerminal(w io.Writer, mode ColorMode, tty *os.File, summary SummaryMode) *Terminal {
	return &Terminal{
		w:       bufio.NewWriter(w),
		color:   shouldColor(mode, tty),
		isTTY:   isTerminal(tty),
		summary: summary,
	}
}

// hyperlink wraps text in an OSC 8 terminal hyperlink escape sequence
// pointing at uri. Terminals that don't support OSC 8 render the escape
// codes as nothing and just show text.
func hyperlink(uri, text string) string {
	return fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", uri, text)
}

// escapeLocationControls rewrites every C0 control byte (0x00-0x1F) and
// DEL (0x7F) that a POSIX filename may legally contain into a visible
// backslash escape wherever a location is displayed — \n, \r, \t get
// their familiar short forms, everything else (notably ESC and BEL)
// gets \xNN. Left raw, a literal newline would split "one line per
// match" across two lines, a tab would throw off the continuation-line
// padding math, and an ESC byte could inject an arbitrary terminal
// escape sequence (e.g. another OSC 8 hyperlink) from a crafted
// filename. Only the location is escaped this way — matched text is
// left untouched, since real tabs/newlines in document content are
// legitimate and already handled by the padding logic.
func escapeLocationControls(s string) string {
	if !strings.ContainsFunc(s, isEscapedControl) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if isEscapedControl(r) {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func isEscapedControl(r rune) bool { return r < 0x20 || r == 0x7f }

// WriteMatch implements ports.OutputSink. It prints one grep-style line
// per match: "path:location text", with the location wrapped in an OSC 8
// hyperlink when the Location provides one. Text spanning multiple lines
// (e.g. a docx table cell) has its continuation lines padded to align
// under the first line's text.
func (t *Terminal) WriteMatch(m domain.Match) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	loc := escapeLocationControls(domain.LocationString(m.Path, m.Location))
	// RuneCountInString, not len: a byte count would under-pad any
	// non-ASCII path. This still isn't true terminal column width (wide
	// CJK glyphs and combining marks can still misalign), but the
	// project has no wcwidth-style dependency and that's a cosmetic gap
	// beyond what a rune count buys cheaply here.
	padding := strings.Repeat(" ", utf8.RuneCountInString(loc)+1)

	// OSC 8 hyperlinks are only meaningful -- and only safe -- when
	// something is actually going to render them as a terminal escape
	// sequence; piped into less/grep/a file without -R, the raw
	// \x1b]8;;...\x1b\ bytes would otherwise show up as literal noise
	// wrapped around the location. Gated on isTTY, not color: a forced
	// `--color=always` doesn't make hyperlinks any safer to emit into a
	// non-terminal, so it doesn't affect this decision either.
	display := loc
	if t.isTTY {
		if uri := m.Location.HyperlinkURI(m.Path, m.Spans); uri != "" {
			display = hyperlink(uri, loc)
		}
	}

	if t.color {
		fmt.Fprintf(t.w, "%s%s%s ", ansiPathColor, display, ansiReset)
	} else {
		fmt.Fprintf(t.w, "%s ", display)
	}

	t.writeHighlighted(m.Text, m.Spans, padding)
	t.w.WriteByte('\n')
	return t.w.Flush()
}

// writeHighlighted writes text with its matched spans colorized (when
// t.color is set), padding every continuation line (after an embedded
// newline) with padding so it aligns under the first line's text.
func (t *Terminal) writeHighlighted(text string, spans []domain.Span, padding string) {
	lines := strings.Split(text, "\n")

	lineStart := 0
	for lineIdx, line := range lines {
		if lineIdx > 0 {
			t.w.WriteString(padding)
		}
		t.writeHighlightedLine(line, lineStart, spans)
		lineStart += len(line) + 1
		if lineIdx < len(lines)-1 {
			t.w.WriteByte('\n')
		}
	}
}

// writeHighlightedLine writes one line of text (the [lineStart,
// lineStart+len(line)) slice of the original, unsplit text), colorizing
// whichever spans overlap it.
func (t *Terminal) writeHighlightedLine(line string, lineStart int, spans []domain.Span) {
	if !t.color || len(spans) == 0 {
		t.w.WriteString(line)
		return
	}
	lineEnd := lineStart + len(line)
	pos := 0
	for _, sp := range spans {
		if sp.End <= lineStart || sp.Start >= lineEnd || sp.End < sp.Start {
			continue
		}
		spanStart := max(sp.Start-lineStart, 0)
		spanEnd := min(sp.End-lineStart, len(line))
		if spanStart < pos {
			continue // defensively skip malformed/overlapping spans
		}

		t.w.WriteString(line[pos:spanStart])
		t.w.WriteString(ansiMatch)
		t.w.WriteString(line[spanStart:spanEnd])
		t.w.WriteString(ansiReset)
		pos = spanEnd
	}
	t.w.WriteString(line[pos:])
}

// WriteFileSummary implements ports.OutputSink. Its rendering depends on
// t.summary: the default mode (SummaryModeOff) renders nothing, since
// WriteMatch already printed every match for this file; SummaryModePathOnly
// (-l/--files-with-matches) prints just the path; SummaryModeCount
// (-c/--count) prints "path:count".
func (t *Terminal) WriteFileSummary(path string, matchCount int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch t.summary {
	case SummaryModePathOnly:
		if t.color {
			fmt.Fprintf(t.w, "%s%s%s\n", ansiPathColor, path, ansiReset)
		} else {
			fmt.Fprintf(t.w, "%s\n", path)
		}
	case SummaryModeCount:
		if t.color {
			fmt.Fprintf(t.w, "%s%s%s:%d\n", ansiPathColor, path, ansiReset, matchCount)
		} else {
			fmt.Fprintf(t.w, "%s:%d\n", path, matchCount)
		}
	default:
		return nil
	}
	return t.w.Flush()
}

// Flush implements ports.OutputSink.
func (t *Terminal) Flush() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.w.Flush()
}
