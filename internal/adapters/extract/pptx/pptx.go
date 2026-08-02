// Package pptx implements a DocumentExtractor for PowerPoint (OOXML
// PresentationML) files. A .pptx is a zip package; this plugin reads
// ppt/presentation.xml plus its relationships file to determine the
// authoritative, editor-visible slide order (which need not match the
// numeric suffixes of the ppt/slides/slideN.xml part names), then
// streams each slide's shape text and, where present, its associated
// speaker notes.
//
// The extractor self-registers into the process-wide registry.Default
// on package init, so importing this package for its side effect (see
// internal/adapters/extract/all) is enough to make it available.
package pptx

import (
	"archive/zip"
	"context"
	"fmt"
	"io"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// presentationPath is the part that both confirms this is a pptx
// package (Sniff) and anchors slide-order resolution (Extract).
const presentationPath = "ppt/presentation.xml"

// presentationRelsPath is presentation.xml's own relationships part,
// which maps the r:id references in its sldIdLst to actual slide part
// paths.
const presentationRelsPath = "ppt/_rels/presentation.xml.rels"

// Extractor implements ports.DocumentExtractor for .pptx files.
type Extractor struct{}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "pptx" }

// Extensions is an optional fast-path hint consumed by the registry;
// it is not authoritative — Sniff is what actually decides.
func (Extractor) Extensions() []string {
	return []string{".pptx"}
}

// Sniff implements ports.DocumentExtractor. It opens the file as a zip
// archive and checks for the presence of ppt/presentation.xml, which
// every valid pptx package must contain. Any failure to open the file
// as a zip (including encrypted OOXML, which is actually an OLE/CFB
// container rather than a zip) is treated as "not a pptx" rather than
// an error, and a defensive recover guards against any unexpected
// panic from malformed zip central-directory data so Sniff never
// crashes the caller.
func (Extractor) Sniff(path string, ra io.ReaderAt, size int64) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()

	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return false
	}
	for _, f := range zr.File {
		if f.Name == presentationPath {
			return true
		}
	}
	return false
}

// Extract implements ports.DocumentExtractor. It streams one TextUnit
// per paragraph: a shapeLocation-tagged unit for paragraphs found in a
// slide's shapes (one TextUnit per <a:p>, not per whole shape, so a
// match's Location pinpoints the paragraph rather than a whole
// possibly-large shape), and a notesLocation-tagged unit for paragraphs
// found in that slide's associated notes part, if any.
func (Extractor) Extract(ctx context.Context, ra io.ReaderAt, size int64) (<-chan domain.TextUnit, <-chan error) {
	units := make(chan domain.TextUnit, domain.TextUnitChannelBuffer)
	errc := make(chan error, 1)

	go func() {
		defer close(units)
		defer close(errc)
		// A panic here happens in this goroutine, not the caller's (e.g.
		// the orchestrator's), so only a recover() here can catch it. Per
		// the ports.DocumentExtractor contract, we convert it into an
		// error on errc instead of letting it crash the process.
		defer func() {
			if r := recover(); r != nil {
				select {
				case errc <- fmt.Errorf("panic during pptx extraction: %v", r):
				default:
				}
			}
		}()

		zr, err := zip.NewReader(ra, size)
		if err != nil {
			sendErr(errc, fmt.Errorf("pptx: opening zip: %w", err))
			return
		}

		files := make(map[string]*zip.File, len(zr.File))
		for _, f := range zr.File {
			files[f.Name] = f
		}

		order, err := resolveSlideOrder(files)
		if err != nil {
			sendErr(errc, err)
			return
		}

		for i, slidePath := range order {
			slideNum := i + 1

			sf, ok := files[slidePath]
			if !ok {
				// Dangling relationship pointing at a part that doesn't
				// exist in the archive; skip it rather than failing the
				// whole extraction.
				continue
			}

			aborted, err := streamPart(ctx, units, sf, shapePart, slideNum)
			if err != nil {
				sendErr(errc, err)
				return
			}
			if aborted {
				return
			}

			notesPath, ok := resolveNotesPath(files, slidePath)
			if !ok {
				continue
			}
			nf, ok := files[notesPath]
			if !ok {
				continue
			}

			aborted, err = streamPart(ctx, units, nf, notesPart, slideNum)
			if err != nil {
				sendErr(errc, err)
				return
			}
			if aborted {
				return
			}
		}
	}()

	return units, errc
}

// sendErr reports err on errc without blocking, matching the "send at
// most one error" contract (errc is buffered with capacity 1).
func sendErr(errc chan<- error, err error) {
	select {
	case errc <- err:
	default:
	}
}
