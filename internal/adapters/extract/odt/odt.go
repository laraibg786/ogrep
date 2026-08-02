// Package odt implements a ports.DocumentExtractor for OASIS
// OpenDocument Text (.odt) files.
//
// An .odt file is a zip package, structurally analogous to a .docx
// (see internal/adapters/extract/docx) but using ODF's own XML schema:
// content.xml's root is office:document-content, whose office:body >
// office:text holds the document as a sequence of text:p (paragraph)
// and text:h (heading, ODF's own distinct element for a heading --
// unlike Word, which reuses w:p with a Heading-named style) elements,
// plus table:table > table:table-row > table:table-cell for tables.
//
// Like docx, paragraphLocation/cellLocation.Human() reports the nearest
// preceding text:h's own text rather than a bare paragraph/cell number
// -- see docx's package doc comment and this package's content.go for
// the shared rationale (ODF's own navigation concepts are headings,
// bookmarks, and pages, never an arbitrary paragraph index either).
//
// Scope: only content.xml's body (paragraphs, headings, tables) is
// extracted. Headers/footers live in styles.xml (as style:header/
// style:footer, referenced indirectly by a page-layout style rather
// than appearing inline in the flow) and footnotes/annotations
// (text:note, office:annotation) are interspersed inline within
// content.xml's own text runs; both are deferred as a follow-up rather
// than attempted here, to keep this first ODF plugin's scope focused
// and well-tested rather than broad and undertested.
//
// All parsing is streamed via encoding/xml's token API -- no part is
// ever unmarshalled into a full in-memory DOM.
package odt

import (
	"archive/zip"
	"context"
	"fmt"
	"io"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// contentPath is the one part every ODF text document package must
// have.
const contentPath = "content.xml"

// Extractor implements ports.DocumentExtractor for OpenDocument Text
// (.odt) files.
type Extractor struct{}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "odt" }

// Extensions is an optional fast-path hint consumed by the registry; it
// is not authoritative -- Sniff is what actually decides.
func (Extractor) Extensions() []string {
	return []string{".odt"}
}

// Sniff implements ports.DocumentExtractor. It opens the file as a zip
// archive, requires content.xml to be present, and -- since ODF's own
// mimetype part (a stored, uncompressed "application/vnd.oasis.
// opendocument.text" entry every conformant package includes) is the
// authoritative discriminator between odt/ods/odp, which otherwise all
// share the exact same content.xml part name -- checks that part's
// content rather than guessing from content.xml alone. A producer that
// omits the mimetype entry (nonconformant but not unheard of) falls
// back to sniffing content.xml's own office:body child element name,
// which is format-specific regardless (office:text vs
// office:spreadsheet vs office:presentation). Any failure to open the
// file as a zip results in false, never a panic.
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
		return mt == "application/vnd.oasis.opendocument.text"
	}

	kind, err := bodyKind(contentFile)
	return err == nil && kind == "text"
}

// Extract implements ports.DocumentExtractor, streaming text units from
// content.xml's document body.
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
				case errc <- fmt.Errorf("panic during odt extraction: %v", r):
				default:
				}
			}
		}()

		zr, err := zip.NewReader(ra, size)
		if err != nil {
			reportErr(errc, fmt.Errorf("opening odt zip: %w", err))
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

// maxMimetypeBytes caps how much of the zip's "mimetype" part Sniff
// will read. Every real ODF mimetype string is well under 100 bytes; a
// crafted zip entry claiming a large uncompressed size (a classic zip-
// bomb shape via DEFLATE) must not be allowed to force an unbounded
// inflate here just to answer "does this look like an ODT file".
const maxMimetypeBytes = 256

// readMimetype returns the content of the zip's mimetype part (present,
// stored uncompressed, as the package's first entry in every
// spec-conformant ODF file), trimmed of any trailing whitespace some
// producers add. ok is false if no such part exists.
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
		// Trim trailing whitespace/newlines only; the mimetype string
		// itself never legitimately contains any.
		for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
			s = s[:len(s)-1]
		}
		return s, true
	}
	return "", false
}
