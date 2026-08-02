package odt

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

// buildOdt assembles a minimal, but real, in-memory zip archive with the
// given named parts plus a bare-bones mimetype entry and META-INF/
// manifest.xml so it looks like a plausible ODF package. This
// deliberately does not shell out to LibreOffice or check in a real
// binary .odt fixture -- everything here is constructed programmatically
// to exercise our own zip/XML streaming code.
func buildOdt(t *testing.T, mimetype string, parts map[string]string) []byte {
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

const odtNS = `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" ` +
	`xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" ` +
	`xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0"`

func wrapContent(body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><office:document-content ` + odtNS + `>` +
		`<office:body><office:text>` + body + `</office:text></office:body></office:document-content>`
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

func TestLocationHyperlinkURI(t *testing.T) {
	const path = "/path/doc.odt"
	cases := []struct {
		name string
		loc  domain.Location
		want string
	}{
		{"paragraph", paragraphLocation{Paragraph: 88}, "file:///path/doc.odt"},
		{"cell", cellLocation{Table: 1, Row: 2, Col: 3}, "file:///path/doc.odt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.loc.HyperlinkURI(path, nil); got != tc.want {
				t.Errorf("HyperlinkURI() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSniffAcceptsOdtViaMimetype(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(`<text:p>hi</text:p>`),
	})
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.odt", r, int64(len(data))) {
		t.Error("expected Sniff to accept an odt via its mimetype part")
	}
}

func TestSniffRejectsOdsViaMimetype(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.spreadsheet", map[string]string{
		"content.xml": wrapContent(`<text:p>hi</text:p>`),
	})
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.odt", r, int64(len(data))) {
		t.Error("expected Sniff to reject an ods package even though it has a content.xml")
	}
}

func TestSniffFallsBackToBodyKindWithoutMimetype(t *testing.T) {
	data := buildOdt(t, "", map[string]string{
		"content.xml": wrapContent(`<text:p>hi</text:p>`),
	})
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.odt", r, int64(len(data))) {
		t.Error("expected Sniff to accept an odt lacking a mimetype part via office:body's child element")
	}
}

func TestSniffRejectsNonZipGarbage(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x00, 0xff, 0xfe}
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.odt", r, int64(len(data))) {
		t.Error("expected Sniff to reject non-zip garbage, not panic")
	}
}

func TestSniffRejectsEmptyFile(t *testing.T) {
	r := bytes.NewReader(nil)
	if (Extractor{}).Sniff("file.odt", r, 0) {
		t.Error("expected Sniff to reject an empty file")
	}
}

func TestExtractMultipleParagraphs(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p>First paragraph</text:p>` +
				`<text:p>Second paragraph</text:p>` +
				`<text:p>Third</text:p>`),
	})

	got := extractAll(t, data)
	if len(got) != 3 {
		t.Fatalf("got %d units, want 3: %+v", len(got), got)
	}

	wantTexts := []string{"First paragraph", "Second paragraph", "Third"}
	for i, u := range got {
		if u.Text != wantTexts[i] {
			t.Errorf("unit %d text = %q, want %q", i, u.Text, wantTexts[i])
		}
		if got := u.Location.Human(); got != "" {
			t.Errorf("unit %d Human() = %q, want \"\" (no heading precedes it)", i, got)
		}
		loc, ok := u.Location.(paragraphLocation)
		if !ok {
			t.Fatalf("unit %d location type = %T, want paragraphLocation", i, u.Location)
		}
		if loc.Paragraph != i+1 {
			t.Errorf("unit %d Paragraph = %d, want %d", i, loc.Paragraph, i+1)
		}
	}
}

// TestExtractHeadingTracking exercises text:h-based heading tracking,
// the direct ODF analogue of docx's Heading-style paragraph tracking.
func TestExtractHeadingTracking(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p>before any heading</text:p>` +
				`<text:h text:outline-level="1">Introduction</text:h>` +
				`<text:p>under introduction</text:p>` +
				`<table:table><table:table-row><table:table-cell><text:p>cell under introduction</text:p></table:table-cell></table:table-row></table:table>` +
				`<text:h text:outline-level="2">Details</text:h>` +
				`<text:p>under details</text:p>`),
	})

	got := extractAll(t, data)

	byText := make(map[string]string)
	for _, u := range got {
		byText[u.Text] = u.Location.Human()
	}

	cases := []struct {
		text string
		want string
	}{
		{"before any heading", ""},
		{"Introduction", "Introduction"},
		{"under introduction", "Introduction"},
		{"cell under introduction", "Introduction"},
		{"Details", "Details"},
		{"under details", "Details"},
	}
	for _, tc := range cases {
		got, ok := byText[tc.text]
		if !ok {
			t.Errorf("no unit found with text %q", tc.text)
			continue
		}
		if got != tc.want {
			t.Errorf("unit %q: Human() = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestExtractTableNotDoubleCounted(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p>Before table</text:p>` +
				`<table:table>` +
				`<table:table-row><table:table-cell><text:p>R1C1</text:p></table:table-cell><table:table-cell><text:p>R1C2</text:p></table:table-cell></table:table-row>` +
				`<table:table-row><table:table-cell><text:p>R2C1a</text:p><text:p>R2C1b</text:p></table:table-cell><table:table-cell><text:p/></table:table-cell></table:table-row>` +
				`</table:table>` +
				`<text:p>After table</text:p>`),
	})

	got := extractAll(t, data)

	var paragraphs, cells []domain.TextUnit
	for _, u := range got {
		switch u.Location.(type) {
		case paragraphLocation:
			paragraphs = append(paragraphs, u)
		case cellLocation:
			cells = append(cells, u)
		default:
			t.Errorf("unexpected location type %T", u.Location)
		}
	}

	if len(paragraphs) != 2 {
		t.Fatalf("got %d bare paragraphs, want 2: %+v", len(paragraphs), paragraphs)
	}
	if paragraphs[0].Text != "Before table" || paragraphs[0].Location.(paragraphLocation).Paragraph != 1 {
		t.Errorf("paragraph 0 = %+v, want text 'Before table' Paragraph 1", paragraphs[0])
	}
	if paragraphs[1].Text != "After table" || paragraphs[1].Location.(paragraphLocation).Paragraph != 2 {
		t.Errorf("paragraph 1 = %+v, want text 'After table' Paragraph 2", paragraphs[1])
	}

	if len(cells) != 4 {
		t.Fatalf("got %d table cell units, want 4: %+v", len(cells), cells)
	}

	wantCell := cellLocation{Table: 1, Row: 1, Col: 1}
	if cells[0].Text != "R1C1" || cells[0].Location != wantCell {
		t.Errorf("cell 0 = %+v, want text R1C1 loc %+v", cells[0], wantCell)
	}
	wantCell2 := cellLocation{Table: 1, Row: 1, Col: 2}
	if cells[1].Text != "R1C2" || cells[1].Location != wantCell2 {
		t.Errorf("cell 1 = %+v, want text R1C2 loc %+v", cells[1], wantCell2)
	}
	wantCell3 := cellLocation{Table: 1, Row: 2, Col: 1}
	if cells[2].Text != "R2C1a" || cells[2].Location != wantCell3 {
		t.Errorf("cell 2 = %+v, want text R2C1a loc %+v", cells[2], wantCell3)
	}
	if cells[3].Text != "R2C1b" || cells[3].Location != wantCell3 {
		t.Errorf("cell 3 = %+v, want text R2C1b loc %+v", cells[3], wantCell3)
	}
}

// TestExtractRepeatedColumnsAdvanceColumnCounter exercises
// table:number-columns-repeated: a repeated (typically blank, trailing)
// cell group must not be double-emitted, but real cells addressed after
// it must still get the correct column number.
func TestExtractRepeatedColumnsAdvanceColumnCounter(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<table:table><table:table-row>` +
				`<table:table-cell><text:p>A</text:p></table:table-cell>` +
				`<table:table-cell table:number-columns-repeated="5"><text:p/></table:table-cell>` +
				`<table:table-cell><text:p>B</text:p></table:table-cell>` +
				`</table:table-row></table:table>`),
	})

	got := extractAll(t, data)
	cells := findByLocationType[cellLocation](got)
	if len(cells) != 2 {
		t.Fatalf("got %d cells, want 2 (repeated blank group must not be emitted): %+v", len(cells), cells)
	}
	if cells[0].Text != "A" || cells[0].Location.(cellLocation).Col != 1 {
		t.Errorf("cell 0 = %+v, want text A col 1", cells[0])
	}
	// Column 2 held the 5-wide repeated blank group (cols 2..6), so "B"
	// must land on column 7.
	if cells[1].Text != "B" || cells[1].Location.(cellLocation).Col != 7 {
		t.Errorf("cell 1 = %+v, want text B col 7", cells[1])
	}
}

// TestExtractRepeatedRowsAdvanceRowCounter is the row-repeat analogue of
// the column test above.
func TestExtractRepeatedRowsAdvanceRowCounter(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<table:table>` +
				`<table:table-row><table:table-cell><text:p>Row1</text:p></table:table-cell></table:table-row>` +
				`<table:table-row table:number-rows-repeated="10"><table:table-cell><text:p/></table:table-cell></table:table-row>` +
				`<table:table-row><table:table-cell><text:p>RowAfter</text:p></table:table-cell></table:table-row>` +
				`</table:table>`),
	})

	got := extractAll(t, data)
	cells := findByLocationType[cellLocation](got)
	if len(cells) != 2 {
		t.Fatalf("got %d cells, want 2: %+v", len(cells), cells)
	}
	if cells[0].Text != "Row1" || cells[0].Location.(cellLocation).Row != 1 {
		t.Errorf("cell 0 = %+v, want text Row1 row 1", cells[0])
	}
	// Row 2 held the 10-wide repeated blank group (rows 2..11), so
	// "RowAfter" must land on row 12.
	if cells[1].Text != "RowAfter" || cells[1].Location.(cellLocation).Row != 12 {
		t.Errorf("cell 1 = %+v, want text RowAfter row 12", cells[1])
	}
}

func TestExtractLineBreakSplitsParagraph(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(`<text:p>a<text:line-break/>b</text:p>`),
	})

	got := extractAll(t, data)
	if len(got) != 2 {
		t.Fatalf("got %d units, want 2: %+v", len(got), got)
	}
	if got[0].Text != "a" || got[1].Text != "b" {
		t.Errorf("got texts %q, %q, want a, b", got[0].Text, got[1].Text)
	}
	for i, u := range got {
		loc, ok := u.Location.(paragraphLocation)
		if !ok || loc.Paragraph != 1 {
			t.Errorf("unit %d Location = %+v, want paragraphLocation{Paragraph: 1}", i, u.Location)
		}
	}
}

func TestExtractTabAndSpaceElements(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(`<text:p>a<text:tab/>b<text:s text:c="3"/>c</text:p>`),
	})

	got := extractAll(t, data)
	if len(got) != 1 {
		t.Fatalf("got %d units, want 1: %+v", len(got), got)
	}
	want := "a\tb   c"
	if got[0].Text != want {
		t.Errorf("text = %q, want %q", got[0].Text, want)
	}
}

func TestExtractEmptyParagraphsStillEmitted(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p>one</text:p><text:p/><text:p>three</text:p>`),
	})

	got := extractAll(t, data)
	if len(got) != 3 {
		t.Fatalf("got %d units, want 3 (blank paragraphs are still emitted, unlike table cells)", len(got))
	}
	if got[1].Text != "" || got[1].Location.(paragraphLocation).Paragraph != 2 {
		t.Errorf("middle unit = %+v, want blank text at Paragraph 2", got[1])
	}
}

// TestExtractNestedFrameParagraphDoesNotCorruptEnclosingParagraph is the
// ODF analogue of docx's text-box regression test: a text:p nested
// inside a draw:frame/draw:text-box that itself sits inside a running
// paragraph (e.g. an inline captioned image) must not clobber the
// enclosing paragraph's in-progress text or shift paragraph numbering.
func TestExtractNestedFrameParagraphDoesNotCorruptEnclosingParagraph(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p>before-frame` +
				`<draw:frame xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"><draw:text-box><text:p>inside-textbox</text:p></draw:text-box></draw:frame>` +
				`after-frame</text:p>` +
				`<text:p>Second real paragraph</text:p>`),
	})

	got := extractAll(t, data)
	if len(got) != 2 {
		t.Fatalf("got %d paragraphs, want 2 (nested frame content must not add a spurious paragraph): %+v", len(got), got)
	}

	wantFirst := "before-frameafter-frame"
	if got[0].Text != wantFirst {
		t.Errorf("paragraph 1 text = %q, want %q (frame-nested content must not corrupt the enclosing paragraph)", got[0].Text, wantFirst)
	}
	if got[0].Location.(paragraphLocation).Paragraph != 1 {
		t.Errorf("paragraph 1 location = %+v, want Paragraph 1", got[0].Location)
	}

	wantSecond := "Second real paragraph"
	if got[1].Text != wantSecond {
		t.Errorf("paragraph 2 text = %q, want %q", got[1].Text, wantSecond)
	}
	if got[1].Location.(paragraphLocation).Paragraph != 2 {
		t.Errorf("paragraph 2 location = %+v, want Paragraph 2 (numbering must not shift due to the nested paragraph)", got[1].Location)
	}
}

// TestExtractImageAltTextDoesNotLeak is a regression test for Fix #1: a
// draw:frame's svg:title/svg:desc (accessible alt-text on an anchored
// image) must not be captured as part of the enclosing paragraph's text.
// On the old pDepth-only model, this leaked because pDepth stayed at 1
// the whole time (no nested text:p was ever opened), so any CharData
// encountered was blindly appended.
func TestExtractImageAltTextDoesNotLeak(t *testing.T) {
	const drawNS = `xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" ` +
		`xmlns:svg="urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0"`
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p ` + drawNS + `>before-image` +
				`<draw:frame><draw:image/><svg:title>A cat sitting on a mat</svg:title><svg:desc>Photo of a cat</svg:desc></draw:frame>` +
				`after-image</text:p>`),
	})

	got := extractAll(t, data)
	if len(got) != 1 {
		t.Fatalf("got %d units, want 1: %+v", len(got), got)
	}
	want := "before-imageafter-image"
	if got[0].Text != want {
		t.Errorf("text = %q, want %q (alt-text must not leak into the paragraph)", got[0].Text, want)
	}
}

// TestExtractImageBinaryDataDoesNotLeak is a regression test for Fix #1:
// office:binary-data (raw, typically base64, embedded image bytes)
// inside a draw:frame/draw:image must not be captured as paragraph text.
// A crafted file could otherwise leak large amounts of binary data
// straight into a paragraph's searchable text.
func TestExtractImageBinaryDataDoesNotLeak(t *testing.T) {
	const drawNS = `xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" ` +
		`xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0"`
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p ` + drawNS + `>before-image` +
				`<draw:frame><draw:image><office:binary-data>QUJDREVGR0hJSktMTU5PUA==</office:binary-data></draw:image></draw:frame>` +
				`after-image</text:p>`),
	})

	got := extractAll(t, data)
	if len(got) != 1 {
		t.Fatalf("got %d units, want 1: %+v", len(got), got)
	}
	want := "before-imageafter-image"
	if got[0].Text != want {
		t.Errorf("text = %q, want %q (embedded binary image data must not leak into the paragraph)", got[0].Text, want)
	}
}

// TestExtractFootnoteCitationDoesNotLeakButCitationExcluded is a
// regression test for Fix #1: a footnote's text:note-citation (the
// footnote mark/number rendered inline in the body) must not leak into
// the enclosing paragraph, and the footnote's own text:note-body content
// must stay excluded too (footnotes remain out of scope entirely, per
// the package doc comment -- this only fixes citation-number leakage,
// it does not add footnote support).
func TestExtractFootnoteCitationDoesNotLeak(t *testing.T) {
	const textNS = `xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"`
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p ` + textNS + `>before-note` +
				`<text:note text:note-class="footnote"><text:note-citation>1</text:note-citation>` +
				`<text:note-body><text:p>This is the footnote body text.</text:p></text:note-body></text:note>` +
				`after-note</text:p>`),
	})

	got := extractAll(t, data)
	if len(got) != 1 {
		t.Fatalf("got %d units, want 1 (footnote body must not become its own paragraph): %+v", len(got), got)
	}
	want := "before-noteafter-note"
	if got[0].Text != want {
		t.Errorf("text = %q, want %q (citation number must not leak into the paragraph)", got[0].Text, want)
	}
}

// TestExtractAnnotationMetadataDoesNotLeak is a regression test for Fix
// #1: office:annotation's dc:creator/dc:date (comment author/date
// metadata) must not leak into the enclosing paragraph's text, and the
// comment's own body text:p must stay excluded too (comments remain
// deferred entirely, per the package doc comment).
func TestExtractAnnotationMetadataDoesNotLeak(t *testing.T) {
	const annNS = `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" ` +
		`xmlns:dc="http://purl.org/dc/elements/1.1/"`
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p ` + annNS + `>before-comment` +
				`<office:annotation><dc:creator>Jane Reviewer</dc:creator><dc:date>2024-01-01T00:00:00</dc:date>` +
				`<text:p>This needs revision.</text:p></office:annotation>` +
				`after-comment</text:p>`),
	})

	got := extractAll(t, data)
	if len(got) != 1 {
		t.Fatalf("got %d units, want 1 (comment body must not become its own paragraph): %+v", len(got), got)
	}
	want := "before-commentafter-comment"
	if got[0].Text != want {
		t.Errorf("text = %q, want %q (comment author/date must not leak into the paragraph)", got[0].Text, want)
	}
}

// TestExtractTrackedChangesDeletionDoesNotLeakOrShiftNumbering is a
// regression test for Fix #1: text:tracked-changes' text:deletion (the
// preserved content of text removed via track-changes) must not be
// emitted as if it were a live paragraph, and -- since on the old model
// its wrapped text:p was indistinguishable from a real top-level
// paragraph -- must not shift subsequent real paragraphs' numbering
// either.
func TestExtractTrackedChangesDeletionDoesNotLeakOrShiftNumbering(t *testing.T) {
	const tcNS = `xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"`
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:tracked-changes ` + tcNS + `>` +
				`<text:changed-region text:id="ct1"><text:deletion><text:p>this text was deleted</text:p></text:deletion></text:changed-region>` +
				`</text:tracked-changes>` +
				`<text:p>First real paragraph</text:p>` +
				`<text:p>Second real paragraph</text:p>`),
	})

	got := extractAll(t, data)
	if len(got) != 2 {
		t.Fatalf("got %d units, want 2 (deleted text must not become its own paragraph): %+v", len(got), got)
	}
	if got[0].Text != "First real paragraph" || got[0].Location.(paragraphLocation).Paragraph != 1 {
		t.Errorf("unit 0 = %+v, want text 'First real paragraph' Paragraph 1", got[0])
	}
	if got[1].Text != "Second real paragraph" || got[1].Location.(paragraphLocation).Paragraph != 2 {
		t.Errorf("unit 1 = %+v, want text 'Second real paragraph' Paragraph 2 (numbering must not shift)", got[1])
	}
}

// TestExtractHeadingInsideTableCellDoesNotLeakOutward is a regression
// test for Fix #3: a text:h found inside a table cell must not become
// the "current heading" reported by paragraphs OUTSIDE that table. On
// the old model, currentHeading was a single flat variable updated
// unconditionally by any text:h, wherever it was found.
func TestExtractHeadingInsideTableCellDoesNotLeakOutward(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:h text:outline-level="1">Outer Heading</text:h>` +
				`<table:table><table:table-row><table:table-cell>` +
				`<text:h text:outline-level="2">Cell-Local Heading</text:h>` +
				`<text:p>cell paragraph</text:p>` +
				`</table:table-cell></table:table-row></table:table>` +
				`<text:p>after the table</text:p>`),
	})

	got := extractAll(t, data)

	byText := make(map[string]string)
	for _, u := range got {
		byText[u.Text] = u.Location.Human()
	}

	if got := byText["cell paragraph"]; got != "Cell-Local Heading" {
		t.Errorf("cell paragraph heading = %q, want %q", got, "Cell-Local Heading")
	}
	if got := byText["after the table"]; got != "Outer Heading" {
		t.Errorf("after-table paragraph heading = %q, want %q (must not pick up the cell-local heading)", got, "Outer Heading")
	}
}

// TestExtractSpaceRunIsCapped is a regression test for Fix #2: text:s's
// text:c count attribute must be clamped to a reasonable upper bound
// rather than synthesizing an unbounded run of spaces. A crafted
// document could otherwise set text:c to an enormous value inside an
// otherwise tiny file and produce a multi-hundred-MB TextUnit.
func TestExtractSpaceRunIsCapped(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(`<text:p>a<text:s text:c="200000000"/>b</text:p>`),
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

func TestSniffRenamedExtensionViaRegistryFallback(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(`<text:p>hi</text:p>`),
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
		t.Fatal("expected registry.For to recognize the renamed odt via content sniffing")
	}
	if got.Name() != "odt" {
		t.Errorf("resolved extractor = %q, want odt", got.Name())
	}
}

func TestExtractContextCancellationStopsCleanly(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p>one</text:p><text:p>two</text:p><text:p>three</text:p>`),
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

// TestExtractCustomShapeDoesNotCorruptEnclosingParagraph is a regression
// test for a review-caught gap in isSuppressedSubtreeRoot: draw:frame is
// the only drawing-shape wrapper it suppresses, but ODF has several
// others that equally anchor a text-box paragraph (draw:custom-shape,
// draw:g, draw:rect, and more) -- a paragraph nested inside any of those
// used to be treated as a normal top-level text:p, corrupting the
// enclosing paragraph's text and shifting paragraph numbering exactly
// the way a draw:frame's nested paragraph did before that fix. pDepth
// (see content.go's doc comment) is the general-purpose backstop that
// closes this gap for ANY wrapper, not just draw:frame specifically.
func TestExtractCustomShapeDoesNotCorruptEnclosingParagraph(t *testing.T) {
	const drawNS = `xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"`
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p>Body paragraph one</text:p>` +
				`<text:p ` + drawNS + `><draw:custom-shape><draw:text-box><text:p>SHAPE LABEL TEXT</text:p></draw:text-box></draw:custom-shape></text:p>` +
				`<text:p>Body paragraph two</text:p>`),
	})

	got := extractAll(t, data)
	paras := findByLocationType[paragraphLocation](got)
	if len(paras) != 3 {
		t.Fatalf("got %d paragraph units, want 3: %+v", len(paras), paras)
	}
	want := []string{"Body paragraph one", "", "Body paragraph two"}
	for i, p := range paras {
		if p.Text != want[i] {
			t.Errorf("paragraph %d text = %q, want %q", i, p.Text, want[i])
		}
		if loc := p.Location.(paragraphLocation); loc.Paragraph != i+1 {
			t.Errorf("paragraph %d number = %d, want %d", i, loc.Paragraph, i+1)
		}
	}
	for _, p := range paras {
		if strings.Contains(p.Text, "SHAPE LABEL TEXT") {
			t.Errorf("shape text leaked into paragraph text: %q", p.Text)
		}
	}
}

// TestExtractOrphanTableCellDoesNotCorruptALaterRealTable is a
// regression test for the cellStack nil-placeholder scheme: a
// table-cell with no enclosing table at all (malformed input -- note an
// orphan cell can never be nested INSIDE a real, still-open cell, since
// encoding/xml's own well-formedness checking guarantees a real cell's
// enclosing table:table cannot have closed while that cell is still
// open, so cellStack non-empty always implies tableStack non-empty; the
// actually-reachable failure mode is this one: an orphan cell earlier in
// the document leaving stale state that corrupts a LATER, unrelated
// table). Before the fix, the orphan's start pushed nothing onto
// cellStack (guarded by `if len(tableStack) > 0`), but its end tag
// unconditionally popped whatever was on top -- here, nothing was on
// top yet, so this specific document wouldn't have broken even before
// the fix, but the same missing-push/unconditional-pop shape is exactly
// what made the truly corrupting case (documented in the very first
// version of this test, since proven unreachable) look plausible. This
// test locks in that an orphan cell's own content is handled
// sensibly (attributed to the paragraph-level fallback, not silently
// dropped or crashing) and that it never leaks state into whatever real
// table follows.
func TestExtractOrphanTableCellDoesNotCorruptALaterRealTable(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p>before</text:p>` +
				`<table:table-cell><text:p>orphan cell, no enclosing table at all</text:p></table:table-cell>` +
				`<table:table><table:table-row><table:table-cell><text:p>A1</text:p></table:table-cell></table:table-row></table:table>` +
				`<text:p>after</text:p>`),
	})

	got := extractAll(t, data)

	cells := findByLocationType[cellLocation](got)
	if len(cells) != 1 {
		t.Fatalf("got %d cell units, want 1 (only the real table's A1): %+v", len(cells), cells)
	}
	if loc := cells[0].Location.(cellLocation); loc.Table != 1 || loc.Row != 1 || loc.Col != 1 || cells[0].Text != "A1" {
		t.Errorf("real cell unit = %+v (text %q), want Table:1 Row:1 Col:1 text \"A1\" -- "+
			"the orphan cell corrupted the real table's tracking", loc, cells[0].Text)
	}

	paras := findByLocationType[paragraphLocation](got)
	var texts []string
	for _, p := range paras {
		texts = append(texts, p.Text)
	}
	wantTexts := []string{"before", "orphan cell, no enclosing table at all", "after"}
	if len(texts) != len(wantTexts) {
		t.Fatalf("paragraph texts = %v, want %v", texts, wantTexts)
	}
	for i, w := range wantTexts {
		if texts[i] != w {
			t.Errorf("paragraph %d text = %q, want %q", i, texts[i], w)
		}
	}
}

// TestExtractSvgTitleSuppressedByNamespaceNotJustLocalName is a
// regression test for isSuppressedSubtreeRoot's namespace-aware
// svg:title/svg:desc check: unlike draw:frame/office:annotation/etc.,
// which are matched by local name alone, title/desc must additionally
// match one of the accepted SVG namespace URIs (see svgTitleDescNS) so
// an unrelated same-named element from a different namespace (e.g.
// dc:title) is never mistakenly suppressed. This constructs svg:title
// OUTSIDE a draw:frame, so suppression can only be happening because of
// the namespace check itself -- inside a draw:frame, the frame's own
// (local-name-only) suppression would mask a broken namespace check
// entirely, which is exactly how the original CodeRabbit fix shipped
// with zero coverage: every existing alt-text test nests svg:title
// inside a draw:frame.
//
// Critically, the probe here is a text:p NESTED inside svg:title, not
// bare CharData directly inside it: isTextContentElement's whitelist
// already refuses CharData whose immediate parent is "title" regardless
// of suppression (title isn't p/h/span/a), so a bare-CharData version of
// this test would pass even with isSuppressedSubtreeRoot's namespace
// check completely broken -- exactly the vacuous-test trap the first
// draft of this test fell into. Only a nested STRUCTURAL element
// (another text:p) makes suppression's effect -- skipping the whole
// subtree, not just refusing direct CharData -- observable: suppressed,
// it never becomes its own paragraph; unsuppressed, it does.
func TestExtractSvgTitleSuppressedByNamespaceNotJustLocalName(t *testing.T) {
	for _, ns := range []string{
		"urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0", // ODF 1.2+
		"http://www.w3.org/2000/svg",                               // ODF 1.0/1.1
	} {
		t.Run(ns, func(t *testing.T) {
			svgNS := `xmlns:svg="` + ns + `"`
			data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
				"content.xml": wrapContent(
					`<text:p>before</text:p>` +
						`<svg:title ` + svgNS + `><text:p>NESTED INSIDE SVG TITLE</text:p></svg:title>` +
						`<text:p>after</text:p>`),
			})
			got := extractAll(t, data)
			paras := findByLocationType[paragraphLocation](got)
			if len(paras) != 2 {
				t.Fatalf("got %d paragraph units, want 2 (before, after): %+v", len(paras), paras)
			}
			if paras[0].Text != "before" || paras[1].Text != "after" {
				t.Errorf("paragraph texts = [%q, %q], want [\"before\", \"after\"] -- "+
					"svg:title's nested paragraph should be entirely suppressed, not emitted as its own unit",
					paras[0].Text, paras[1].Text)
			}
		})
	}
}

// TestExtractUnrelatedTitleElementNotSuppressedByNamespaceCheck is the
// negative counterpart to the test above: a "title"-named element in a
// namespace that is NOT one of svgTitleDescNS's accepted SVG namespaces
// (e.g. Dublin Core's dc:title, a real ODF metadata element) must not be
// treated as a suppressed subtree root just because its local name
// matches. Nested content inside it (here, a paragraph) is unusual for
// dc:title in practice, but is the only externally observable way to
// tell "suppressed" apart from "not suppressed" for an otherwise-empty
// wrapper -- if isSuppressedSubtreeRoot regressed to matching "title" by
// local name alone again, this nested paragraph would silently vanish
// (suppressed) instead of being emitted.
func TestExtractUnrelatedTitleElementNotSuppressedByNamespaceCheck(t *testing.T) {
	const dcNS = `xmlns:dc="http://purl.org/dc/elements/1.1/"`
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<dc:title ` + dcNS + `><text:p>nested under dc:title, not svg:title</text:p></dc:title>`),
	})
	got := extractAll(t, data)
	paras := findByLocationType[paragraphLocation](got)
	if len(paras) != 1 || paras[0].Text != "nested under dc:title, not svg:title" {
		t.Fatalf("got paragraphs %+v, want exactly one unit with the nested paragraph's text -- "+
			"dc:title must not be suppressed just because its local name matches svg:title/desc", paras)
	}
}

// TestSniffRejectsOversizedMimetypeAndFallsBackToBodyKind is a
// regression test for maxMimetypeBytes: a mimetype part exceeding the
// cap must not be read in full (a crafted zip could pair a huge
// declared/compressed size with this well-known part name to force an
// unbounded inflate); Sniff must fall back to bodyKind's structural
// check instead, and correctly still recognize the file via that path.
func TestSniffRejectsOversizedMimetypeAndFallsBackToBodyKind(t *testing.T) {
	oversized := strings.Repeat("x", maxMimetypeBytes+1)
	data := buildOdt(t, oversized, map[string]string{
		"content.xml": wrapContent(`<text:p>hello</text:p>`),
	})
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.odt", r, int64(len(data))) {
		t.Error("Sniff = false, want true (should fall back to bodyKind's structural check)")
	}
}

// findByLocationType returns the subset of units whose Location is of
// concrete type T, preserving order.
func findByLocationType[T domain.Location](units []domain.TextUnit) []domain.TextUnit {
	var out []domain.TextUnit
	for _, u := range units {
		if _, ok := u.Location.(T); ok {
			out = append(out, u)
		}
	}
	return out
}
