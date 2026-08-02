// Package jsonldoc implements a ports.DocumentExtractor for JSON Lines
// (JSONL/NDJSON) documents: files where each line is its own standalone
// JSON value, commonly used for logs and data dumps.
//
// This deliberately doesn't reimplement JSON flattening: each non-blank
// line is handed to jsondoc.Extractor.Extract as if it were its own
// tiny file, and the resulting units are re-emitted with the line's real
// file line number substituted in place of the "line 1" jsondoc always
// reports for a single-line input (a single JSONL line can never itself
// contain a newline, so jsondoc's own position tracking always reports
// line 1 for it -- only the line number needs correcting, the column
// jsondoc computes is already accurate for that line's own content).
//
// No ".document[N]"-style prefix is added to the jq path, unlike
// yamldoc's multi-document case: jq itself processes each top-level
// value in its input as a separate item by default (this is exactly
// what makes `jq '.foo'` a well-known idiom for extracting a field from
// every line of an NDJSON stream) with no document-index selector
// needed, so a bare per-record path like ".foo.bar[2]" is already
// directly pasteable and correct when run against the whole file.
//
// Malformed JSON on any single line aborts the whole extraction with an
// error identifying that line, matching the "any decode error aborts"
// behavior every other structured-format plugin in this codebase uses
// (jsondoc/yamldoc/xmldoc all behave the same way for their own
// single-document malformed content) -- this is a deliberate consistency
// choice, not an oversight; NDJSON tooling conventions (e.g. jq's default
// mode) commonly behave the same way.
//
// The package self-registers into registry.Default on package init, so
// importing this package for its side effect (see
// internal/adapters/extract/all) is enough to make it available.
package jsonldoc

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/laraibg786/ogrep/internal/adapters/extract/jsondoc"
	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// maxLineSize bounds how long a single JSONL line may be before it's
// truncated by the scanner's buffer, mirroring text.go's identical bound
// for the same reason (bufio.Scanner's 64KB default is too small for
// some real-world single-line JSON records).
const maxLineSize = 1024 * 1024 // 1MiB

// Extractor implements ports.DocumentExtractor for JSON Lines documents.
type Extractor struct{}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "jsonl" }

// Extensions is an optional fast-path hint consumed by the registry;
// Sniff (content inspection) is what actually decides. ".ndjson" is the
// other common extension for the same format.
func (Extractor) Extensions() []string { return []string{".jsonl", ".ndjson"} }

// Sniff implements ports.DocumentExtractor. It finds the first non-blank
// line and delegates to jsondoc's own Sniff on just that line -- reusing
// its cheap first-byte-plausible-then-first-token-decodes check rather
// than reimplementing it. A file with no non-blank line at all (empty,
// or blank lines only) is declined.
func (Extractor) Sniff(path string, ra io.ReaderAt, size int64) (ok bool) {
	// Sniff must never panic-crash the caller.
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()

	if size <= 0 {
		return false
	}

	sr := io.NewSectionReader(ra, 0, size)
	scanner := bufio.NewScanner(sr)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue // blank separator line
		}
		return (jsondoc.Extractor{}).Sniff("", bytes.NewReader(line), int64(len(line)))
	}
	return false
}

// Extract implements ports.DocumentExtractor, streaming each non-blank
// line's flattened key/value pairs as TextUnits with the line's real
// file line number substituted in.
func (Extractor) Extract(ctx context.Context, ra io.ReaderAt, size int64) (<-chan domain.TextUnit, <-chan error) {
	units := make(chan domain.TextUnit)
	errc := make(chan error, 1)

	go func() {
		defer close(units)
		defer close(errc)
		// This goroutine's own panics cannot be caught by a recover()
		// anywhere else (e.g. the orchestrator's per-file worker) --
		// only a recover deferred here, in the same goroutine, can. Per
		// the ports.DocumentExtractor contract, convert any panic into
		// a single error on errc instead of crashing the process. See
		// internal/adapters/extract/text/text.go for the pattern this
		// mirrors.
		defer func() {
			if r := recover(); r != nil {
				select {
				case errc <- fmt.Errorf("panic during jsonl extraction: %v", r):
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
			if err := ctx.Err(); err != nil {
				return
			}

			raw := scanner.Bytes()
			if len(bytes.TrimSpace(raw)) == 0 {
				continue // blank separator line
			}
			// Copy: scanner.Bytes() aliases a buffer reused by the next
			// Scan() call, and jsondoc.Extract's own goroutine reads it
			// concurrently with this loop until fully drained below.
			// Deliberately NOT trimmed before decoding: jsondoc's
			// position tracking is relative to whatever bytes it's
			// given, so passing the raw (not whitespace-trimmed) line
			// keeps its reported column accurate against the real file
			// content -- json.Decoder already skips leading
			// insignificant whitespace itself when computing offsets.
			lineData := append([]byte(nil), raw...)

			lineUnits, lineErrc := (jsondoc.Extractor{}).Extract(ctx, bytes.NewReader(lineData), int64(len(lineData)))
			for u := range lineUnits {
				fields := u.Location.Fields(nil)
				path, _ := fields["jsonpath"].(string)
				col, _ := fields["col"].(int)
				unit := domain.TextUnit{
					Location: lineLocation{Path: path, Line: lineNo, Column: col},
					Text:     u.Text,
				}
				select {
				case units <- unit:
				case <-ctx.Done():
					return
				}
			}
			if err := <-lineErrc; err != nil {
				select {
				case errc <- fmt.Errorf("line %d: %w", lineNo, err):
				default:
				}
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
