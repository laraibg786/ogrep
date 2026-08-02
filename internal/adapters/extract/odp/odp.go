// Package odp implements a ports.DocumentExtractor for OASIS
// OpenDocument Presentation (.odp) files.
//
// An .odp file is a zip package, structurally analogous to a .pptx
// (see internal/adapters/extract/pptx) but, like ods relative to xlsx,
// keeping everything in one part: content.xml's root is
// office:document-content, whose office:body > office:presentation
// holds every slide directly as a sequence of draw:page elements, in
// the real, editor-visible slide order -- unlike pptx, which has to
// resolve slide order through a separate presentation.xml plus a
// relationships indirection, since a pptx slide's part file name need
// not match its editor-visible position.
//
// Each draw:page's draw:frame elements are its shapes; text inside one
// (found in a nested draw:text-box) becomes a shapeLocation-tagged
// TextUnit per text:p, one per paragraph rather than per whole shape, so
// a match's Location pinpoints the paragraph -- mirroring pptx's
// identical per-paragraph granularity. A slide's speaker notes, if
// present, live in a presentation:notes child of its draw:page and
// become notesLocation-tagged units the same way pptx's associated
// notes part does.
//
// All parsing is streamed via encoding/xml's token API -- no part is
// ever unmarshalled into a full in-memory DOM.
package odp

import (
	"archive/zip"
	"context"
	"fmt"
	"io"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// contentPath is the one part every ODF presentation package must have.
const contentPath = "content.xml"

// Extractor implements ports.DocumentExtractor for OpenDocument
// Presentation (.odp) files.
type Extractor struct{}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "odp" }

// Extensions is an optional fast-path hint consumed by the registry; it
// is not authoritative -- Sniff is what actually decides.
func (Extractor) Extensions() []string {
	return []string{".odp"}
}

// Sniff implements ports.DocumentExtractor. See odt.Extractor.Sniff's
// doc comment for the mimetype-part-first, body-child-element-fallback
// strategy this mirrors exactly.
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
		return mt == "application/vnd.oasis.opendocument.presentation"
	}

	kind, err := bodyKind(contentFile)
	return err == nil && kind == "presentation"
}

// Extract implements ports.DocumentExtractor, streaming text units from
// content.xml's presentation body in slide (draw:page) order.
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
				case errc <- fmt.Errorf("panic during odp extraction: %v", r):
				default:
				}
			}
		}()

		zr, err := zip.NewReader(ra, size)
		if err != nil {
			reportErr(errc, fmt.Errorf("opening odp zip: %w", err))
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
// part exists. Mirrors odt/ods's identical helper.
// maxMimetypeBytes caps how much of the zip's "mimetype" part Sniff
// will read. Every real ODF mimetype string is well under 100 bytes; a
// crafted zip entry claiming a large uncompressed size (a classic zip-
// bomb shape via DEFLATE) must not be allowed to force an unbounded
// inflate here just to answer "does this look like an ODP file".
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
