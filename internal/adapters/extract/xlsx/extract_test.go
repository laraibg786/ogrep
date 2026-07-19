package xlsx_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/laraibg786/ogrep/internal/adapters/extract/xlsx"
	"github.com/laraibg786/ogrep/internal/core/domain"
)

// collectUnits runs Extract to completion (with a generous timeout so a
// bug that blocks forever fails the test instead of hanging the suite)
// and returns every emitted unit plus the first extraction error, if
// any.
func collectUnits(t *testing.T, data []byte) ([]domain.TextUnit, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	e := xlsx.Extractor{}
	ra := bytes.NewReader(data)
	unitsCh, errc := e.Extract(ctx, ra, int64(len(data)))

	var units []domain.TextUnit
	for u := range unitsCh {
		units = append(units, u)
	}
	err := <-errc
	if ctx.Err() != nil {
		t.Fatalf("Extract() did not complete within the test timeout")
	}
	return units, err
}

func findUnit(t *testing.T, units []domain.TextUnit, human string) domain.TextUnit {
	t.Helper()
	for _, u := range units {
		if u.Location.Human() == human {
			return u
		}
	}
	t.Fatalf("no unit found with Location.Human() = %q (got %d units)", human, len(units))
	return domain.TextUnit{}
}

func hasUnit(units []domain.TextUnit, human string) bool {
	for _, u := range units {
		if u.Location.Human() == human {
			return true
		}
	}
	return false
}

// multiSheetFixture builds a workbook deliberately exercising the
// indirection between a sheet's declared name/order and its underlying
// part file: "Sheet1" is declared first but lives in sheet2.xml via
// rId2, and "Budget 2024" is declared second but lives in sheet3.xml
// via rId1 -- i.e. neither declaration order nor a naive "sheetN.xml
// matches position N" assumption would resolve correctly. It also
// exercises every cell type the spec calls out.
func multiSheetFixture() map[string]string {
	workbookXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <sheets>
    <sheet name="Sheet1" sheetId="1" r:id="rId2"/>
    <sheet name="Budget 2024" sheetId="2" r:id="rId1"/>
  </sheets>
</workbook>`

	workbookRelsXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet3.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet2.xml"/>
</Relationships>`

	sharedStringsXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="2" uniqueCount="2">
  <si><r><t>Hello</t></r><r><t> World</t></r></si>
  <si><t>Second Sheet Value</t></si>
</sst>`

	// Sheet1's data, despite being declared first, lives in sheet2.xml.
	sheet2XML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <sheetData>
    <row r="1">
      <c r="A1" t="s"><v>0</v></c>
    </row>
  </sheetData>
</worksheet>`

	// Budget 2024's data, despite being declared second, lives in
	// sheet3.xml, and exercises every cell type.
	sheet3XML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <sheetData>
    <row r="1">
      <c r="C1" t="str"><f>A1&amp;B1</f><v>FormulaResult</v></c>
      <c r="D1"><v>3.14</v></c>
      <c r="E1" t="b"><v>1</v></c>
      <c r="F1" t="b"><v>0</v></c>
      <c r="G1" t="e"><v>#DIV/0!</v></c>
      <c r="H1" t="inlineStr"><is><t>Inline text</t></is></c>
      <c r="I1"/>
    </row>
    <row r="45">
      <c r="B45" t="s"><v>1</v></c>
    </row>
  </sheetData>
</worksheet>`

	return map[string]string{
		"[Content_Types].xml":        contentTypesXML,
		"_rels/.rels":                rootRelsXML,
		"xl/workbook.xml":            workbookXML,
		"xl/_rels/workbook.xml.rels": workbookRelsXML,
		"xl/sharedStrings.xml":       sharedStringsXML,
		"xl/worksheets/sheet2.xml":   sheet2XML,
		"xl/worksheets/sheet3.xml":   sheet3XML,
	}
}

func TestExtractResolvesSheetNamesViaRels(t *testing.T) {
	data := buildXlsx(t, multiSheetFixture())
	units, err := collectUnits(t, data)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}

	// "Sheet1" (declared first, r:id=rId2) must resolve to sheet2.xml's
	// content, not sheet1.xml (which doesn't even exist in this
	// fixture) and not sheet3.xml (Budget 2024's actual part).
	u := findUnit(t, units, "Sheet1!A1")
	if u.Text != "Hello World" {
		t.Errorf("Sheet1!A1 text = %q, want %q (multi-run shared string)", u.Text, "Hello World")
	}
	fields := u.Location.Fields()
	if fields["sheet"] != "Sheet1" || fields["cell"] != "A1" {
		t.Errorf("Sheet1!A1 Fields = %+v", fields)
	}
	if fields["col"] != 1 || fields["row"] != 1 {
		t.Errorf("Sheet1!A1 Col/Row = %v/%v, want 1/1", fields["col"], fields["row"])
	}
	// "Budget 2024" (declared second, r:id=rId1) must resolve to
	// sheet3.xml's content, with the correctly-mapped shared string.
	b := findUnit(t, units, "Budget 2024!B45")
	if b.Text != "Second Sheet Value" {
		t.Errorf("Budget 2024!B45 text = %q, want %q", b.Text, "Second Sheet Value")
	}
	bFields := b.Location.Fields()
	if bFields["col"] != 2 || bFields["row"] != 45 {
		t.Errorf("Budget 2024!B45 Col/Row = %v/%v, want 2/45", bFields["col"], bFields["row"])
	}
}

func TestExtractFormulaResultUsesCachedValue(t *testing.T) {
	data := buildXlsx(t, multiSheetFixture())
	units, err := collectUnits(t, data)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	u := findUnit(t, units, "Budget 2024!C1")
	if u.Text != "FormulaResult" {
		t.Errorf("Budget 2024!C1 text = %q, want the cached <v> value %q, not the formula source", u.Text, "FormulaResult")
	}
}

func TestExtractNumericCell(t *testing.T) {
	data := buildXlsx(t, multiSheetFixture())
	units, err := collectUnits(t, data)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	u := findUnit(t, units, "Budget 2024!D1")
	if u.Text != "3.14" {
		t.Errorf("Budget 2024!D1 text = %q, want %q", u.Text, "3.14")
	}
}

func TestExtractBooleanCells(t *testing.T) {
	data := buildXlsx(t, multiSheetFixture())
	units, err := collectUnits(t, data)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	// Judgment call: booleans render as the literal words TRUE/FALSE
	// (what a user visually sees in Excel), not "1"/"0".
	trueU := findUnit(t, units, "Budget 2024!E1")
	if trueU.Text != "TRUE" {
		t.Errorf("Budget 2024!E1 text = %q, want %q", trueU.Text, "TRUE")
	}
	falseU := findUnit(t, units, "Budget 2024!F1")
	if falseU.Text != "FALSE" {
		t.Errorf("Budget 2024!F1 text = %q, want %q", falseU.Text, "FALSE")
	}
}

func TestExtractErrorCell(t *testing.T) {
	data := buildXlsx(t, multiSheetFixture())
	units, err := collectUnits(t, data)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	u := findUnit(t, units, "Budget 2024!G1")
	if u.Text != "#DIV/0!" {
		t.Errorf("Budget 2024!G1 text = %q, want %q", u.Text, "#DIV/0!")
	}
}

func TestExtractInlineStringCell(t *testing.T) {
	data := buildXlsx(t, multiSheetFixture())
	units, err := collectUnits(t, data)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	u := findUnit(t, units, "Budget 2024!H1")
	if u.Text != "Inline text" {
		t.Errorf("Budget 2024!H1 text = %q, want %q", u.Text, "Inline text")
	}
}

func TestExtractSkipsEmptyCells(t *testing.T) {
	data := buildXlsx(t, multiSheetFixture())
	units, err := collectUnits(t, data)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if hasUnit(units, "Budget 2024!I1") {
		t.Error("expected no unit for empty cell I1, but one was emitted")
	}
}

func TestExtractContextCancellationStopsCleanly(t *testing.T) {
	data := buildXlsx(t, multiSheetFixture())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before we even start reading

	e := xlsx.Extractor{}
	ra := bytes.NewReader(data)
	unitsCh, errc := e.Extract(ctx, ra, int64(len(data)))

	done := make(chan struct{})
	go func() {
		for range unitsCh {
		}
		<-errc
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Extract() did not close its channels promptly after ctx cancellation")
	}
}
