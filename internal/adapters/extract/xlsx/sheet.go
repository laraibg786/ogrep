package xlsx

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// cellLocation implements domain.Location for a single non-empty
// worksheet cell.
type cellLocation struct {
	Sheet, Cell string
	Row, Col    int
}

// Human renders "Sheet:Cell" (colon-separated, matching the rest of
// ogrep's "path:location" display) rather than Excel's own "Sheet!Cell"
// formula syntax, which would visually clash with the path:location
// colon that already precedes it.
func (l cellLocation) Human() string { return fmt.Sprintf("%s:%s", l.Sheet, l.Cell) }

func (l cellLocation) Fields() map[string]any {
	return map[string]any{"sheet": l.Sheet, "cell": l.Cell, "row": l.Row, "col": l.Col}
}

func (l cellLocation) HyperlinkURI(path string) string {
	return domain.FileURI(path, quotedSheetRef(l.Sheet)+"!"+l.Cell)
}

// quotedSheetRef renders a sheet name the way Excel itself does inside
// a reference: names containing anything other than letters, digits,
// or underscores must be wrapped in single quotes, with any embedded
// single quote doubled (Excel's escape for a literal quote in a quoted
// sheet name).
func quotedSheetRef(sheet string) string {
	plain := true
	for _, r := range sheet {
		if !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
			plain = false
			break
		}
	}
	if plain {
		return sheet
	}
	return "'" + strings.ReplaceAll(sheet, "'", "''") + "'"
}

// extractSheet streams one worksheet part (xl/worksheets/sheetN.xml),
// emitting one domain.TextUnit per non-empty cell. It never buffers the
// sheet into a DOM: it walks encoding/xml tokens one at a time and
// reacts to <row>/<c> as they're seen, per the ports.DocumentExtractor
// streaming requirement (this is the one part of an xlsx file that can
// legitimately be huge -- 100k+ rows -- so an xml.Unmarshal here would
// defeat the whole point).
func extractSheet(ctx context.Context, f *zip.File, sheetName string, sharedStrings []string, units chan<- domain.TextUnit) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("opening %s: %w", f.Name, err)
	}
	defer rc.Close()

	dec := xml.NewDecoder(rc)

	// currentRow tracks the most recently seen <row r="..."> value, used
	// only as a fallback when a cell is missing its own r attribute
	// (malformed input) -- normally each cell's own reference (e.g.
	// "B45") is authoritative for both column and row.
	currentRow := 0
	// colCounter is a per-row fallback column counter, likewise only
	// used when a cell lacks a parsable r attribute.
	colCounter := 0

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("parsing %s: %w", f.Name, err)
		}

		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}

		switch se.Name.Local {
		case "row":
			colCounter = 0
			if r := attrLocal(se.Attr, "r"); r != "" {
				if n, err := strconv.Atoi(r); err == nil {
					currentRow = n
				}
			} else {
				currentRow++
			}

		case "c":
			cellRef := attrLocal(se.Attr, "r")
			cellType := attrLocal(se.Attr, "t")

			v, isText, formulaSrc, err := parseCellBody(dec)
			if err != nil {
				return fmt.Errorf("parsing %s: %w", f.Name, err)
			}
			// formulaSrc (the <f> child, e.g. "SUM(A1:A10)") is
			// intentionally kept but unused for v1: we search the
			// cell's cached/displayed value, not its formula source.
			// Retained here so a future --search-formulas flag can
			// extend this without re-plumbing the cell parser.
			_ = formulaSrc

			text := resolveCellText(cellType, v, isText, sharedStrings)
			if text == "" {
				continue
			}

			col, row, refOK := parseCellRef(cellRef)
			if refOK {
				colCounter = col
			} else {
				colCounter++
				col = colCounter
				row = currentRow
				cellRef = colLetters(col) + strconv.Itoa(row)
			}

			unit := domain.TextUnit{
				Location: cellLocation{Sheet: sheetName, Cell: cellRef, Row: row, Col: col},
				Text:     text,
			}
			select {
			case units <- unit:
			case <-ctx.Done():
				return nil
			}
		}
	}
	return nil
}

// attrLocal looks up an attribute by its local name (ignoring
// namespace), matching how we resolve cell/row references and types.
func attrLocal(attrs []xml.Attr, local string) string {
	for _, a := range attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// parseCellBody consumes tokens for one <c>...</c> element (whose
// StartElement has already been read by the caller) and returns the raw
// text of its <v>, <is>, and <f> children, whichever are present. It
// tolerates and skips any other/unknown child elements (e.g. an
// <extLst> extension block) so unrecognized-but-well-formed content
// doesn't abort the whole sheet.
func parseCellBody(dec *xml.Decoder) (v, isText, formulaSrc string, err error) {
	for {
		tok, terr := dec.Token()
		if terr != nil {
			return v, isText, formulaSrc, terr
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "v":
				v, err = captureText(dec)
			case "is":
				isText, err = captureRichText(dec)
			case "f":
				formulaSrc, err = captureText(dec)
			default:
				err = skipElement(dec)
			}
			if err != nil {
				return v, isText, formulaSrc, err
			}
		case xml.EndElement:
			if t.Name.Local == "c" {
				return v, isText, formulaSrc, nil
			}
		}
	}
}

// resolveCellText renders a cell's final searchable text given its
// OOXML cell type (t attribute) and raw child content. Judgment calls:
//   - t="s" (shared string): v holds an integer index into
//     sharedStrings; an out-of-range or non-numeric index is treated as
//     empty rather than erroring the whole sheet, since one malformed
//     cell shouldn't sink extraction of the rest of a large sheet.
//   - t="b" (boolean): rendered as the literal words "TRUE"/"FALSE"
//     (rather than "1"/"0") since that's what a user visually sees in
//     Excel and is what they're most likely to grep for.
//   - t="str"/"e"/"n"/"" (formula-string result, error code, numeric,
//     or the default/untyped numeric case): v is used verbatim -- in
//     all of these cases v already holds the literal display text, not
//     an index.
//   - t="inlineStr": isText (from <is>) is used instead of v.
//   - any other/unrecognized t: fall back to v, on the theory that an
//     unknown-but-present <v> is more useful to surface than nothing.
func resolveCellText(cellType, v, isText string, sharedStrings []string) string {
	switch cellType {
	case "s":
		idx, err := strconv.Atoi(v)
		if err != nil || idx < 0 || idx >= len(sharedStrings) {
			return ""
		}
		return sharedStrings[idx]
	case "inlineStr":
		return isText
	case "b":
		switch v {
		case "1":
			return "TRUE"
		case "0":
			return "FALSE"
		default:
			return v
		}
	default:
		return v
	}
}

// captureText concatenates all character data found until the
// currently-open element (whose StartElement was already consumed by
// the caller) closes, regardless of any further nesting inside it. Used
// for <v> and <f>, which are not expected to nest but are handled
// depth-safely regardless.
func captureText(dec *xml.Decoder) (string, error) {
	var sb []byte
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return string(sb), err
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb = append(sb, t...)
		case xml.StartElement:
			depth++
		case xml.EndElement:
			if depth == 0 {
				return string(sb), nil
			}
			depth--
		}
	}
}

// captureRichText concatenates only the text found inside <t>
// descendant elements until the currently-open element (already
// consumed by the caller, e.g. <si> or <is>) closes. This handles both
// shapes of shared/inline strings: a single direct <t>text</t> child,
// or multiple <r><t>...</t></r> rich-text runs whose text must be
// joined in document order while skipping non-text sibling content
// (e.g. <rPr> run-formatting metadata, and incidental whitespace
// between tags in pretty-printed XML).
func captureRichText(dec *xml.Decoder) (string, error) {
	var sb []byte
	inT := false
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return string(sb), err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" {
				inT = true
			}
			depth++
		case xml.CharData:
			if inT {
				sb = append(sb, t...)
			}
		case xml.EndElement:
			if t.Name.Local == "t" {
				inT = false
			}
			if depth == 0 {
				return string(sb), nil
			}
			depth--
		}
	}
}

// skipElement discards tokens through the matching end of the
// currently-open element (already consumed by the caller), for
// child elements we don't otherwise recognize.
func skipElement(dec *xml.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			if depth == 0 {
				return nil
			}
			depth--
		}
	}
}
