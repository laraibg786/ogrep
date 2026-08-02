package ods

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// buildOds assembles a minimal, but real, in-memory zip archive with the
// given named parts plus a mimetype entry, exercising only our own
// zip/XML streaming code (not a LibreOffice-round-trippable file).
func buildOds(t *testing.T, mimetype string, parts map[string]string) []byte {
	t.Helper()

	all := map[string]string{
		"META-INF/manifest.xml": `<?xml version="1.0" encoding="UTF-8"?>` +
			`<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0"></manifest:manifest>`,
	}
	if mimetype != "" {
		all["mimetype"] = mimetype
	}
	for name, content := range parts {
		all[name] = content
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range all {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("creating zip part %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("writing zip part %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip writer: %v", err)
	}
	return buf.Bytes()
}

const odsNS = `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" ` +
	`xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" ` +
	`xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0"`

func wrapContent(body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><office:document-content ` + odsNS + `>` +
		`<office:body><office:spreadsheet>` + body + `</office:spreadsheet></office:body></office:document-content>`
}

func extractAll(t *testing.T, data []byte) []domain.TextUnit {
	t.Helper()
	r := bytes.NewReader(data)
	units, errc := (Extractor{}).Extract(context.Background(), r, int64(len(data)))

	var got []domain.TextUnit
	for u := range units {
		got = append(got, u)
	}
	if err := <-errc; err != nil {
		t.Fatalf("unexpected extraction error: %v", err)
	}
	return got
}

func TestLocationHyperlinkURIAndHuman(t *testing.T) {
	loc := cellLocation{Sheet: "Sheet1", Cell: "B45", Row: 45, Col: 2}
	if got, want := loc.Human(), "Sheet1:B45"; got != want {
		t.Errorf("Human() = %q, want %q", got, want)
	}
	if got, want := loc.HyperlinkURI("/path/doc.ods", nil), "file:///path/doc.ods#Sheet1!B45"; got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

func TestQuotedSheetRefWrapsSpecialNames(t *testing.T) {
	loc := cellLocation{Sheet: "My Sheet", Cell: "A1", Row: 1, Col: 1}
	got := loc.HyperlinkURI("/x.ods", nil)
	want := "file:///x.ods#" + "%27My%20Sheet%27!A1"
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

func TestSniffAcceptsOdsViaMimetype(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(`<table:table table:name="Sheet1"></table:table>`),
	})
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.ods", r, int64(len(data))) {
		t.Error("expected Sniff to accept an ods via its mimetype part")
	}
}

func TestSniffRejectsOdtViaMimetype(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(`<table:table table:name="Sheet1"></table:table>`),
	})
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.ods", r, int64(len(data))) {
		t.Error("expected Sniff to reject an odt package even though it has a content.xml")
	}
}

func TestSniffFallsBackToBodyKindWithoutMimetype(t *testing.T) {
	data := buildOds(t, "", map[string]string{
		"content.xml": wrapContent(`<table:table table:name="Sheet1"></table:table>`),
	})
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.ods", r, int64(len(data))) {
		t.Error("expected Sniff to accept an ods lacking a mimetype part via office:body's child element")
	}
}

func TestSniffRejectsNonZipGarbage(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x00, 0xff, 0xfe}
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.ods", r, int64(len(data))) {
		t.Error("expected Sniff to reject non-zip garbage, not panic")
	}
}

func TestSniffRejectsEmptyFile(t *testing.T) {
	r := bytes.NewReader(nil)
	if (Extractor{}).Sniff("file.ods", r, 0) {
		t.Error("expected Sniff to reject an empty file")
	}
}

func TestExtractBasicCells(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(
			`<table:table table:name="Sheet1">` +
				`<table:table-row><table:table-cell><text:p>A1</text:p></table:table-cell><table:table-cell><text:p>B1</text:p></table:table-cell></table:table-row>` +
				`<table:table-row><table:table-cell><text:p>A2</text:p></table:table-cell></table:table-row>` +
				`</table:table>`),
	})

	got := extractAll(t, data)
	if len(got) != 3 {
		t.Fatalf("got %d units, want 3: %+v", len(got), got)
	}
	want := []cellLocation{
		{Sheet: "Sheet1", Cell: "A1", Row: 1, Col: 1, Repeat: 1},
		{Sheet: "Sheet1", Cell: "B1", Row: 1, Col: 2, Repeat: 1},
		{Sheet: "Sheet1", Cell: "A2", Row: 2, Col: 1, Repeat: 1},
	}
	for i, u := range got {
		if u.Text != want[i].Cell {
			t.Errorf("unit %d text = %q, want %q", i, u.Text, want[i].Cell)
		}
		if u.Location != want[i] {
			t.Errorf("unit %d location = %+v, want %+v", i, u.Location, want[i])
		}
	}
}

func TestExtractDefaultSheetNameWhenMissing(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(
			`<table:table><table:table-row><table:table-cell><text:p>X</text:p></table:table-cell></table:table-row></table:table>`),
	})

	got := extractAll(t, data)
	if len(got) != 1 {
		t.Fatalf("got %d units, want 1: %+v", len(got), got)
	}
	if got[0].Location.(cellLocation).Sheet != "Sheet1" {
		t.Errorf("sheet = %q, want %q", got[0].Location.(cellLocation).Sheet, "Sheet1")
	}
}

func TestExtractMultipleSheets(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(
			`<table:table table:name="First"><table:table-row><table:table-cell><text:p>a</text:p></table:table-cell></table:table-row></table:table>` +
				`<table:table table:name="Second"><table:table-row><table:table-cell><text:p>b</text:p></table:table-cell></table:table-row></table:table>`),
	})

	got := extractAll(t, data)
	if len(got) != 2 {
		t.Fatalf("got %d units, want 2: %+v", len(got), got)
	}
	if got[0].Location.(cellLocation).Sheet != "First" || got[1].Location.(cellLocation).Sheet != "Second" {
		t.Errorf("sheets = %q, %q, want First, Second", got[0].Location.(cellLocation).Sheet, got[1].Location.(cellLocation).Sheet)
	}
}

func TestExtractRepeatedColumnsAdvanceColumnCounter(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(
			`<table:table table:name="Sheet1"><table:table-row>` +
				`<table:table-cell><text:p>A</text:p></table:table-cell>` +
				`<table:table-cell table:number-columns-repeated="5"><text:p/></table:table-cell>` +
				`<table:table-cell><text:p>B</text:p></table:table-cell>` +
				`</table:table-row></table:table>`),
	})

	got := extractAll(t, data)
	if len(got) != 2 {
		t.Fatalf("got %d units, want 2 (repeated blank group must not be emitted): %+v", len(got), got)
	}
	if got[0].Text != "A" || got[0].Location.(cellLocation).Cell != "A1" {
		t.Errorf("unit 0 = %+v, want text A cell A1", got[0])
	}
	// Column 2 held the 5-wide repeated blank group (cols 2..6), so "B"
	// must land on column 7 = "G1".
	if got[1].Text != "B" || got[1].Location.(cellLocation).Cell != "G1" {
		t.Errorf("unit 1 = %+v, want text B cell G1", got[1])
	}
}

func TestExtractRepeatedRowsAdvanceRowCounter(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(
			`<table:table table:name="Sheet1">` +
				`<table:table-row><table:table-cell><text:p>Row1</text:p></table:table-cell></table:table-row>` +
				`<table:table-row table:number-rows-repeated="10"><table:table-cell><text:p/></table:table-cell></table:table-row>` +
				`<table:table-row><table:table-cell><text:p>RowAfter</text:p></table:table-cell></table:table-row>` +
				`</table:table>`),
	})

	got := extractAll(t, data)
	if len(got) != 2 {
		t.Fatalf("got %d units, want 2: %+v", len(got), got)
	}
	if got[0].Text != "Row1" || got[0].Location.(cellLocation).Row != 1 {
		t.Errorf("unit 0 = %+v, want text Row1 row 1", got[0])
	}
	// Row 2 held the 10-wide repeated blank group (rows 2..11), so
	// "RowAfter" must land on row 12.
	if got[1].Text != "RowAfter" || got[1].Location.(cellLocation).Row != 12 {
		t.Errorf("unit 1 = %+v, want text RowAfter row 12", got[1])
	}
}

// TestExtractMultiParagraphCellEmitsSeparateUnits exercises a cell
// wrapped across multiple text:p children (ODS's equivalent of a
// manually line-wrapped Excel cell): each non-blank paragraph must be
// its own TextUnit sharing the cell's Location, not joined with an
// embedded newline -- see content.go's doc comment.
func TestExtractMultiParagraphCellEmitsSeparateUnits(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(
			`<table:table table:name="Sheet1"><table:table-row>` +
				`<table:table-cell><text:p>line one</text:p><text:p>line two</text:p></table:table-cell>` +
				`</table:table-row></table:table>`),
	})

	got := extractAll(t, data)
	if len(got) != 2 {
		t.Fatalf("got %d units, want 2: %+v", len(got), got)
	}
	if got[0].Text != "line one" || got[1].Text != "line two" {
		t.Errorf("texts = %q, %q, want 'line one', 'line two'", got[0].Text, got[1].Text)
	}
	if got[0].Location != got[1].Location {
		t.Errorf("locations differ: %+v vs %+v, want same cell", got[0].Location, got[1].Location)
	}
}

func TestExtractBlankCellsSkipped(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(
			`<table:table table:name="Sheet1"><table:table-row>` +
				`<table:table-cell><text:p>A</text:p></table:table-cell>` +
				`<table:table-cell><text:p></text:p></table:table-cell>` +
				`<table:table-cell><text:p>C</text:p></table:table-cell>` +
				`</table:table-row></table:table>`),
	})

	got := extractAll(t, data)
	if len(got) != 2 {
		t.Fatalf("got %d units, want 2 (blank cell skipped): %+v", len(got), got)
	}
	if got[0].Location.(cellLocation).Cell != "A1" || got[1].Location.(cellLocation).Cell != "C1" {
		t.Errorf("cells = %q, %q, want A1, C1", got[0].Location.(cellLocation).Cell, got[1].Location.(cellLocation).Cell)
	}
}

func TestExtractTabAndLineBreakInCell(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(
			`<table:table table:name="Sheet1"><table:table-row>` +
				`<table:table-cell><text:p>a<text:tab/>b<text:line-break/>c</text:p></table:table-cell>` +
				`</table:table-row></table:table>`),
	})

	got := extractAll(t, data)
	if len(got) != 2 {
		t.Fatalf("got %d units, want 2 (line-break splits the paragraph): %+v", len(got), got)
	}
	if got[0].Text != "a\tb" || got[1].Text != "c" {
		t.Errorf("texts = %q, %q, want 'a\\tb', 'c'", got[0].Text, got[1].Text)
	}
}

// TestExtractCommentOnlyCellReportsNoContent is a regression test for
// Fix #1: a table:table-cell whose only content is an office:annotation
// (a cell comment, with no real value of its own) must report NO
// content at all -- not the comment's text, and not its author/date
// metadata either. On the old pDepth-only model, the comment's own
// text:p (and any bare CharData like dc:creator/dc:date) leaked through
// as if it were the cell's own value, making a visually-empty
// (comment-only) cell falsely report content.
func TestExtractCommentOnlyCellReportsNoContent(t *testing.T) {
	const annNS = `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" ` +
		`xmlns:dc="http://purl.org/dc/elements/1.1/"`
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(
			`<table:table table:name="Sheet1" ` + annNS + `><table:table-row>` +
				`<table:table-cell>` +
				`<office:annotation><dc:creator>Jane Reviewer</dc:creator>` +
				`<text:p>Double check this number.</text:p></office:annotation>` +
				`</table:table-cell>` +
				`<table:table-cell><text:p>B1</text:p></table:table-cell>` +
				`</table:table-row></table:table>`),
	})

	got := extractAll(t, data)
	if len(got) != 1 {
		t.Fatalf("got %d units, want 1 (comment-only cell A1 must report no content): %+v", len(got), got)
	}
	if got[0].Location.(cellLocation).Cell != "B1" || got[0].Text != "B1" {
		t.Errorf("unit 0 = %+v, want text B1 at cell B1", got[0])
	}
}

// TestExtractSpaceRunIsCapped is a regression test for Fix #2: text:s's
// text:c count attribute must be clamped to a reasonable upper bound
// rather than synthesizing an unbounded run of spaces.
func TestExtractSpaceRunIsCapped(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(
			`<table:table table:name="Sheet1"><table:table-row>` +
				`<table:table-cell><text:p>a<text:s text:c="200000000"/>b</text:p></table:table-cell>` +
				`</table:table-row></table:table>`),
	})

	got := extractAll(t, data)
	if len(got) != 1 {
		t.Fatalf("got %d units, want 1: %+v", len(got), got)
	}
	wantLen := 1 + maxSpaceRun + 1
	if len(got[0].Text) != wantLen {
		t.Errorf("text length = %d, want %d (capped at maxSpaceRun=%d spaces)", len(got[0].Text), wantLen, maxSpaceRun)
	}
}

// TestExtractRepeatedCellReportsRepeatCount exercises cellLocation.Repeat
// (Fix #5): a repeated cell/row that carries real (non-blank) content is
// still only reported once, at its first cell reference, but the
// occurrence count itself is available via Repeat/Fields() so a caller
// can at least learn how many times the value actually occurs.
func TestExtractRepeatedCellReportsRepeatCount(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(
			`<table:table table:name="Sheet1"><table:table-row>` +
				`<table:table-cell table:number-columns-repeated="4"><text:p>same value</text:p></table:table-cell>` +
				`</table:table-row></table:table>`),
	})

	got := extractAll(t, data)
	if len(got) != 1 {
		t.Fatalf("got %d units, want 1 (repeated non-blank content reported once): %+v", len(got), got)
	}
	loc := got[0].Location.(cellLocation)
	if loc.Cell != "A1" {
		t.Errorf("cell = %q, want A1", loc.Cell)
	}
	if loc.Repeat != 4 {
		t.Errorf("Repeat = %d, want 4", loc.Repeat)
	}
	if got, want := loc.Fields(nil)["repeat"], 4; got != want {
		t.Errorf("Fields()[\"repeat\"] = %v, want %v", got, want)
	}
}

func TestSniffRenamedExtensionViaRegistryFallback(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(`<table:table table:name="Sheet1"></table:table>`),
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "renamed.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	reg := registry.New()
	reg.Register(Extractor{})

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	got, ok := reg.For(path, f, info.Size())
	if !ok {
		t.Fatal("expected registry.For to recognize the renamed ods via content sniffing")
	}
	if got.Name() != "ods" {
		t.Errorf("resolved extractor = %q, want ods", got.Name())
	}
}

func TestExtractContextCancellationStopsCleanly(t *testing.T) {
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(
			`<table:table table:name="Sheet1">` +
				`<table:table-row><table:table-cell><text:p>one</text:p></table:table-cell></table:table-row>` +
				`<table:table-row><table:table-cell><text:p>two</text:p></table:table-cell></table:table-row>` +
				`<table:table-row><table:table-cell><text:p>three</text:p></table:table-cell></table:table-row>` +
				`</table:table>`),
	})

	ctx, cancel := context.WithCancel(context.Background())
	r := bytes.NewReader(data)
	units, errc := (Extractor{}).Extract(ctx, r, int64(len(data)))

	<-units
	cancel()
	for range units {
	}
	if err := <-errc; err != nil {
		t.Fatalf("unexpected error after cancellation: %v", err)
	}
}

// TestExtractCustomShapeInCellDoesNotLoseCellContent is a regression
// test for a review-caught gap in isSuppressedSubtreeRoot: draw:frame is
// the only drawing-shape wrapper it suppresses, but a cell can anchor
// several other ODF drawing shapes (draw:custom-shape, draw:g, and more)
// that equally embed a text-box paragraph. Before pDepth (see
// content.go's doc comment) was added as a general-purpose backstop, a
// paragraph nested inside one of those was treated as the cell's OWN
// paragraph -- silently replacing the cell's real content with the
// shape's, rather than merely mis-tagging it. This is more severe than
// odt's equivalent bug: the cell's real value doesn't just get
// corrupted, it disappears from search results entirely.
func TestExtractCustomShapeInCellDoesNotLoseCellContent(t *testing.T) {
	const drawNS = `xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"`
	data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(
			`<table:table table:name="Sheet1"><table:table-row>` +
				`<table:table-cell ` + drawNS + `><text:p>cell-before` +
				`<draw:custom-shape><draw:text-box><text:p>SHAPE TEXT IN CELL</text:p></draw:text-box></draw:custom-shape>` +
				`cell-after</text:p></table:table-cell>` +
				`</table:table-row></table:table>`),
	})

	got := extractAll(t, data)
	if len(got) != 1 {
		t.Fatalf("got %d units, want 1: %+v", len(got), got)
	}
	if got[0].Text != "cell-beforecell-after" {
		t.Errorf("cell text = %q, want %q -- the shape's nested paragraph replaced the cell's real content", got[0].Text, "cell-beforecell-after")
	}
	if loc := got[0].Location.(cellLocation); loc.Cell != "A1" {
		t.Errorf("cell location = %+v, want Cell:A1", loc)
	}
}

// TestExtractSvgTitleSuppressedByNamespaceNotJustLocalName is a
// regression test for isSuppressedSubtreeRoot's namespace-aware
// svg:title/svg:desc check -- see odt's identical test for the fuller
// rationale, including why svg:title must sit OUTSIDE a draw:frame
// (draw:frame's own local-name-only suppression would otherwise mask a
// broken namespace check) and, critically, why the probe must be a
// text:p NESTED inside svg:title rather than bare CharData directly
// inside it: isTextContentElement's whitelist already refuses direct
// CharData whose parent is "title" regardless of suppression, so a
// bare-CharData version of this test would pass even with the
// namespace check completely broken. Only a nested structural element
// makes suppression's effect (skip the whole subtree, not just refuse
// direct CharData) observable -- exactly the trap the first draft of
// this test fell into.
func TestExtractSvgTitleSuppressedByNamespaceNotJustLocalName(t *testing.T) {
	for _, ns := range []string{
		"urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0", // ODF 1.2+
		"http://www.w3.org/2000/svg",                               // ODF 1.0/1.1
	} {
		t.Run(ns, func(t *testing.T) {
			svgNS := `xmlns:svg="` + ns + `"`
			data := buildOds(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
				"content.xml": wrapContent(
					`<table:table table:name="Sheet1"><table:table-row>` +
						`<table:table-cell><text:p>before</text:p></table:table-cell>` +
						`<table:table-cell ` + svgNS + `><svg:title><text:p>NESTED INSIDE SVG TITLE</text:p></svg:title></table:table-cell>` +
						`<table:table-cell><text:p>after</text:p></table:table-cell>` +
						`</table:table-row></table:table>`),
			})
			got := extractAll(t, data)
			var texts []string
			for _, u := range got {
				texts = append(texts, u.Text)
			}
			want := []string{"before", "after"}
			if len(texts) != len(want) {
				t.Fatalf("cell texts = %v, want %v -- svg:title's nested paragraph should be entirely "+
					"suppressed, not emitted as its own cell", texts, want)
			}
			for i, w := range want {
				if texts[i] != w {
					t.Errorf("cell %d text = %q, want %q", i, texts[i], w)
				}
			}
		})
	}
}

// TestSniffRejectsOversizedMimetypeAndFallsBackToBodyKind is a
// regression test for maxMimetypeBytes -- see odt's identical test for
// the fuller rationale.
func TestSniffRejectsOversizedMimetypeAndFallsBackToBodyKind(t *testing.T) {
	oversized := strings.Repeat("x", maxMimetypeBytes+1)
	data := buildOds(t, oversized, map[string]string{
		"content.xml": wrapContent(`<table:table table:name="Sheet1"><table:table-row><table:table-cell><text:p>hello</text:p></table:table-cell></table:table-row></table:table>`),
	})
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.ods", r, int64(len(data))) {
		t.Error("Sniff = false, want true (should fall back to bodyKind's structural check)")
	}
}
