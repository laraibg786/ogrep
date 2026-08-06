package output

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

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

// escapeControls rewrites every C0 control byte (0x00-0x1F) and DEL
// (0x7F) that a POSIX filename may legally contain into a visible
// backslash escape wherever a location is displayed — \n, \r, \t get
// their familiar short forms, everything else (notably ESC and BEL)
// gets \xNN. Left raw, a literal newline would split "one line per
// match" across two lines, a tab would throw off the continuation-line
// padding math, and an ESC byte could inject an arbitrary terminal
// escape sequence (e.g. another OSC 8 hyperlink) from a crafted
// filename. Only the location is escaped this way — matched text is
// left untouched: domain.TextUnit.Text is guaranteed to never contain a
// newline (see its doc comment), so there is nothing here to flatten.
func escapeControls(s string) string {
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
// per match: "path:location text", with the location wrapped in an OSC
// 8 hyperlink when the Location provides one and we're writing to a
// real terminal, and matched spans colorized. m.Text is guaranteed to
// never contain an embedded newline (see domain.TextUnit's doc
// comment), so there's exactly one physical output line per match.
func (t *Terminal) WriteMatch(m domain.Match) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	loc := escapeControls(domain.LocationString(m.Path, m.Location))

	// OSC 8 hyperlinks are only meaningful -- and only safe -- when
	// something is actually going to render them as a terminal escape
	// sequence; piped into less/grep/a file without -R, the raw
	// \x1b]8;;...\x1b\ bytes would otherwise show up as literal noise
	// wrapped around the location. Gated on isTTY, not color: a forced
	// `--color=always` doesn't make hyperlinks any safer to emit into a
	// non-terminal, so it doesn't affect this decision either.
	// m.Path == domain.StdinPath has no real backing file to link to
	// (see domain.StdinPath's doc comment), so the hyperlink is skipped
	// entirely rather than building one that can never resolve.
	display := loc
	if t.isTTY && m.Path != domain.StdinPath {
		if uri := m.Location.HyperlinkURI(m.Path, m.Spans); uri != "" {
			display = hyperlink(uri, loc)
		}
	}

	if t.color {
		fmt.Fprintf(t.w, "%s%s%s ", ansiPathColor, display, ansiReset)
	} else {
		fmt.Fprintf(t.w, "%s ", display)
	}

	t.writeHighlighted(m.Text, m.Spans)
	t.w.WriteByte('\n')
	return t.w.Flush()
}

// writeHighlighted writes text with its matched spans colorized, when
// t.color is set.
func (t *Terminal) writeHighlighted(text string, spans []domain.Span) {
	if !t.color || len(spans) == 0 {
		t.w.WriteString(text)
		return
	}
	pos := 0
	for _, sp := range spans {
		if sp.End <= 0 || sp.Start >= len(text) || sp.End < sp.Start {
			continue
		}
		spanStart := max(sp.Start, 0)
		spanEnd := min(sp.End, len(text))
		if spanStart < pos {
			continue // defensively skip malformed/overlapping spans
		}

		t.w.WriteString(text[pos:spanStart])
		t.w.WriteString(ansiMatch)
		t.w.WriteString(text[spanStart:spanEnd])
		t.w.WriteString(ansiReset)
		pos = spanEnd
	}
	t.w.WriteString(text[pos:])
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
