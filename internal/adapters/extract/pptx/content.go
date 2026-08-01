package pptx

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// shapeCtx tracks one open <p:sp> element while streaming a slide or
// notes part: its document-order ordinal (used for a fallback name
// when the shape has no cNvPr name) and, once seen, its actual name.
type shapeCtx struct {
	ordinal int
	name    string
	hasName bool
}

// displayName returns the shape's cNvPr name attribute if one was
// found, or a fallback ordinal name ("Shape N") otherwise.
func (s shapeCtx) displayName() string {
	if s.hasName && s.name != "" {
		return s.name
	}
	return fmt.Sprintf("Shape %d", s.ordinal)
}

// shapeLocation implements domain.Location for text found within a
// named shape on a slide.
type shapeLocation struct {
	Slide int
	Shape string
}

// Human deliberately omits the shape name: which shape a match landed in
// is useful machine-readable metadata (see Fields, which still carries
// it), but cluttered the console line with `(Shape "Title 1")` for every
// match without adding much a reader scanning terminal output needs.
func (l shapeLocation) Human() string {
	return fmt.Sprintf("Slide %d", l.Slide)
}

// spans is unused: pptx has no byte-offset-addressable target below the
// shape level, so every match within a shape reports the same location.
func (l shapeLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"slide": l.Slide, "shape": l.Shape}
}

// HyperlinkURI uses a bare slide-number fragment (`#N`), which is the
// convention PowerPoint and viewers actually honor when opening a local
// file:// link at a specific slide (unlike `#slide=N`, which isn't
// recognized). spans is unused for the same reason as Fields.
func (l shapeLocation) HyperlinkURI(path string, spans []domain.Span) string {
	return domain.FileURI(path, strconv.Itoa(l.Slide))
}

// notesLocation implements domain.Location for text found in a slide's
// speaker notes.
type notesLocation struct {
	Slide int
}

func (l notesLocation) Human() string {
	return fmt.Sprintf("Slide %d (Notes)", l.Slide)
}

func (l notesLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"slide": l.Slide}
}

// HyperlinkURI returns "": PowerPoint has no addressable location for
// speaker notes.
func (l notesLocation) HyperlinkURI(path string, spans []domain.Span) string { return "" }

// paragraphEmitter is invoked once per completed paragraph (on the
// <a:p> closing tag) with the name of its enclosing shape and the
// paragraph's concatenated text (all <a:t> runs within it, in document
// order, including ones inside <a:fld> fields since we key off <a:t>
// itself rather than requiring a specific parent). It returns false to
// ask streamParagraphs to stop scanning early, e.g. because the
// consumer's context was cancelled; streamParagraphs then returns nil
// (not an error) as soon as it observes that.
type paragraphEmitter func(shapeName, text string) (cont bool)

// streamParagraphs walks r's XML token by token — never buffering more
// than one in-progress paragraph's text plus a small stack of
// currently-open shape names — recognizing the shared
// p:cSld > p:spTree > p:sp > p:txBody > a:p > a:r > a:t structure used
// by both regular slide parts (root p:sld) and notes parts (root
// p:notes). Paragraphs with no text (empty after concatenation, e.g. a
// blank line) are not emitted, since they can never match anything.
func streamParagraphs(r io.Reader, emit paragraphEmitter) error {
	dec := xml.NewDecoder(r)

	var shapes []shapeCtx
	shapeOrdinal := 0

	var inParagraph, inText bool
	var para strings.Builder

	currentShapeName := func() string {
		if len(shapes) == 0 {
			return "Shape"
		}
		return shapes[len(shapes)-1].displayName()
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case nameIs(t.Name, nsPresentationMain, "p", "sp"):
				shapeOrdinal++
				shapes = append(shapes, shapeCtx{ordinal: shapeOrdinal})
			case nameIs(t.Name, nsPresentationMain, "p", "cNvPr"):
				if len(shapes) > 0 {
					top := &shapes[len(shapes)-1]
					if !top.hasName {
						for _, a := range t.Attr {
							if a.Name.Local == "name" {
								top.name = a.Value
								top.hasName = true
								break
							}
						}
					}
				}
			case nameIs(t.Name, nsDrawingMain, "a", "p"):
				inParagraph = true
				para.Reset()
			case nameIs(t.Name, nsDrawingMain, "a", "t"):
				inText = true
			}
		case xml.EndElement:
			switch {
			case nameIs(t.Name, nsPresentationMain, "p", "sp"):
				if len(shapes) > 0 {
					shapes = shapes[:len(shapes)-1]
				}
			case nameIs(t.Name, nsDrawingMain, "a", "p"):
				if inParagraph {
					text := para.String()
					if text != "" {
						if !emit(currentShapeName(), text) {
							return nil
						}
					}
					inParagraph = false
					para.Reset()
				}
			case nameIs(t.Name, nsDrawingMain, "a", "t"):
				inText = false
			}
		case xml.CharData:
			if inParagraph && inText {
				para.Write(t)
			}
		}
	}
}

// partKind distinguishes a slide's own shapes from its speaker notes,
// purely as an internal detail of how streamPart picks a Location type —
// it has no meaning outside this package.
type partKind int

const (
	shapePart partKind = iota
	notesPart
)

// streamPart opens f (a slide or notes zip part) and streams its
// paragraphs onto units as TextUnits of the given kind, associated
// with slideNum. It reports aborted=true if the consumer's context was
// cancelled partway through (units<- lost the race to ctx.Done()), in
// which case the caller should stop processing further parts
// immediately rather than continuing to produce units nobody will
// read.
func streamPart(ctx context.Context, units chan<- domain.TextUnit, f *zip.File, kind partKind, slideNum int) (aborted bool, err error) {
	rc, oerr := f.Open()
	if oerr != nil {
		return false, fmt.Errorf("pptx: opening %s: %w", f.Name, oerr)
	}
	defer rc.Close()

	perr := streamParagraphs(rc, func(shapeName, text string) bool {
		var loc domain.Location
		switch kind {
		case notesPart:
			loc = notesLocation{Slide: slideNum}
		default: // shapePart
			loc = shapeLocation{Slide: slideNum, Shape: shapeName}
		}

		unit := domain.TextUnit{Location: loc, Text: text}
		select {
		case units <- unit:
			return true
		case <-ctx.Done():
			aborted = true
			return false
		}
	})
	if perr != nil {
		return aborted, fmt.Errorf("pptx: parsing %s: %w", f.Name, perr)
	}
	return aborted, nil
}
