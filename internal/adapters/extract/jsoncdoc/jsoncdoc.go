// Package jsoncdoc implements a ports.DocumentExtractor for JSONC (JSON
// With Commas and Comments, aka JWCC -- the format VS Code uses for
// settings.json/tsconfig.json): JSON extended with "//" line comments,
// "/* */" block comments, and trailing commas.
//
// Extraction produces two kinds of domain.TextUnit:
//   - one per comment, each independently greppable and located at its
//     own real source line/column; and
//   - one per JSON value leaf, identical in shape to jsondoc's own
//     output (jq-style path, "<path> = <value>" text).
//
// The value side is not reimplemented: github.com/tailscale/hujson's
// Standardize method blanks every comment and trailing comma with
// whitespace in place, preserving byte length and every line number
// exactly -- so the standardized bytes can be hand straight to
// jsondoc.Extractor.Extract unchanged, and the line/column it computes
// against those bytes is already correct against the original JSONC
// source, since nothing shifted.
//
// hujson has no streaming API (Parse takes a []byte, not an io.Reader),
// so -- like yamldoc -- this package gates Sniff on file size and
// accepts the cost of buffering the whole file: JSONC in practice means
// human-edited configuration files, not bulk data, so this is not
// expected to matter.
//
// The package self-registers into registry.Default on package init, so
// importing this package for its side effect (see
// internal/adapters/extract/all) is enough to make it available.
package jsoncdoc

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/tailscale/hujson"

	"github.com/laraibg786/ogrep/internal/adapters/extract/jsondoc"
	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// maxSniffSize caps the file size Sniff (and therefore Extract, since
// the registry only calls Extract on files Sniff has claimed) will
// attempt to parse. hujson.Parse buffers the entire document in memory
// -- there is no streaming JSONC parser -- so gating oversized files
// here means the registry's fallback dispatch hands them to the
// permissive text plugin instead, degrading gracefully rather than
// risking an unbounded-memory parse.
const maxSniffSize = 64 * 1024 * 1024 // 64 MiB

// Extractor implements ports.DocumentExtractor for JSONC documents.
type Extractor struct{}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "jsonc" }

// Extensions is an optional fast-path hint consumed by the registry;
// Sniff (content inspection) is what actually decides.
func (Extractor) Extensions() []string { return []string{".jsonc"} }

// Sniff implements ports.DocumentExtractor. It gates on size first (see
// maxSniffSize), then attempts a real parse: JWCC's grammar is strict
// enough (unlike YAML's permissive plain-scalar production) that
// arbitrary plain text -- an MS Office lock file, prose, etc. -- simply
// fails to parse, so no separate "does this actually have structure"
// guard is needed the way yamldoc's Sniff requires one.
func (Extractor) Sniff(path string, ra io.ReaderAt, size int64) (ok bool) {
	// Sniff must never panic-crash the caller: hujson is third-party
	// code operating on attacker-controllable input.
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()

	if size <= 0 || size > maxSniffSize {
		return false
	}

	data, err := readAll(ra, size)
	if err != nil {
		return false
	}
	_, err = hujson.Parse(data)
	return err == nil
}

// readAll reads the full size bytes from ra via a bounded
// io.SectionReader, mirroring how the other size-gated extractors turn
// an io.ReaderAt into a byte slice.
func readAll(ra io.ReaderAt, size int64) ([]byte, error) {
	sr := io.NewSectionReader(ra, 0, size)
	return io.ReadAll(sr)
}

// Extract implements ports.DocumentExtractor.
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
				case errc <- fmt.Errorf("panic during jsonc extraction: %v", r):
				default:
				}
			}
		}()

		data, err := readAll(ra, size)
		if err != nil {
			select {
			case errc <- err:
			default:
			}
			return
		}

		v, err := hujson.Parse(data)
		if err != nil {
			select {
			case errc <- err:
			default:
			}
			return
		}

		// Comments must be collected before Standardize: it blanks
		// every Extra region's non-whitespace bytes in place, which
		// would destroy the comment text if read afterward.
		for _, c := range collectComments(data, &v) {
			if err := ctx.Err(); err != nil {
				return
			}
			unit := domain.TextUnit{
				Location: commentLocation{Line: c.line, Column: c.col},
				Text:     c.text,
			}
			select {
			case units <- unit:
			case <-ctx.Done():
				return
			}
		}

		v.Standardize()
		standardized := v.Pack()

		valueUnits, valueErrc := (jsondoc.Extractor{}).Extract(ctx, bytes.NewReader(standardized), int64(len(standardized)))
		for u := range valueUnits {
			select {
			case units <- u:
			case <-ctx.Done():
				return
			}
		}
		if err := <-valueErrc; err != nil {
			select {
			case errc <- err:
			default:
			}
		}
	}()

	return units, errc
}
