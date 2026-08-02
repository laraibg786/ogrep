// Package ods implements a ports.DocumentExtractor for OASIS
// OpenDocument Spreadsheet (.ods) files.
//
// An .ods file is a zip package, structurally analogous to a .xlsx
// (see internal/adapters/extract/xlsx) but using ODF's own XML schema
// and, unlike xlsx, keeping everything in one part: content.xml's root
// is office:document-content, whose office:body > office:spreadsheet
// holds every sheet directly as a sequence of table:table elements (no
// separate workbook.xml + relationships indirection is needed the way
// xlsx requires to resolve a sheet's display name to its worksheet
// part -- an ODS table:table carries its own table:name attribute right
// on the element).
//
// Each table:table > table:table-row > table:table-cell holds its
// display text as one or more text:p children (a cell wrapped across
// multiple visual lines has multiple text:p elements, each emitted as
// its own TextUnit sharing that cell's Location -- see content.go).
// table:number-columns-repeated / table:number-rows-repeated are ODF's
// compact encoding for "this cell/row repeats N times", almost always
// used for a large trailing run of empty cells/rows; see content.go's
// doc comment for how those are handled without either double-emitting
// or leaving the row/column cursor pointing at the wrong place for
// whatever real content follows.
//
// All parsing is streamed via encoding/xml's token API -- no part is
// ever unmarshalled into a full in-memory DOM.
package ods

import (
	"archive/zip"
	"context"
	"fmt"
	"io"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// contentPath is the one part every ODF spreadsheet package must have.
const contentPath = "content.xml"

// Extractor implements ports.DocumentExtractor for OpenDocument
// Spreadsheet (.ods) files.
type Extractor struct{}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "ods" }

// Extensions is an optional fast-path hint consumed by the registry; it
// is not authoritative -- Sniff is what actually decides.
func (Extractor) Extensions() []string {
	return []string{".ods"}
}

// Sniff implements ports.DocumentExtractor. See odt.Extractor.Sniff's
// doc comment for the mimetype-part-first, body-child-element-fallback
// strategy this mirrors exactly (odt/ods/odp all share the same
// content.xml part name, so the mimetype part -- or, failing that,
// office:body's own child element name -- is what actually
// discriminates between them).
func (Extractor) Sniff(path string, ra io.ReaderAt, size int64) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()

	if size <= 0 {
		return false
	}

	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return false
	}

	var contentFile *zip.File
	for _, f := range zr.File {
		if f.Name == contentPath {
			contentFile = f
		}
	}
	if contentFile == nil {
		return false
	}

	if mt, ok := readMimetype(zr); ok {
		return mt == "application/vnd.oasis.opendocument.spreadsheet"
	}

	kind, err := bodyKind(contentFile)
	return err == nil && kind == "spreadsheet"
}

// Extract implements ports.DocumentExtractor, streaming text units from
// content.xml's spreadsheet body in table (sheet) order.
func (Extractor) Extract(ctx context.Context, ra io.ReaderAt, size int64) (<-chan domain.TextUnit, <-chan error) {
	units := make(chan domain.TextUnit, domain.TextUnitChannelBuffer)
	errc := make(chan error, 1)

	go func() {
		defer close(units)
		defer close(errc)
		// A panic here happens in this goroutine, not the caller's, so
		// only a recover() here can catch it. Per the
		// ports.DocumentExtractor contract, convert it into a single
		// error on errc instead of letting it crash the process.
		defer func() {
			if r := recover(); r != nil {
				select {
				case errc <- fmt.Errorf("panic during ods extraction: %v", r):
				default:
				}
			}
		}()

		zr, err := zip.NewReader(ra, size)
		if err != nil {
			reportErr(errc, fmt.Errorf("opening ods zip: %w", err))
			return
		}

		var contentFile *zip.File
		for _, f := range zr.File {
			if f.Name == contentPath {
				contentFile = f
			}
		}
		if contentFile == nil {
			return
		}

		send := func(u domain.TextUnit) bool {
			select {
			case units <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}

		if err := extractContentXML(contentFile, send); err != nil {
			reportErr(errc, err)
		}
	}()

	return units, errc
}

// reportErr sends a single error on errc without blocking, matching the
// "send at most one error" contract.
func reportErr(errc chan<- error, err error) {
	select {
	case errc <- err:
	default:
	}
}

// readMimetype returns the content of the zip's mimetype part, trimmed
// of any trailing whitespace some producers add. ok is false if no such
// part exists. Mirrors odt's identical helper.
// maxMimetypeBytes caps how much of the zip's "mimetype" part Sniff
// will read. Every real ODF mimetype string is well under 100 bytes; a
// crafted zip entry claiming a large uncompressed size (a classic zip-
// bomb shape via DEFLATE) must not be allowed to force an unbounded
// inflate here just to answer "does this look like an ODS file".
const maxMimetypeBytes = 256

func readMimetype(zr *zip.Reader) (mimetype string, ok bool) {
	for _, f := range zr.File {
		if f.Name != "mimetype" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", false
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, maxMimetypeBytes+1))
		if err != nil || len(data) > maxMimetypeBytes {
			return "", false
		}
		s := string(data)
		for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
			s = s[:len(s)-1]
		}
		return s, true
	}
	return "", false
}
