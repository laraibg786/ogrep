package ods

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
// to 1 when absent or unparsable -- used for
// table:number-columns-repeated / table:number-rows-repeated.
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

// svgTitleDescNS lists the namespace URIs svg:title/svg:desc have
// actually been declared under across ODF's own history -- see odt's
// identical map (internal/adapters/extract/odt/content.go) for the
// full rationale (ODF 1.2+'s own "svg-compatible" URN vs. ODF 1.0/1.1's
// plain W3C SVG namespace) and why matching the full name, not just the
// local name, matters.
var svgTitleDescNS = map[string]bool{
	"urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0": true, // ODF 1.2+
	"http://www.w3.org/2000/svg":                               true, // ODF 1.0/1.1
}

// isSuppressedSubtreeRoot reports whether name starts a subtree whose
// entire contents must be excluded from whatever cell paragraph
// encloses it, regardless of what's nested inside -- the direct ODF
// analogue of docx's isDrawingWrapper + suppressDepth model (see
// internal/adapters/extract/docx/xmlstream.go), generalized here mainly
// for office:annotation: an ODS cell comment. Both its author/date
// metadata (dc:creator, dc:date) and its own comment-body text:p
// children must stay out of the cell's own extracted text -- a
// comment-only cell (no real table:table-cell content, just an
// annotation) must report no content at all, not the comment's text.
// draw:frame/office:binary-data/svg:title/svg:desc/text:note/
// text:deletion are included too, mirroring odt's identical list, since
// a spreadsheet cell can in principle anchor an image or carry the same
// tracked-changes/footnote-shaped metadata odt guards against. This
// list is not, and cannot practically be, exhaustive over every ODF
// drawing shape a cell could anchor (draw:custom-shape, draw:g, and
// others besides draw:frame) -- extractContentXML's pDepth guard is the
// general-purpose backstop for a nested text:p under any of those,
// known or not; see its doc comment.
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
// elements whose DIRECT xml.CharData children are genuine cell text:
// text:p itself, text:span (an inline formatting run), and text:a (an
// ODF hyperlink). See odt's identical helper for why a whitelist alone
// is not sufficient by itself -- office:annotation's own comment-body
// text:p is *also* text:p content, so only suppression (which looks at
// the whole ancestor chain) can tell it apart from the cell's own
// paragraph.
func isTextContentElement(localName string) bool {
	switch localName {
	case "p", "span", "a":
		return true
	}
	return false
}

// extractContentXML streams content.xml's office:spreadsheet body,
// emitting one cellLocation-tagged TextUnit per non-blank paragraph
// found inside a table:table-cell (a cell containing multiple text:p
// children -- e.g. a manually wrapped multi-line cell -- emits one unit
// per non-blank paragraph, all sharing that cell's Location, rather than
// joining them into a single unit with an embedded newline; see docx's
// package doc comment "Line splitting" for why that matters for -A/-B/-C
// context lines).
//
// Text capture uses docx's own two-part model (see
// internal/adapters/extract/docx/xmlstream.go's runTracker doc comment),
// ported to ODF's shape exactly the way odt's extractContentXML does --
// see that function's doc comment for the full rationale. In short: a
// whitelist (isTextContentElement) restricts CharData capture to
// text:p/text:span/text:a's direct children, and an explicit
// subtree-suppression counter (suppressDepth, isSuppressedSubtreeRoot)
// ignores everything -- CharData and structural elements alike -- for
// the rest of an office:annotation (or draw:frame/text:note/etc.)
// subtree once entered, regardless of what's nested inside. This is what
// keeps a cell comment's text (and its author/date) from being reported
// as if it were the cell's own value.
//
// table:number-columns-repeated / table:number-rows-repeated are ODF's
// own compact encoding for "this cell/row repeats N times" -- almost
// always used for a large trailing run of empty cells/rows rather than
// genuinely duplicated content. Repeated cells/rows are only extracted
// once (from the single XML element actually present), but the
// enclosing table's row/col cursor still advances by the full repeat
// count, so any real cell/row addressed after a repeated group still
// gets the column/row number a user opening the file in a spreadsheet
// app would see -- mirroring xlsx/sheet.go's equally careful treatment
// of its own analogous quirks (shared strings, empty cells). When the
// repeated cell/row itself carries real (non-blank) content, it is still
// reported only once, at its first cell/row reference -- see
// cellLocation.Repeat's doc comment and this package's README section
// for why, and for what a search misses as a result.
//
// pDepth guards against a text:p nested inside another one -- e.g. a
// draw:custom-shape's own text-box paragraph, sitting inside the cell
// paragraph that anchors the shape -- ported from odt/odp's identical
// guard (see odt's content.go for the fuller rationale): only the
// outermost text:p on the current path (pDepth == 1) is eligible for
// CharData capture and its own flush/emit when it closes; anything
// nested deeper is walked only to keep elemStack/pDepth balanced, never
// touching curPara. Without this, a shape's own nested paragraph could
// silently replace the cell's real content -- a comment-only cell
// aside, this was the one case where a cell's actual value could be
// lost entirely rather than merely mis-tagged.
func extractContentXML(f *zip.File, out send) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	dec := xml.NewDecoder(rc)

	var sheetName string
	var sheetCounter int
	inSheet := false

	var row, col int
	var rowRepeat int

	inCell := false
	var cellCol, cellColRepeat int

	var curPara *strings.Builder
	var pDepth int
	var suppressDepth int
	var elemStack []string
	aborted := false

	flushSegment := func(text string) {
		if !inCell || strings.TrimSpace(text) == "" {
			return
		}
		cellRef := colLetters(cellCol) + strconv.Itoa(row)
		repeat := cellColRepeat * rowRepeat
		if repeat < 1 {
			repeat = 1
		}
		unit := domain.TextUnit{
			Location: cellLocation{Sheet: sheetName, Cell: cellRef, Row: row, Col: cellCol, Repeat: repeat},
			Text:     text,
		}
		if !out(unit) {
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
			if isSuppressedSubtreeRoot(t.Name) {
				suppressDepth++
				continue
			}
			if suppressDepth > 0 {
				continue
			}
			switch t.Name.Local {
			case "table":
				sheetCounter++
				sheetName = attrValue(t, "name")
				if sheetName == "" {
					sheetName = fmt.Sprintf("Sheet%d", sheetCounter)
				}
				inSheet = true
				row = 0
			case "table-row":
				if inSheet {
					row++
					col = 0
					rowRepeat = attrIntDefault1(t, "number-rows-repeated")
				}
			case "table-cell", "covered-table-cell":
				if inSheet {
					col++
					cellCol = col
					cellColRepeat = attrIntDefault1(t, "number-columns-repeated")
					inCell = true
				}
			case "p":
				pDepth++
				if pDepth == 1 && inCell {
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
			case "table":
				inSheet = false
			case "table-row":
				if inSheet && rowRepeat > 1 {
					row += rowRepeat - 1
				}
			case "table-cell", "covered-table-cell":
				if inCell && cellColRepeat > 1 {
					col += cellColRepeat - 1
				}
				inCell = false
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
