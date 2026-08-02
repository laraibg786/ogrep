package odt

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
// name matches, ignoring namespace (mirrors docx's identical helper).
func attrValue(t xml.StartElement, localName string) string {
	for _, a := range t.Attr {
		if a.Name.Local == localName {
			return a.Value
		}
	}
	return ""
}

// attrIntDefault1 parses the named attribute as an integer, defaulting
// to 1 when the attribute is absent or unparsable -- used for
// table:number-columns-repeated / table:number-rows-repeated, whose
// absence means "not repeated" (i.e. exactly one column/row), same as
// ODF itself treats a missing repeat count.
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
// from a single text:s element's text:c count attribute. text:s exists
// so ODF can represent a run of literal spaces compactly (plain
// whitespace between elements is otherwise collapsed); any real,
// human-authored document uses this for at most a handful to a few
// dozen spaces (legacy fixed-width padding, table-of-contents dot
// leaders, and similar). A crafted file can set text:c to an enormous
// value (e.g. "200000000") inside an otherwise tiny zip, and without a
// cap that turns into a 200MB+ single TextUnit from a ~600-byte input --
// a large enough value can exhaust process memory outright, which,
// unlike an ordinary panic, this package's goroutine-level recover()
// cannot catch. 65536 is far beyond anything a legitimate document would
// ever need for one text:s run while keeping the worst case bounded and
// cheap.
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
// whose entire contents -- regardless of what's nested inside, including
// elements that would otherwise be part of isTextContentElement's
// whitelist below -- must be excluded from whatever paragraph or heading
// encloses it. This is the direct ODF analogue of docx's isDrawingWrapper
// + suppressDepth model (see internal/adapters/extract/docx/xmlstream.go),
// generalized to ODF's several distinct "not really body text" wrappers
// instead of docx's single w:drawing/w:pict case:
//
//   - draw:frame: an anchored image or shape. Its svg:title/svg:desc
//     (accessible alt-text) and any office:binary-data (raw embedded
//     image bytes, frequently base64 and huge) are not body text and
//     must never reach the enclosing paragraph. A nested draw:text-box's
//     own text:p (e.g. an inline captioned image) IS real content, but
//     extracting it is out of scope for this package (see the package
//     doc comment); suppressing the whole frame is what keeps that
//     nested paragraph from corrupting the enclosing paragraph's text or
//     shifting paragraph numbering, exactly as docx's suppressDepth does
//     for a w:txbxContent nested inside w:drawing.
//   - office:annotation: a margin comment. Both its author/date metadata
//     (dc:creator, dc:date) and its own comment-body text:p children are
//     out of scope -- comments are deferred entirely as a follow-up (see
//     the package doc comment), not just their body text.
//   - text:note: a footnote/endnote. Its text:note-citation (the
//     footnote mark/number rendered inline) and text:note-body (the
//     footnote's own text) are both out of scope for the same reason
//     footnotes generally are.
//   - office:binary-data / svg:title / svg:desc: listed explicitly too,
//     even though draw:frame already covers the common case, since these
//     can in principle appear via other drawing elements this package
//     doesn't otherwise special-case.
//   - text:deletion: tracked-changes' record of text removed from the
//     live document, nested inside text:tracked-changes/
//     text:changed-region. text:insertion is deliberately NOT included
//     here: unlike a deletion, an insertion's own text stays inline in
//     the normal document flow (merely bracketed by text:change-start/
//     text:change-end markers referencing it), so text:insertion within
//     text:changed-region carries only change metadata, nothing that
//     needs suppressing.
//
// This list is deliberately NOT extended to cover every ODF drawing
// shape that can anchor a text box (draw:custom-shape, draw:g, draw:
// rect, draw:ellipse, draw:line, draw:polygon, and others) -- an
// allowlist of wrapper element names is only ever as complete as
// whatever's been thought of, and draw:frame is far from the only shape
// a real ODF producer emits. extractContentXML's pDepth guard is the
// general-purpose backstop for exactly this gap: it makes ANY text:p/
// text:h nested inside another one inert (parsed for structural
// balance, never captured, never flushed as its own unit) regardless of
// what wrapper sits in between, known or not -- see its doc comment.
// draw:frame stays in this suppression list anyway because, unlike a
// bare nested paragraph, a frame's svg:title/svg:desc/office:binary-data
// aren't wrapped in a text:p at all, so pDepth alone wouldn't catch
// them.

// svgTitleDescNS lists the namespace URIs svg:title/svg:desc have
// actually been declared under across ODF's own history: the OASIS TC
// only introduced its own "svg-compatible" URN from ODF 1.2 onward;
// ODF 1.0/1.1 (and OpenOffice.org-era producers) used the plain W3C SVG
// namespace directly. isSuppressedSubtreeRoot checks the full name
// (not local name alone) against either, so an unrelated element from
// some other namespace that happens to share the local name "title" or
// "desc" -- e.g. dc:title, Dublin Core's document-title metadata
// element -- is never mistakenly suppressed; the other suppressed
// elements below carry no such ambiguity (their local names aren't
// reused elsewhere in ODF with a different meaning), so they're still
// matched by local name alone.
var svgTitleDescNS = map[string]bool{
	"urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0": true, // ODF 1.2+
	"http://www.w3.org/2000/svg":                               true, // ODF 1.0/1.1
}

func isSuppressedSubtreeRoot(name xml.Name) bool {
	switch name.Local {
	case "frame", "annotation", "note", "binary-data", "deletion":
		return true
	case "title", "desc":
		return svgTitleDescNS[name.Space]
	}
	return false
}

// isTextContentElement reports whether localName is one of the ODF
// elements whose DIRECT xml.CharData children are genuine paragraph
// text: text:p and text:h themselves (a document producer can put
// CharData straight inside a paragraph/heading element, unlike docx
// which always wraps run text in a leaf w:t), plus text:span (an inline
// formatting run) and text:a (an ODF hyperlink), the two elements real
// ODF producers nest CharData in directly within a paragraph. This is
// the whitelist half of the fix: even disregarding suppression
// entirely, CharData whose immediate enclosing element is anything else
// (svg:title, svg:desc, office:binary-data, dc:creator, ...) is refused
// on this ground alone. See isSuppressedSubtreeRoot's doc comment for
// why a whitelist alone is still not sufficient by itself: office:
// annotation's own comment-body text:p, text:note-body's footnote text,
// and text:deletion's removed text are all *also* text:p content, so
// only suppression (which looks at the whole ancestor chain, not just
// the immediate parent) can tell those apart from a genuine body
// paragraph.
func isTextContentElement(localName string) bool {
	switch localName {
	case "p", "h", "span", "a":
		return true
	}
	return false
}

// tableState tracks the current row/col cursor for one open
// table:table. rowRepeat holds the just-opened row's own
// table:number-rows-repeated value, applied to row once that row
// closes -- see the table-row Start/End handling in extractContentXML.
type tableState struct {
	num, row, col int
	rowRepeat     int
}

// cellState tracks which table/row/col a currently-open table:table-cell
// (or table:covered-table-cell) addresses, plus how many columns it (and
// any trailing repeats) actually occupies -- needed so the enclosing
// tableState's column cursor advances by the full repeat count once the
// cell closes, keeping subsequent real cells addressed correctly. See
// extractContentXML's table:number-columns-repeated handling.
//
// heading snapshots currentHeading (the nearest heading OUTSIDE any
// table) at the moment this cell opened, and is what this cell's own
// paragraphs report as their Heading -- see the heading-tracking note in
// extractContentXML's doc comment for why a text:h found inside a cell
// must update only this per-cell field, never the shared currentHeading
// that content outside the table (and other cells in the same table)
// relies on.
type cellState struct {
	table, row, col int
	colRepeat       int
	heading         string
}

// extractContentXML streams content.xml's office:text body, emitting one
// paragraphLocation-tagged unit per top-level body paragraph (text:p or
// text:h) and one cellLocation-tagged unit per non-blank table-cell
// paragraph, mirroring docx's document-body extraction (see that
// package's doc comment for the general rationale: Word/ODF navigation
// both work off headings, not raw paragraph/cell addresses, hence the
// same "nearest preceding heading" Human() choice here).
//
// Text capture uses docx's own two-part model (see
// internal/adapters/extract/docx/xmlstream.go's runTracker doc comment
// for the original rationale), ported to ODF's shape:
//
//  1. A whitelist (isTextContentElement) -- CharData only contributes to
//     the currently-open paragraph when its immediate enclosing element
//     is text:p/text:h/text:span/text:a, never "whatever happens to be
//     open while a paragraph is in progress".
//  2. An explicit subtree-suppression counter (suppressDepth,
//     isSuppressedSubtreeRoot) that, once inside a draw:frame,
//     office:annotation, text:note, office:binary-data, svg:title,
//     svg:desc, or text:deletion, ignores every token -- CharData *and*
//     structural elements like nested text:p/text:h -- until that
//     subtree closes, regardless of what's nested inside it. This is
//     what keeps a footnote citation number, a comment's author/date, an
//     image's alt-text or embedded binary data, and tracked-changes'
//     deleted text from leaking into the enclosing paragraph, AND keeps
//     the paragraphs nested inside those wrappers (a comment body, a
//     footnote body, deleted text formatted as its own text:p) from
//     being mistaken for real top-level content and shifting paragraph
//     numbering -- exactly docx's suppressDepth's job for w:drawing/
//     w:pict, just generalized to ODF's several distinct wrapper kinds.
//
// Heading tracking is scoped the same way: text:h found directly under
// office:text updates currentHeading (visible to every paragraph/cell
// after it, anywhere), but text:h found inside a table cell updates only
// that cellState's own heading field -- see cellState's doc comment.
//
// A text:line-break splits its enclosing paragraph into multiple
// TextUnits at that point (all sharing the paragraph's Location), for
// the same -A/-B/-C context-line reasons docx splits on w:br/w:cr.
//
// pDepth guards against a text:p/text:h nested inside another one --
// e.g. a draw:custom-shape's own text-box paragraph, sitting inside the
// body paragraph that anchors the shape -- ported from odp's identical
// guard (see that package's content.go), and for the identical reason:
// isSuppressedSubtreeRoot's allowlist of wrapper elements can't be
// exhaustive over every ODF shape type, so this is the general-purpose
// backstop. Only the outermost text:p/text:h on the current path
// (pDepth == 1) is treated as "the" paragraph being built -- eligible
// for CharData capture, tab/text:s/line-break handling, and its own
// flush/paragraphNum-increment when it closes; anything nested one (or
// more) levels deeper is walked only to keep elemStack/pDepth balanced,
// never touching curPara. Without this, a nested text:p's own
// StartElement would reassign curPara out from under the real,
// enclosing paragraph, and its own EndElement would flush that nested
// text as if it were real body content -- exactly the corruption (and,
// for a table cell whose paragraph anchors such a shape, complete loss
// of the cell's own real content) an earlier version of this package
// shipped before this guard was added.
func extractContentXML(f *zip.File, out send) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	dec := xml.NewDecoder(rc)

	var curPara *strings.Builder
	var pDepth int
	var suppressDepth int
	var elemStack []string

	var paragraphNum int
	var tableCounter int
	var tableStack []tableState
	var cellStack []*cellState
	aborted := false

	// Heading tracking: currentHeading is the nearest preceding text:h's
	// own text found OUTSIDE any table cell, reported by every
	// subsequent body paragraphLocation's Human() until the next such
	// heading replaces it (a heading found inside a cell never touches
	// this -- see cellState.heading). isHeadingPara/headingAccum track
	// whether the currently open paragraph is itself a heading and
	// accumulate its text as it streams in, so a heading that itself
	// matches the search pattern reports itself, not the previous
	// heading; flushSegment below decides whether that accumulated text
	// becomes the document-wide currentHeading or just the current
	// cell's local heading.
	var currentHeading string
	var isHeadingPara bool
	var headingAccum strings.Builder

	// currentCell returns the innermost real (non-placeholder) cellState
	// on cellStack, or nil if we're not inside a real cell -- see
	// cellStack's push/pop sites for why a placeholder nil entry can
	// legitimately be on top (a malformed table-cell with no enclosing
	// table).
	currentCell := func() *cellState {
		if n := len(cellStack); n > 0 {
			return cellStack[n-1]
		}
		return nil
	}

	flushSegment := func(text string) {
		if isHeadingPara {
			if headingAccum.Len() > 0 {
				headingAccum.WriteByte(' ')
			}
			headingAccum.WriteString(text)
			headingText := headingAccum.String()
			if cell := currentCell(); cell != nil {
				cell.heading = headingText
			} else {
				currentHeading = headingText
			}
		}

		if cell := currentCell(); cell != nil {
			if strings.TrimSpace(text) == "" {
				return
			}
			unit := domain.TextUnit{
				Location: cellLocation{Table: cell.table, Row: cell.row, Col: cell.col, Heading: cell.heading},
				Text:     text,
			}
			if !out(unit) {
				aborted = true
			}
		} else {
			unit := domain.TextUnit{
				Location: paragraphLocation{Paragraph: paragraphNum, Heading: currentHeading},
				Text:     text,
			}
			if !out(unit) {
				aborted = true
			}
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
			if isSuppressedSubtreeRoot(t.Name) {
				suppressDepth++
				continue
			}
			if suppressDepth > 0 {
				continue
			}
			switch t.Name.Local {
			case "p", "h":
				pDepth++
				if pDepth == 1 {
					curPara = &strings.Builder{}
					isHeadingPara = t.Name.Local == "h"
					headingAccum.Reset()
					if currentCell() == nil {
						paragraphNum++
					}
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
			case "table":
				tableCounter++
				tableStack = append(tableStack, tableState{num: tableCounter})
			case "table-row":
				if n := len(tableStack); n > 0 {
					tableStack[n-1].row++
					tableStack[n-1].col = 0
					tableStack[n-1].rowRepeat = attrIntDefault1(t, "number-rows-repeated")
				}
			case "table-cell", "covered-table-cell":
				if n := len(tableStack); n > 0 {
					repeat := attrIntDefault1(t, "number-columns-repeated")
					tableStack[n-1].col++
					top := tableStack[n-1]
					cellStack = append(cellStack, &cellState{table: top.num, row: top.row, col: top.col, colRepeat: repeat, heading: currentHeading})
				} else {
					// Malformed input: a table-cell with no enclosing
					// table. Push a nil placeholder anyway so this
					// element's matching end tag (guaranteed by
					// encoding/xml's own well-formedness checking to
					// arrive at the same nesting depth) pops exactly
					// this entry, rather than -- if nothing were pushed
					// here -- potentially popping an unrelated,
					// legitimately-open ancestor cell instead. See
					// currentCell, which already treats a nil top as
					// "not in a real cell".
					cellStack = append(cellStack, nil)
				}
			}
		case xml.EndElement:
			if n := len(elemStack); n > 0 {
				elemStack = elemStack[:n-1]
			}
			if isSuppressedSubtreeRoot(t.Name) {
				if suppressDepth > 0 {
					suppressDepth--
				}
				continue
			}
			if suppressDepth > 0 {
				continue
			}
			switch t.Name.Local {
			case "p", "h":
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
			case "table-cell", "covered-table-cell":
				if n := len(cellStack); n > 0 {
					cell := cellStack[n-1]
					cellStack = cellStack[:n-1]
					if cell != nil {
						if tn := len(tableStack); tn > 0 && cell.colRepeat > 1 {
							tableStack[tn-1].col += cell.colRepeat - 1
						}
					}
				}
			case "table-row":
				if n := len(tableStack); n > 0 && tableStack[n-1].rowRepeat > 1 {
					tableStack[n-1].row += tableStack[n-1].rowRepeat - 1
				}
			case "table":
				if n := len(tableStack); n > 0 {
					tableStack = tableStack[:n-1]
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
