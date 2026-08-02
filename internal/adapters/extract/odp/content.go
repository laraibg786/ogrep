package odp

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// send is the callback shape used throughout this file: it delivers one
// TextUnit and reports whether the caller should keep going (false means
// the consumer's context was cancelled, so the streaming loop should
// stop early without treating that as an error).
type send func(domain.TextUnit) bool

// attrValue returns the value of the first attribute on t whose local
// name matches, ignoring namespace.
func attrValue(t xml.StartElement, localName string) string {
	for _, a := range t.Attr {
		if a.Name.Local == localName {
			return a.Value
		}
	}
	return ""
}

// attrIntDefault1 parses the named attribute as an integer, defaulting
// to 1 when absent or unparsable.
func attrIntDefault1(t xml.StartElement, localName string) int {
	v := attrValue(t, localName)
	if v == "" {
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// maxSpaceRun caps the number of literal space characters synthesized
// from a single text:s element's text:c count attribute. See odt's
// identical constant (internal/adapters/extract/odt/content.go) for the
// full rationale: without a cap, a crafted file can set text:c to an
// enormous value inside an otherwise tiny zip and turn it into a
// multi-hundred-MB TextUnit -- a large enough value can exhaust process
// memory outright, which this package's goroutine-level recover() cannot
// catch. 65536 is far beyond any legitimate document's use of this
// attribute.
const maxSpaceRun = 65536

// spaceRunCount reads text:s's text:c attribute (see attrIntDefault1)
// and clamps it to maxSpaceRun.
func spaceRunCount(t xml.StartElement) int {
	n := attrIntDefault1(t, "c")
	if n > maxSpaceRun {
		return maxSpaceRun
	}
	return n
}

// isSuppressedSubtreeRoot reports whether localName starts a subtree
// whose entire contents must be excluded from whatever shape paragraph
// encloses it, regardless of what's nested inside -- the direct ODF
// analogue of docx's isDrawingWrapper + suppressDepth model (see
// internal/adapters/extract/docx/xmlstream.go), ported here for
// consistency with odt/ods even though a slide's own leak scenarios
// (alt-text on an image, a slide/cell comment) are exercised less often
// in practice: svg:title/svg:desc (accessible alt-text on an image) and
// office:binary-data (raw embedded image bytes) are not shape text,
// office:annotation (a slide comment) and text:note are out of scope the
// same way odt defers them, and text:deletion is tracked-changes' record
// of removed text (text:insertion is deliberately excluded -- see odt's
// identical list for why).
//
// Unlike odt/ods, draw:frame itself is deliberately NOT in this list: in
// odt/ods a draw:frame only ever anchors an inline image/object floating
// over otherwise unrelated body text, so suppressing it wholesale is
// correct and simplest. In odp, a draw:frame IS a slide's shape -- the
// primary container extractContentXML exists to read text out of -- so
// suppressing it here would silently discard every slide's content.
// Guarding against a paragraph nested one level too deep inside a shape
// (e.g. an embedded object within a text box, itself another draw:frame)
// is handled separately below by pDepth, not by treating draw:frame as a
// suppressed wrapper.
func isSuppressedSubtreeRoot(localName string) bool {
	switch localName {
	case "annotation", "note", "binary-data", "title", "desc", "deletion":
		return true
	}
	return false
}

// isTextContentElement reports whether localName is one of the ODF
// elements whose DIRECT xml.CharData children are genuine paragraph
// text: text:p itself, text:span (an inline formatting run), and text:a
// (an ODF hyperlink). See odt's identical helper for why a whitelist
// alone is not sufficient by itself.
func isTextContentElement(localName string) bool {
	switch localName {
	case "p", "span", "a":
		return true
	}
	return false
}

// shapeCtx tracks one open draw:frame while streaming a slide (or its
// notes): its document-order ordinal (used for a fallback name when the
// frame has no draw:name) and, once seen, its actual name. Mirrors
// pptx's identical shapeCtx.
type shapeCtx struct {
	ordinal int
	name    string
	hasName bool
}

func (s shapeCtx) displayName() string {
	if s.hasName && s.name != "" {
		return s.name
	}
	return fmt.Sprintf("Shape %d", s.ordinal)
}

// extractContentXML streams content.xml's office:presentation body,
// emitting one shapeLocation-tagged TextUnit per paragraph found inside
// a slide's shapes (one unit per text:p, not per whole shape, so a
// match's Location pinpoints the paragraph) and one notesLocation-tagged
// unit per paragraph found in that slide's associated speaker notes
// (presentation:notes), if any -- mirroring pptx's per-slide/per-shape
// model exactly. Unlike pptx (which has to resolve slide order through
// a separate presentation.xml + relationships indirection, since a
// slide's part file name need not match its editor-visible position),
// ODF's draw:page elements already appear directly inside
// office:presentation in the real, editor-visible slide order -- so no
// separate ordering step is needed here.
//
// Text capture uses docx's own two-part model (see
// internal/adapters/extract/docx/xmlstream.go's runTracker doc comment),
// ported to ODF exactly the way odt's extractContentXML does -- see that
// function's doc comment for the full rationale (a whitelist restricting
// CharData capture to text:p/text:span/text:a's direct children, plus an
// explicit subtree-suppression counter that ignores an entire
// office:annotation/text:note/svg:title/etc. subtree once entered,
// regardless of what's nested inside).
//
// Unlike odt/ods, draw:frame is NOT one of the suppressed wrappers here
// (see isSuppressedSubtreeRoot's doc comment: a frame IS a slide's
// shape, the very content this function extracts). That leaves one gap
// the whitelist/suppression pair alone doesn't close: a text:p can still
// legitimately nest one level deeper than the shape's own paragraph
// (e.g. an embedded object inside a text box, itself another
// draw:frame), and without a separate guard that nested text:p's own
// StartElement would blow away the enclosing paragraph's in-progress
// curPara. pDepth is exactly that guard, restored from the model this
// package used before this fix: only the outermost (pDepth == 1) text:p
// is treated as "the" paragraph being built and is eligible for
// CharData capture; anything nested one level deeper is parsed (so its
// own EndElement/StartElement bookkeeping stays balanced) but never
// touches curPara or triggers a flush.
//
// shapeOrdinal and notesShapeOrdinal are two INDEPENDENT counters: a
// slide's speaker notes are their own presentation:notes subtree with
// their own frames, and a notes-context frame must not consume the
// slide-body shape-numbering sequence (or vice versa) -- otherwise an
// unnamed slide-body shape following a notes frame would be mislabeled
// "Shape 3" instead of "Shape 2", since the notes frame's own ordinal
// would have silently consumed a number from the slide-body sequence.
// Both reset to 0 on each new draw:page.
func extractContentXML(f *zip.File, out send) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	dec := xml.NewDecoder(rc)

	var slideNum int
	var shapeOrdinal int
	var notesShapeOrdinal int
	var shapes []shapeCtx
	var inNotes bool

	var curPara *strings.Builder
	var pDepth int
	var suppressDepth int
	var elemStack []string
	aborted := false

	currentShapeName := func() string {
		if len(shapes) == 0 {
			return "Shape"
		}
		return shapes[len(shapes)-1].displayName()
	}

	flushSegment := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		var loc domain.Location
		if inNotes {
			loc = notesLocation{Slide: slideNum}
		} else {
			loc = shapeLocation{Slide: slideNum, Shape: currentShapeName()}
		}
		if !out(domain.TextUnit{Location: loc, Text: text}) {
			aborted = true
		}
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("decoding content.xml: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			elemStack = append(elemStack, t.Name.Local)
			if isSuppressedSubtreeRoot(t.Name.Local) {
				suppressDepth++
				continue
			}
			if suppressDepth > 0 {
				continue
			}
			switch t.Name.Local {
			case "page":
				if !inNotes {
					slideNum++
					shapeOrdinal = 0
					notesShapeOrdinal = 0
					shapes = nil
				}
			case "notes":
				inNotes = true
			case "frame":
				var ord int
				if inNotes {
					notesShapeOrdinal++
					ord = notesShapeOrdinal
				} else {
					shapeOrdinal++
					ord = shapeOrdinal
				}
				name := attrValue(t, "name")
				shapes = append(shapes, shapeCtx{ordinal: ord, name: name, hasName: name != ""})
			case "p":
				pDepth++
				if pDepth == 1 {
					curPara = &strings.Builder{}
				}
			case "tab":
				if pDepth == 1 && curPara != nil {
					curPara.WriteString("\t")
				}
			case "s":
				if pDepth == 1 && curPara != nil {
					n := spaceRunCount(t)
					curPara.WriteString(strings.Repeat(" ", n))
				}
			case "line-break":
				if pDepth == 1 && curPara != nil {
					flushSegment(curPara.String())
					curPara.Reset()
					if aborted {
						return nil
					}
				}
			}
		case xml.EndElement:
			if n := len(elemStack); n > 0 {
				elemStack = elemStack[:n-1]
			}
			if isSuppressedSubtreeRoot(t.Name.Local) {
				if suppressDepth > 0 {
					suppressDepth--
				}
				continue
			}
			if suppressDepth > 0 {
				continue
			}
			switch t.Name.Local {
			case "notes":
				inNotes = false
			case "frame":
				if n := len(shapes); n > 0 {
					shapes = shapes[:n-1]
				}
			case "p":
				if pDepth == 1 {
					text := ""
					if curPara != nil {
						text = curPara.String()
					}
					curPara = nil
					flushSegment(text)
					if aborted {
						return nil
					}
				}
				if pDepth > 0 {
					pDepth--
				}
			}
		case xml.CharData:
			if suppressDepth == 0 && pDepth == 1 && curPara != nil {
				if n := len(elemStack); n > 0 && isTextContentElement(elemStack[n-1]) {
					curPara.Write(t)
				}
			}
		}
	}

	return nil
}
