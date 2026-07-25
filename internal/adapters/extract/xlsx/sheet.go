package xlsx

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/xuri/excelize/v2"

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

// extractSheet streams one worksheet via excelize's Rows() iterator,
// emitting one domain.TextUnit per non-empty cell. Rows() reads the
// underlying xl/worksheets/sheetN.xml part row-by-row rather than
// unmarshaling it into a DOM, per the ports.DocumentExtractor streaming
// requirement (this is the one part of an xlsx file that can
// legitimately be huge -- 100k+ rows is an explicit target scenario).
func extractSheet(ctx context.Context, f *excelize.File, sheetName string, units chan<- domain.TextUnit) error {
	rows, err := f.Rows(sheetName)
	if err != nil {
		return err
	}
	defer rows.Close()

	rowNum := 0
	for rows.Next() {
		rowNum++
		if ctx.Err() != nil {
			return nil
		}
		cols, err := rows.Columns()
		if err != nil {
			return fmt.Errorf("row %d: %w", rowNum, err)
		}
		for colIdx, val := range cols {
			if val == "" {
				continue
			}
			col := colIdx + 1
			cellRef, err := excelize.CoordinatesToCellName(col, rowNum)
			if err != nil {
				continue
			}
			select {
			case units <- domain.TextUnit{
				Location: cellLocation{Sheet: sheetName, Cell: cellRef, Row: rowNum, Col: col},
				Text:     val,
			}:
			case <-ctx.Done():
				return nil
			}
		}
	}
	return rows.Error()
}
