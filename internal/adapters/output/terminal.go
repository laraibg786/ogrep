package output

import (
	"bufio"
	"fmt"
	"io"
	"os"
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
	summary SummaryMode
}

// NewTerminal builds a Terminal sink writing to w. tty is the *os.File
// actually behind w (typically os.Stdout); pass nil if w isn't backed
// by a real file (e.g. in tests capturing to a buffer), in which case
// ColorAuto behaves as "never colorize". summary controls how
// WriteFileSummary renders (see SummaryMode); pass SummaryModeOff for
// the default per-match output mode.
func NewTerminal(w io.Writer, mode ColorMode, tty *os.File, summary SummaryMode) *Terminal {
	color := mode == ColorAlways
	if tty != nil {
		color = shouldColor(mode, tty)
	}
	return &Terminal{w: bufio.NewWriter(w), color: color, summary: summary}
}

// WriteMatch implements ports.OutputSink.
func (t *Terminal) WriteMatch(m domain.Match) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	loc := domain.LocationString(m.Path, m.Location)
	if t.color {
		fmt.Fprintf(t.w, "%s%s%s\n", ansiPathColor, loc, ansiReset)
	} else {
		fmt.Fprintf(t.w, "%s\n", loc)
	}

	t.writeHighlighted(m.Text, m.Spans)
	t.w.WriteByte('\n')
	return t.w.Flush()
}

func (t *Terminal) writeHighlighted(text string, spans []domain.Span) {
	if len(spans) == 0 || !t.color {
		t.w.WriteString(text)
		return
	}
	pos := 0
	for _, sp := range spans {
		if sp.Start < pos || sp.Start > len(text) || sp.End > len(text) || sp.End < sp.Start {
			continue // defensively skip malformed spans
		}
		t.w.WriteString(text[pos:sp.Start])
		t.w.WriteString(ansiMatch)
		t.w.WriteString(text[sp.Start:sp.End])
		t.w.WriteString(ansiReset)
		pos = sp.End
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
