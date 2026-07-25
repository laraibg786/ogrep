package docx

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// paragraphLocation implements domain.Location for a top-level body
// paragraph.
type paragraphLocation struct {
	Paragraph int
}

func (l paragraphLocation) Human() string { return fmt.Sprintf("Paragraph %d", l.Paragraph) }

func (l paragraphLocation) Fields() map[string]any {
	return map[string]any{"paragraph": l.Paragraph}
}

// HyperlinkURI opens the file with no location fragment: Word only
// navigates a file:// fragment to a real bookmark already present in
// the document, so a paragraph number can't jump there and is omitted
// rather than looking clickable while silently doing nothing.
func (l paragraphLocation) HyperlinkURI(path string) string {
	return domain.FileURI(path, "")
}

// cellLocation implements domain.Location for a table cell.
type cellLocation struct {
	Table, Row, Col int
}

func (l cellLocation) Human() string {
	return fmt.Sprintf("Table %d, Row %d, Cell %d", l.Table, l.Row, l.Col)
}

func (l cellLocation) Fields() map[string]any {
	return map[string]any{"table": l.Table, "row": l.Row, "col": l.Col}
}

// HyperlinkURI opens the file with no location fragment; see
// paragraphLocation.HyperlinkURI for why a table/row/cell address can't
// be expressed as a navigable fragment in Word.
func (l cellLocation) HyperlinkURI(path string) string {
	return domain.FileURI(path, "")
}

// headerFooterLocation implements domain.Location for a paragraph found
// in a header or footer part, fully described by a pre-rendered label
// ("Header 1", "Footer 2").
type headerFooterLocation struct {
	Label string
}

func (l headerFooterLocation) Human() string { return l.Label }

// Fields exposes "label" since the JSON sink no longer emits a generic
// human-readable location string; the label ("Header 1", "Footer 2") is
// this location's only identifying information.
func (l headerFooterLocation) Fields() map[string]any { return map[string]any{"label": l.Label} }

// HyperlinkURI returns "": Word has no addressable location for headers
// and footers.
func (l headerFooterLocation) HyperlinkURI(path string) string { return "" }

// footnoteLocation implements domain.Location for one footnote, fully
// described by a pre-rendered label ("Footnote 3").
type footnoteLocation struct {
	Label string
}

func (l footnoteLocation) Human() string { return l.Label }

// Fields exposes "label" ("Footnote 3"); see headerFooterLocation.Fields.
func (l footnoteLocation) Fields() map[string]any { return map[string]any{"label": l.Label} }

// HyperlinkURI returns "": Word has no addressable location for
// footnotes.
func (l footnoteLocation) HyperlinkURI(path string) string { return "" }

// commentLocation implements domain.Location for one comment, fully
// described by a pre-rendered label ("Comment 2").
type commentLocation struct {
	Label string
}

func (l commentLocation) Human() string { return l.Label }

// Fields exposes "label" ("Comment 2"); see headerFooterLocation.Fields.
func (l commentLocation) Fields() map[string]any { return map[string]any{"label": l.Label} }

// HyperlinkURI returns "": Word has no addressable location for
// comments.
func (l commentLocation) HyperlinkURI(path string) string { return "" }

// send is the callback shape used throughout this file: it delivers one
// TextUnit and reports whether the caller should keep going (false means
// the context was cancelled, so the streaming loop should stop early
// without treating that as an error).
type send func(domain.TextUnit) bool

// readCharData accumulates character data up to (and consuming) the
// matching end element for the currently-open leaf element (identified
// by localName, e.g. "t"). Real WordprocessingML w:t elements never
// contain nested elements, so this simple loop — collect CharData,
// stop at the first EndElement — is sufficient without tracking depth.
func readCharData(dec *xml.Decoder, localName string) (string, error) {
	var sb strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.EndElement:
			if t.Name.Local == localName {
				return sb.String(), nil
			}
		}
	}
}

// attrValue returns the value of the first attribute on t whose local
// name matches, ignoring namespace, or "" if absent.
func attrValue(t xml.StartElement, localName string) string {
	for _, a := range t.Attr {
		if a.Name.Local == localName {
			return a.Value
		}
	}
	return ""
}

// isDrawingWrapper reports whether localName is an element that can wrap
// "floating" nested content with its own independent w:p/w:r/w:t
// structure — most commonly a text box (w:drawing > ... > w:txbxContent
// > w:p, or the legacy VML equivalent w:pict > v:shape > v:textbox >
// w:txbxContent > w:p). Content inside one of these must not be allowed
// to interact with the paragraph/run/table state of whatever paragraph
// happens to enclose the drawing — see suppressDepth on runTracker.
func isDrawingWrapper(localName string) bool {
	return localName == "drawing" || localName == "pict"
}

// runTracker holds the small bit of state needed to build up the text of
// one in-progress paragraph while streaming: whether we're currently
// inside a w:r (run) — which is what makes w:t/w:tab/w:br/w:cr
// meaningful — the builder for the paragraph currently open (nil when
// we're not inside a w:p at all), and suppressDepth, which counts how
// many nested w:drawing/w:pict wrappers we're currently inside.
//
// Word can nest an entire independent w:p/w:r/w:t structure inside a
// text box, itself reached via w:r > w:drawing > ... > w:txbxContent >
// w:p. Text boxes are common (flyers, forms, templates), not exotic.
// Without suppression, the nested w:p's StartElement would blow away
// the enclosing (real) paragraph's in-progress curPara builder before
// the enclosing w:p closes, silently discarding its accumulated text,
// emitting a spurious extra (usually empty) paragraph, and shifting
// every subsequent paragraph's number by one — silent data loss, not a
// crash. While suppressDepth > 0, every content-affecting element
// (p/tbl/tr/tc/r/t/tab/br/cr) is ignored outright: neither curPara nor
// the table/cell stacks are touched, so the enclosing paragraph's text
// and the document's paragraph/table numbering come out exactly as if
// the drawing were never there. (This intentionally means text-box
// content itself is not extracted as separate searchable text; adding
// that is possible but out of scope for this fix, which is only about
// not corrupting the surrounding document.)
type runTracker struct {
	inRun         bool
	curPara       *strings.Builder
	suppressDepth int
}

// suppressed reports whether we're currently inside a w:drawing/w:pict
// wrapper and should ignore content-affecting elements.
func (rt *runTracker) suppressed() bool { return rt.suppressDepth > 0 }

// enterDrawing/exitDrawing adjust suppressDepth on w:drawing/w:pict
// start/end; callers should invoke these unconditionally (regardless of
// current suppression state) so nested drawings are tracked correctly.
func (rt *runTracker) enterDrawing() { rt.suppressDepth++ }
func (rt *runTracker) exitDrawing() {
	if rt.suppressDepth > 0 {
		rt.suppressDepth--
	}
}

// handleStart processes a StartElement that is one of the "leaf content"
// elements common to every part (t/tab/br/cr/r), given the current
// paragraph state. It returns true if it consumed the token itself
// (i.e. the caller doesn't need to do anything else for it).
func (rt *runTracker) handleStart(dec *xml.Decoder, t xml.StartElement) error {
	switch t.Name.Local {
	case "r":
		rt.inRun = true
	case "t":
		if rt.inRun && rt.curPara != nil {
			txt, err := readCharData(dec, "t")
			if err != nil {
				return err
			}
			rt.curPara.WriteString(txt)
		}
	case "tab":
		// Only a run-content w:tab (an actual inserted tab character)
		// should be honored here. A w:tab can also appear inside
		// w:pPr/w:tabs as a tab-STOP definition, which is unrelated to
		// inserted text — tracking inRun (only true between w:r start
		// and end) is what keeps that case from being misread.
		if rt.inRun && rt.curPara != nil {
			rt.curPara.WriteString("\t")
		}
	case "br", "cr":
		if rt.inRun && rt.curPara != nil {
			rt.curPara.WriteString("\n")
		}
	}
	return nil
}

func (rt *runTracker) handleEnd(t xml.EndElement) {
	if t.Name.Local == "r" {
		rt.inRun = false
	}
}

// A note on OMML math content: embedded equations use m:oMath/
// m:oMathPara containing m:r/m:t runs, and since Local name stripping
// discards the namespace prefix, our "r"/"t" handling above matches
// m:r/m:t exactly the same as w:r/w:t. This was deliberately checked,
// not just assumed safe: unlike w:txbxContent, OMML never nests a w:p
// inside m:r, so there's no equivalent of the text-box bug here — an
// m:r/m:t pair just contributes its literal text to whichever
// paragraph's builder is currently open, which is the desired behavior
// (embedded formula text becomes searchable, same as any other run
// text). No suppression is needed for math content.

// tableState tracks the current row/col cursor for one open w:tbl.
type tableState struct {
	num, row, col int
}

// cellState accumulates the (possibly multi-paragraph) text of one open
// w:tc, to be emitted as a single TextUnit when the cell closes.
type cellState struct {
	table, row, col int
	builder         strings.Builder
}

// extractDocumentBody streams word/document.xml, emitting one
// paragraphLocation-tagged unit per top-level body paragraph and one
// cellLocation-tagged unit per non-blank table cell (concatenating all
// of the cell's paragraphs). See the package doc comment for the
// numbering and double-counting rules this implements.
func extractDocumentBody(f *zip.File, out send) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	dec := xml.NewDecoder(rc)
	rt := &runTracker{}

	var paragraphNum int
	var tableCounter int
	var tableStack []tableState
	var cellStack []*cellState

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("decoding word/document.xml: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if isDrawingWrapper(t.Name.Local) {
				rt.enterDrawing()
				continue
			}
			if rt.suppressed() {
				continue
			}
			switch t.Name.Local {
			case "p":
				rt.curPara = &strings.Builder{}
			case "tbl":
				tableCounter++
				tableStack = append(tableStack, tableState{num: tableCounter})
			case "tr":
				if n := len(tableStack); n > 0 {
					tableStack[n-1].row++
					tableStack[n-1].col = 0
				}
			case "tc":
				if n := len(tableStack); n > 0 {
					tableStack[n-1].col++
					top := tableStack[n-1]
					cellStack = append(cellStack, &cellState{table: top.num, row: top.row, col: top.col})
				}
			default:
				if err := rt.handleStart(dec, t); err != nil {
					return fmt.Errorf("decoding word/document.xml: %w", err)
				}
			}
		case xml.EndElement:
			if isDrawingWrapper(t.Name.Local) {
				rt.exitDrawing()
				continue
			}
			if rt.suppressed() {
				continue
			}
			switch t.Name.Local {
			case "p":
				text := ""
				if rt.curPara != nil {
					text = rt.curPara.String()
				}
				rt.curPara = nil
				if n := len(cellStack); n > 0 {
					cell := cellStack[n-1]
					if cell.builder.Len() > 0 {
						cell.builder.WriteString("\n")
					}
					cell.builder.WriteString(text)
				} else {
					paragraphNum++
					unit := domain.TextUnit{
						Location: paragraphLocation{Paragraph: paragraphNum},
						Text:     text,
					}
					if !out(unit) {
						return nil
					}
				}
			case "tc":
				if n := len(cellStack); n > 0 {
					cell := cellStack[n-1]
					cellStack = cellStack[:n-1]
					text := cell.builder.String()
					if strings.TrimSpace(text) != "" {
						unit := domain.TextUnit{
							Location: cellLocation{Table: cell.table, Row: cell.row, Col: cell.col},
							Text:     text,
						}
						if !out(unit) {
							return nil
						}
					}
				}
			case "tbl":
				if n := len(tableStack); n > 0 {
					tableStack = tableStack[:n-1]
				}
			default:
				rt.handleEnd(t)
			}
		}
	}

	return nil
}

// extractHeaderFooterPart streams a word/header*.xml or word/footer*.xml
// part, emitting one headerFooterLocation-tagged unit per non-blank
// paragraph, all sharing the given label (e.g. "Header 1"). Table
// structure inside headers/footers is deliberately not tracked; see the
// package doc comment for why that's safe here.
func extractHeaderFooterPart(f *zip.File, label string, out send) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	dec := xml.NewDecoder(rc)
	rt := &runTracker{}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("decoding %s: %w", f.Name, err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if isDrawingWrapper(t.Name.Local) {
				rt.enterDrawing()
				continue
			}
			if rt.suppressed() {
				continue
			}
			switch t.Name.Local {
			case "p":
				rt.curPara = &strings.Builder{}
			default:
				if err := rt.handleStart(dec, t); err != nil {
					return fmt.Errorf("decoding %s: %w", f.Name, err)
				}
			}
		case xml.EndElement:
			if isDrawingWrapper(t.Name.Local) {
				rt.exitDrawing()
				continue
			}
			if rt.suppressed() {
				continue
			}
			switch t.Name.Local {
			case "p":
				text := ""
				if rt.curPara != nil {
					text = rt.curPara.String()
				}
				rt.curPara = nil
				if strings.TrimSpace(text) != "" {
					unit := domain.TextUnit{
						Location: headerFooterLocation{Label: label},
						Text:     text,
					}
					if !out(unit) {
						return nil
					}
				}
			default:
				rt.handleEnd(t)
			}
		}
	}

	return nil
}

// extractFootnoteLike streams word/footnotes.xml or word/comments.xml,
// where elemName is "footnote" or "comment" respectively: each such
// element becomes one TextUnit whose Location is built by newLoc,
// concatenating all of its paragraphs (joined by "\n"). Location.Human
// uses the element's own w:id attribute ("Footnote 3"/"Comment 2"),
// falling back to a running counter if that attribute is missing or
// unparsable as a plain string (in practice w:id is always present on
// real documents; the fallback only guards against malformed input).
func extractFootnoteLike(f *zip.File, elemName string, newLoc func(label string) domain.Location, labelPrefix string, out send) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	dec := xml.NewDecoder(rc)
	rt := &runTracker{}

	var inItem bool
	var itemBuilder strings.Builder
	var itemID string
	var counter int

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("decoding %s: %w", f.Name, err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if isDrawingWrapper(t.Name.Local) {
				rt.enterDrawing()
				continue
			}
			if rt.suppressed() {
				continue
			}
			switch t.Name.Local {
			case elemName:
				inItem = true
				itemBuilder.Reset()
				counter++
				itemID = attrValue(t, "id")
				if itemID == "" {
					itemID = fmt.Sprintf("%d", counter)
				}
			case "p":
				if inItem {
					rt.curPara = &strings.Builder{}
				}
			default:
				if err := rt.handleStart(dec, t); err != nil {
					return fmt.Errorf("decoding %s: %w", f.Name, err)
				}
			}
		case xml.EndElement:
			if isDrawingWrapper(t.Name.Local) {
				rt.exitDrawing()
				continue
			}
			if rt.suppressed() {
				continue
			}
			switch t.Name.Local {
			case "p":
				if inItem && rt.curPara != nil {
					if itemBuilder.Len() > 0 {
						itemBuilder.WriteString("\n")
					}
					itemBuilder.WriteString(rt.curPara.String())
					rt.curPara = nil
				}
			case elemName:
				if inItem {
					text := itemBuilder.String()
					inItem = false
					if strings.TrimSpace(text) != "" {
						unit := domain.TextUnit{
							Location: newLoc(fmt.Sprintf("%s %s", labelPrefix, itemID)),
							Text:     text,
						}
						if !out(unit) {
							return nil
						}
					}
				}
			default:
				rt.handleEnd(t)
			}
		}
	}

	return nil
}
