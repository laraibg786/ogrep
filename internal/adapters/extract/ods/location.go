package ods

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// cellLocation implements domain.Location for a single non-empty
// spreadsheet cell, mirroring xlsx's cellLocation exactly (same shape,
// same Human/Fields/HyperlinkURI conventions) since ODS cells are
// addressed the same way Excel cells are (a sheet name plus an A1-style
// reference).
type cellLocation struct {
	Sheet, Cell string
	Row, Col    int
	// Repeat is how many times this exact cell/row combination actually
	// occurs in the document (table:number-columns-repeated times
	// table:number-rows-repeated), or 1 for an ordinary, non-repeated
	// cell. content.go's extractContentXML only ever reports a repeated
	// group once, at its first cell reference -- see that function's doc
	// comment for why (matching LibreOffice's common large-trailing-
	// blank-run usage) -- so Repeat is this extractor's one cheap way to
	// let a caller learn "this value actually appears N times" without
	// restructuring extraction to emit N separate units for what is, in
	// the overwhelmingly common case, a single blank padding run.
	Repeat int
}

// Human renders "Sheet:Cell" (colon-separated, matching the rest of
// ogrep's "path:location" display), same as xlsx.cellLocation.Human.
// Repeat is deliberately not part of Human's output: a console match
// line reads as one match at one location, and a ">1" repeat count is
// secondary information that belongs in Fields()/JSON, not clutter on
// every ordinary (non-repeated, Repeat == 1) match.
func (l cellLocation) Human() string { return fmt.Sprintf("%s:%s", l.Sheet, l.Cell) }

func (l cellLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"sheet": l.Sheet, "cell": l.Cell, "row": l.Row, "col": l.Col, "repeat": l.Repeat}
}

// spans is unused: a cell is already the finest-grained addressable
// target ODS has, regardless of where within its text a match starts.
func (l cellLocation) HyperlinkURI(path string, spans []domain.Span) string {
	return domain.FileURI(path, quotedSheetRef(l.Sheet)+"!"+l.Cell)
}

// quotedSheetRef renders a sheet name the way spreadsheet formula
// syntax does inside a reference: names containing anything other than
// letters, digits, or underscores must be wrapped in single quotes, with
// any embedded single quote doubled. Mirrors xlsx's identical helper.
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
