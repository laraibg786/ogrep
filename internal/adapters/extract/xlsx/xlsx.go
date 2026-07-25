// Package xlsx implements a ports.DocumentExtractor for MS Excel
// SpreadsheetML (.xlsx, and by extension the macro-enabled .xlsm --
// same OOXML container, different content type) packages.
//
// Cell data is read via github.com/xuri/excelize/v2's Rows() iterator,
// which streams a worksheet row-by-row from its XML rather than
// unmarshaling the whole sheet into a DOM -- 100k+ rows is an explicit
// target scenario for this package (see perf_bench_test.go). Peak
// memory grows sub-linearly with row count under that iterator (far
// from a full-DOM implementation's near-linear scaling, but not
// perfectly flat either, unlike a hand-rolled encoding/xml.Decoder.Token()
// streamer), at a higher live-heap and allocation cost overall; see
// perf_bench_test.go's TestExtractPeakMemoryBoundedNotLinear for the
// measured numbers and the reasoning behind its thresholds. Sheet name
// resolution, shared strings, formula cached-values, and all the other
// OOXML cell-type quirks (boolean, error, inline string) are handled by
// excelize itself rather than reimplemented here.
//
// Known trade-off: extractAll opens the workbook via excelize.OpenReader,
// which unconditionally reads the entire file into memory (io.ReadAll)
// before parsing the zip -- unlike excelize.OpenFile, which streams off
// a real *os.File and never does this. OpenFile isn't available to us
// here: ports.DocumentExtractor.Extract is only handed an io.ReaderAt,
// not the file's path, and there's no public excelize entry point that
// takes a ReaderAt directly. This means peak memory for very large xlsx
// files scales with total FILE size (not row count -- see above), which
// the hand-rolled predecessor's zip.NewReader(ra, size) usage avoided.
// Accepted for now rather than plumbing a path through the shared
// extractor interface or spooling ra to a self-managed temp file; revisit
// if it proves to matter in practice (e.g. very large real-world xlsx
// files causing memory pressure).
package xlsx

import (
	"archive/zip"
	"context"
	"fmt"
	"io"

	"github.com/xuri/excelize/v2"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// Extractor implements ports.DocumentExtractor for .xlsx workbooks.
type Extractor struct{}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "xlsx" }

// Extensions is an optional fast-path hint consumed by the registry;
// it is not authoritative — Sniff is what actually decides.
func (Extractor) Extensions() []string { return []string{".xlsx"} }

// Sniff implements ports.DocumentExtractor. It opens the file as a zip
// and checks for the presence of xl/workbook.xml, the one part every
// valid xlsx package must have. Anything that fails to open as a zip at
// all (including encrypted OOXML, which is actually an OLE/CFB
// container rather than a zip) is simply not recognized -- Sniff never
// panics or returns an error, only false. This deliberately does NOT go
// through excelize.OpenReader, which is far heavier than this fast-path
// contract needs.
func (Extractor) Sniff(path string, ra io.ReaderAt, size int64) (isXlsx bool) {
	defer func() {
		if recover() != nil {
			isXlsx = false
		}
	}()

	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return false
	}
	return findZipFile(zr, "xl/workbook.xml") != nil
}

// findZipFile returns the *zip.File with the given exact name, or nil.
func findZipFile(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// Extract implements ports.DocumentExtractor, streaming every sheet's
// cells as TextUnits in workbook tab order.
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
				case errc <- fmt.Errorf("panic during xlsx extraction: %v", r):
				default:
				}
			}
		}()

		if err := extractAll(ctx, ra, size, units); err != nil {
			select {
			case errc <- err:
			default:
			}
		}
	}()

	return units, errc
}

// extractAll opens the workbook via excelize and streams each sheet's
// cells in tab order.
func extractAll(ctx context.Context, ra io.ReaderAt, size int64, units chan<- domain.TextUnit) error {
	f, err := excelize.OpenReader(io.NewSectionReader(ra, 0, size))
	if err != nil {
		return fmt.Errorf("opening xlsx: %w", err)
	}
	defer f.Close()

	for _, sheetName := range f.GetSheetList() {
		if ctx.Err() != nil {
			return nil
		}
		if err := extractSheet(ctx, f, sheetName, units); err != nil {
			return fmt.Errorf("sheet %q: %w", sheetName, err)
		}
	}
	return nil
}
