// Package xlsx implements a ports.DocumentExtractor for MS Excel
// SpreadsheetML (.xlsx) packages.
//
// An xlsx file is a zip package. The parts this extractor reads are:
//
//   - xl/workbook.xml: declares each sheet's display name and a
//     relationship id (r:id) -- NOT its file name directly.
//   - xl/_rels/workbook.xml.rels: resolves each r:id to the worksheet
//     part that actually holds it (e.g. "worksheets/sheet3.xml"). Sheet
//     declaration order and underlying file naming are independent, so
//     both parts must be read to build an accurate sheet name -> part
//     path mapping; guessing (e.g. assuming "Sheet1" lives in
//     sheet1.xml) would be wrong for reordered/renamed sheets.
//   - xl/sharedStrings.xml: the shared-string table, small relative to
//     the workbook as a whole, loaded eagerly into memory.
//   - xl/worksheets/sheetN.xml: the actual cell data, which can be huge
//     (100k+ rows is an explicit target scenario) and is therefore
//     streamed with encoding/xml.Decoder.Token() rather than unmarshaled
//     into a DOM.
//
// The package self-registers into registry.Default on init(), mirroring
// internal/adapters/extract/text/text.go, the reference implementation
// this extractor's Extract goroutine panic-safety pattern is copied
// from.
package xlsx

import (
	"archive/zip"
	"context"
	"fmt"
	"io"

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
// Sniff (content inspection) is what actually decides.
func (Extractor) Extensions() []string { return []string{".xlsx"} }

// Sniff implements ports.DocumentExtractor. It opens the file as a zip
// and checks for the presence of xl/workbook.xml, the one part every
// valid xlsx package must have. Anything that fails to open as a zip at
// all (including encrypted OOXML, which is actually an OLE/CFB
// container rather than a zip) is simply not recognized -- Sniff never
// panics or returns an error, only false.
func (Extractor) Sniff(path string, ra io.ReaderAt, size int64) (isXlsx bool) {
	// Defensive: archive/zip is not expected to panic on malformed
	// input, but Sniff is a hard "never panic" contract per
	// ports.DocumentExtractor, so guard it anyway rather than relying
	// on that expectation holding for every adversarial input.
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

// Extract implements ports.DocumentExtractor, streaming every sheet's
// cells as TextUnits in workbook tab order.
func (Extractor) Extract(ctx context.Context, ra io.ReaderAt, size int64) (<-chan domain.TextUnit, <-chan error) {
	units := make(chan domain.TextUnit, domain.TextUnitChannelBuffer)
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

// extractAll opens the zip, resolves sheet names to their part paths,
// loads shared strings, and streams each sheet's cells in turn.
func extractAll(ctx context.Context, ra io.ReaderAt, size int64, units chan<- domain.TextUnit) error {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return fmt.Errorf("opening xlsx zip: %w", err)
	}

	sheets, err := readWorkbookSheets(zr)
	if err != nil {
		return err
	}

	sharedStrings, err := readSharedStrings(zr)
	if err != nil {
		return err
	}

	for _, sh := range sheets {
		if ctx.Err() != nil {
			return nil
		}
		f := findZipFile(zr, sh.Target)
		if f == nil {
			// Dangling sheet->part reference (malformed input); skip
			// this sheet rather than failing the whole workbook.
			continue
		}
		if err := extractSheet(ctx, f, sh.Name, sharedStrings, units); err != nil {
			return fmt.Errorf("sheet %q: %w", sh.Name, err)
		}
	}
	return nil
}
