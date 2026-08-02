// Package text implements a DocumentExtractor for plain text files. It
// streams the file line by line, skipping files that look binary (a
// null byte in the first few KB), mirroring rg's default binary-file
// skip behavior.
//
// The extractor self-registers into the process-wide registry.Default
// on package init, so importing this package for its side effect (see
// internal/adapters/extract/all) is enough to make it available.
package text

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// sniffWindow is the number of leading bytes inspected to decide whether
// a file looks binary.
const sniffWindow = 8192

// maxLineSize bounds how long a single line may be before it's
// truncated by the scanner's buffer. bufio.Scanner's default token
// limit (64KB) is too small for some real-world text files (e.g.
// minified data dumps), so we raise it substantially.
const maxLineSize = 1024 * 1024 // 1MiB

// Extractor implements ports.DocumentExtractor for plain text files.
type Extractor struct{}

// lineLocation implements domain.Location for a single line of a plain
// text file.
type lineLocation struct {
	Line int
}

// Human renders just the line number so LocationString produces the
// familiar grep "path:N" prefix, rather than the redundant "path:line N".
func (l lineLocation) Human() string { return strconv.Itoa(l.Line) }

// Fields includes "col" alongside "line" for parity with the
// ":line:col" grep-style suffix HyperlinkURI produces. The column is the
// first span's start converted to a 1-based byte offset within the
// line, since spans are the triggering Match's byte-offset ranges into
// this unit's Text (which for a text line IS the real line content, not
// a synthesized string) — falls back to column 1 when spans is empty
// (e.g. a Location-only test calling Fields(nil) directly).
func (l lineLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"line": l.Line, "col": startColumn(spans)}
}

// HyperlinkURI implements domain.Location with the standard grep-style
// file/line/column URI, landing on the first matched span's actual
// starting byte within the line rather than always column 1 -- an
// editor opening this URI lands the cursor at the match, not just
// somewhere on the right line. When more than one span matched on this
// line, the first one's start is used (the same convention rg-style
// tools use when multiple matches occur on a single line). The
// ":line:col" suffix is appended after escaping, not before: it's editor
// convention, not a URI fragment, and ':' is a legal unescaped path
// character.
func (l lineLocation) HyperlinkURI(path string, spans []domain.Span) string {
	return fmt.Sprintf("%s:%d:%d", domain.FileURI(path, ""), l.Line, startColumn(spans))
}

// startColumn converts the first span's byte-offset Start into a 1-based
// column, or 1 if spans is empty.
func startColumn(spans []domain.Span) int {
	if len(spans) == 0 {
		return 1
	}
	return spans[0].Start + 1
}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "text" }

// Extensions is an optional fast-path hint consumed by the registry; it
// is not authoritative — Sniff is what actually decides.
func (Extractor) Extensions() []string {
	return []string{".txt", ".md", ".log", ".csv", ".ini", ".conf", ".cfg", ".go", ".py", ".js", ".ts", ".java", ".c", ".h", ".cpp", ".rs", ""}
}

// Sniff implements ports.DocumentExtractor. It treats any file that
// doesn't look binary as plain text; this extractor is intended to be
// tried as the fallback after the OOXML format extractors have had a
// chance to claim the file, since the registry's dispatch order tries
// extension-matched extractors first within the office formats, and
// text acts as the catch-all.
func (Extractor) Sniff(path string, ra io.ReaderAt, size int64) bool {
	// Never claim a zip-based OOXML package, even if the office
	// extractors somehow didn't run first (e.g. a test using this
	// extractor in isolation). This keeps behavior sane no matter the
	// registration order.
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".docx", ".pptx", ".xlsx":
		return false
	}

	n := size
	if n > sniffWindow {
		n = sniffWindow
	}
	if n <= 0 {
		return true // empty file: treat as (empty) text
	}
	buf := make([]byte, n)
	if _, err := ra.ReadAt(buf, 0); err != nil && err != io.EOF {
		return false
	}
	for _, b := range buf {
		if b == 0 {
			return false
		}
	}
	return true
}

// Extract implements ports.DocumentExtractor, streaming the file one
// line at a time.
func (Extractor) Extract(ctx context.Context, ra io.ReaderAt, size int64) (<-chan domain.TextUnit, <-chan error) {
	units := make(chan domain.TextUnit)
	errc := make(chan error, 1)

	go func() {
		defer close(units)
		defer close(errc)
		// A panic here happens in this goroutine, not the caller's
		// (e.g. the orchestrator's), so only a recover() here — not one
		// in whatever goroutine reads from units/errc — can catch it.
		// Per the ports.DocumentExtractor contract, we convert it into
		// an error on errc instead of letting it crash the process.
		defer func() {
			if r := recover(); r != nil {
				select {
				case errc <- fmt.Errorf("panic during text extraction: %v", r):
				default:
				}
			}
		}()

		sr := io.NewSectionReader(ra, 0, size)
		scanner := bufio.NewScanner(sr)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			unit := domain.TextUnit{
				Location: lineLocation{Line: lineNo},
				Text:     line,
			}
			select {
			case units <- unit:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case errc <- err:
			default:
			}
		}
	}()

	return units, errc
}
