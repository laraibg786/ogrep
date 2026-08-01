package docx

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

// buildDocx assembles a minimal, but real, in-memory zip archive with
// the given named parts (plus a bare-bones [Content_Types].xml and
// _rels/.rels so it looks like a plausible OOXML package), and returns
// its bytes. This deliberately does not shell out to any external tool
// or check in a real binary .docx fixture -- everything here is
// constructed programmatically and is only meant to exercise our own
// zip/XML streaming code, not to be a Word-round-trippable file.
func buildDocx(t *testing.T, parts map[string]string) []byte {
	t.Helper()

	all := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="xml" ContentType="application/xml"/>` +
			`</Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"></Relationships>`,
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

const wNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`

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
	const path = "/path/doc.docx"
	cases := []struct {
		name string
		loc  domain.Location
		want string
	}{
		{"paragraph", paragraphLocation{Paragraph: 88}, "file:///path/doc.docx"},
		{"cell", cellLocation{Table: 1, Row: 2, Col: 3}, "file:///path/doc.docx"},
		{"headerFooter", headerFooterLocation{Label: "Header 1"}, ""},
		{"footnote", footnoteLocation{Label: "Footnote 3"}, ""},
		{"comment", commentLocation{Label: "Comment 2"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.loc.HyperlinkURI(path, nil); got != tc.want {
				t.Errorf("HyperlinkURI() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLabelOnlyLocationsExposeLabelInFields(t *testing.T) {
	cases := []struct {
		name string
		loc  domain.Location
		want string
	}{
		{"headerFooter", headerFooterLocation{Label: "Header 1"}, "Header 1"},
		{"footnote", footnoteLocation{Label: "Footnote 3"}, "Footnote 3"},
		{"comment", commentLocation{Label: "Comment 2"}, "Comment 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.loc.Fields(nil)["label"]; got != tc.want {
				t.Errorf("Fields()[\"label\"] = %v, want %q", got, tc.want)
			}
		})
	}
}

func TestParagraphLocationHyperlinkURIEscapesPath(t *testing.T) {
	loc := paragraphLocation{Paragraph: 1}
	got := loc.HyperlinkURI("/path/my doc.docx", nil)
	want := "file:///path/my%20doc.docx"
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

func TestSniffAcceptsDocx(t *testing.T) {
	data := buildDocx(t, map[string]string{
		"word/document.xml": `<?xml version="1.0" encoding="UTF-8"?><w:document ` + wNS + `><w:body><w:p><w:r><w:t>hi</w:t></w:r></w:p></w:body></w:document>`,
	})
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.docx", r, int64(len(data))) {
		t.Error("expected Sniff to accept a docx with word/document.xml")
	}
}

func TestSniffRejectsZipWithoutDocumentXML(t *testing.T) {
	data := buildDocx(t, map[string]string{
		"word/other.xml": `<root/>`,
	})
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.docx", r, int64(len(data))) {
		t.Error("expected Sniff to reject a zip lacking word/document.xml")
	}
}

func TestSniffRejectsNonZipGarbage(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x00, 0xff, 0xfe}
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.docx", r, int64(len(data))) {
		t.Error("expected Sniff to reject non-zip garbage, not panic")
	}
}

func TestSniffRejectsEmptyFile(t *testing.T) {
	r := bytes.NewReader(nil)
	if (Extractor{}).Sniff("file.docx", r, 0) {
		t.Error("expected Sniff to reject an empty file")
	}
}

func TestExtractMultipleParagraphs(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?><w:document ` + wNS + `><w:body>` +
		`<w:p><w:r><w:t>First paragraph</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>Second </w:t></w:r><w:r><w:t>paragraph</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>Third</w:t></w:r></w:p>` +
		`</w:body></w:document>`
	data := buildDocx(t, map[string]string{"word/document.xml": doc})

	got := extractAll(t, data)
	if len(got) != 3 {
		t.Fatalf("got %d units, want 3: %+v", len(got), got)
	}

	wantTexts := []string{"First paragraph", "Second paragraph", "Third"}
	for i, u := range got {
		if u.Text != wantTexts[i] {
			t.Errorf("unit %d text = %q, want %q", i, u.Text, wantTexts[i])
		}
		wantHuman := "Paragraph " + string(rune('1'+i))
		if u.Location.Human() != wantHuman {
			t.Errorf("unit %d Human() = %q, want %q", i, u.Location.Human(), wantHuman)
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

func TestExtractTableNotDoubleCounted(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?><w:document ` + wNS + `><w:body>` +
		`<w:p><w:r><w:t>Before table</w:t></w:r></w:p>` +
		`<w:tbl>` +
		`<w:tr><w:tc><w:p><w:r><w:t>R1C1</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>R1C2</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:p><w:r><w:t>R2C1a</w:t></w:r></w:p><w:p><w:r><w:t>R2C1b</w:t></w:r></w:p></w:tc><w:tc><w:p/></w:tc></w:tr>` +
		`</w:tbl>` +
		`<w:p><w:r><w:t>After table</w:t></w:r></w:p>` +
		`</w:body></w:document>`
	data := buildDocx(t, map[string]string{"word/document.xml": doc})

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

	// Only the two paragraphs outside the table should be bare
	// paragraphs, numbered 1 and 2 (the table doesn't bump the
	// counter).
	if len(paragraphs) != 2 {
		t.Fatalf("got %d bare paragraphs, want 2: %+v", len(paragraphs), paragraphs)
	}
	if paragraphs[0].Text != "Before table" || paragraphs[0].Location.(paragraphLocation).Paragraph != 1 {
		t.Errorf("paragraph 0 = %+v, want text 'Before table' Paragraph 1", paragraphs[0])
	}
	if paragraphs[1].Text != "After table" || paragraphs[1].Location.(paragraphLocation).Paragraph != 2 {
		t.Errorf("paragraph 1 = %+v, want text 'After table' Paragraph 2", paragraphs[1])
	}

	// 3 non-blank cells: R1C1, R1C2, and the merged R2C1a/R2C1b cell.
	// The 4th cell (row 2, col 2) is blank and must be skipped.
	if len(cells) != 3 {
		t.Fatalf("got %d table cells, want 3: %+v", len(cells), cells)
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
	if cells[2].Text != "R2C1a\nR2C1b" || cells[2].Location != wantCell3 {
		t.Errorf("cell 2 = %+v, want text 'R2C1a\\nR2C1b' loc %+v", cells[2], wantCell3)
	}
}

func TestExtractHeaderFooterFootnoteComment(t *testing.T) {
	hdr := `<?xml version="1.0" encoding="UTF-8"?><w:hdr ` + wNS + `><w:p><w:r><w:t>My Header</w:t></w:r></w:p></w:hdr>`
	ftr := `<?xml version="1.0" encoding="UTF-8"?><w:ftr ` + wNS + `><w:p><w:r><w:t>My Footer</w:t></w:r></w:p></w:ftr>`
	footnotes := `<?xml version="1.0" encoding="UTF-8"?><w:footnotes ` + wNS + `>` +
		`<w:footnote w:id="0"><w:p><w:r><w:t></w:t></w:r></w:p></w:footnote>` +
		`<w:footnote w:id="3"><w:p><w:r><w:t>A footnote</w:t></w:r></w:p></w:footnote>` +
		`</w:footnotes>`
	comments := `<?xml version="1.0" encoding="UTF-8"?><w:comments ` + wNS + `>` +
		`<w:comment w:id="2"><w:p><w:r><w:t>A comment</w:t></w:r></w:p></w:comment>` +
		`</w:comments>`
	doc := `<?xml version="1.0" encoding="UTF-8"?><w:document ` + wNS + `><w:body><w:p><w:r><w:t>Body text</w:t></w:r></w:p></w:body></w:document>`

	data := buildDocx(t, map[string]string{
		"word/document.xml":  doc,
		"word/header1.xml":   hdr,
		"word/footer1.xml":   ftr,
		"word/footnotes.xml": footnotes,
		"word/comments.xml":  comments,
	})

	got := extractAll(t, data)

	headers := findByLocationType[headerFooterLocation](got)
	// Both header1.xml and footer1.xml produce headerFooterLocation
	// units; distinguish by Human label.
	var sawHeader, sawFooter bool
	for _, u := range headers {
		switch u.Location.Human() {
		case "Header 1":
			sawHeader = true
			if u.Text != "My Header" {
				t.Errorf("header text = %q, want %q", u.Text, "My Header")
			}
		case "Footer 1":
			sawFooter = true
			if u.Text != "My Footer" {
				t.Errorf("footer text = %q, want %q", u.Text, "My Footer")
			}
		default:
			t.Errorf("unexpected header/footer label %q", u.Location.Human())
		}
	}
	if !sawHeader {
		t.Error("expected a Header 1 unit")
	}
	if !sawFooter {
		t.Error("expected a Footer 1 unit")
	}

	footnoteUnits := findByLocationType[footnoteLocation](got)
	// The blank id=0 footnote (a typical separator placeholder) must be
	// skipped since it has no non-blank text; only id=3 should surface.
	if len(footnoteUnits) != 1 {
		t.Fatalf("got %d footnote units, want 1: %+v", len(footnoteUnits), footnoteUnits)
	}
	if footnoteUnits[0].Location.Human() != "Footnote 3" || footnoteUnits[0].Text != "A footnote" {
		t.Errorf("footnote unit = %+v, want Human 'Footnote 3' text 'A footnote'", footnoteUnits[0])
	}

	commentUnits := findByLocationType[commentLocation](got)
	if len(commentUnits) != 1 {
		t.Fatalf("got %d comment units, want 1: %+v", len(commentUnits), commentUnits)
	}
	if commentUnits[0].Location.Human() != "Comment 2" || commentUnits[0].Text != "A comment" {
		t.Errorf("comment unit = %+v, want Human 'Comment 2' text 'A comment'", commentUnits[0])
	}

	paragraphs := findByLocationType[paragraphLocation](got)
	if len(paragraphs) != 1 || paragraphs[0].Text != "Body text" {
		t.Errorf("body paragraphs = %+v, want single 'Body text' paragraph", paragraphs)
	}
}

func TestExtractTabAndBreak(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?><w:document ` + wNS + `><w:body>` +
		`<w:p><w:r><w:t>a</w:t><w:tab/><w:t>b</w:t><w:br/><w:t>c</w:t></w:r></w:p>` +
		`</w:body></w:document>`
	data := buildDocx(t, map[string]string{"word/document.xml": doc})

	got := extractAll(t, data)
	if len(got) != 1 {
		t.Fatalf("got %d units, want 1", len(got))
	}
	want := "a\tb\nc"
	if got[0].Text != want {
		t.Errorf("text = %q, want %q", got[0].Text, want)
	}
}

func TestExtractEmptyParagraphsStillEmitted(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?><w:document ` + wNS + `><w:body>` +
		`<w:p><w:r><w:t>one</w:t></w:r></w:p>` +
		`<w:p/>` +
		`<w:p><w:r><w:t>three</w:t></w:r></w:p>` +
		`</w:body></w:document>`
	data := buildDocx(t, map[string]string{"word/document.xml": doc})

	got := extractAll(t, data)
	if len(got) != 3 {
		t.Fatalf("got %d units, want 3 (blank paragraphs are still emitted, unlike table cells)", len(got))
	}
	if got[1].Text != "" || got[1].Location.(paragraphLocation).Paragraph != 2 {
		t.Errorf("middle unit = %+v, want blank text at Paragraph 2", got[1])
	}
}

// TestExtractTextBoxNestedParagraphDoesNotCorruptEnclosingParagraph is a
// regression test for a bug found in review: a w:p nested inside a text
// box (w:r > w:drawing > ... > w:txbxContent > w:p) would clobber the
// enclosing (real) paragraph's in-progress text before it closed,
// discarding its content, emitting a spurious extra empty paragraph, and
// shifting every subsequent paragraph's number by one. The fix
// suppresses all content-affecting elements (p/tbl/tr/tc/r/t/tab/br/cr)
// while inside a w:drawing/w:pict wrapper, so the enclosing paragraph's
// text and the document's paragraph numbering are unaffected by
// text-box-nested content.
func TestExtractTextBoxNestedParagraphDoesNotCorruptEnclosingParagraph(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?><w:document ` + wNS + `><w:body>` +
		`<w:p><w:r><w:t>before-drawing</w:t>` +
		`<w:drawing><w:txbxContent><w:p><w:r><w:t>inside-textbox</w:t></w:r></w:p></w:txbxContent></w:drawing>` +
		`<w:t>after-drawing</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>Second real paragraph</w:t></w:r></w:p>` +
		`</w:body></w:document>`
	data := buildDocx(t, map[string]string{"word/document.xml": doc})

	got := extractAll(t, data)

	if len(got) != 2 {
		t.Fatalf("got %d paragraphs, want 2 (textbox content must not add a spurious paragraph): %+v", len(got), got)
	}

	wantFirst := "before-drawingafter-drawing"
	if got[0].Text != wantFirst {
		t.Errorf("paragraph 1 text = %q, want %q (textbox-nested content must not corrupt the enclosing paragraph)", got[0].Text, wantFirst)
	}
	if got[0].Location.(paragraphLocation).Paragraph != 1 || got[0].Location.Human() != "Paragraph 1" {
		t.Errorf("paragraph 1 location = %+v, want Paragraph 1", got[0].Location)
	}
	if strings.Contains(got[0].Text, "inside-textbox") {
		t.Errorf("paragraph 1 text = %q must not contain the textbox's own nested content", got[0].Text)
	}

	wantSecond := "Second real paragraph"
	if got[1].Text != wantSecond {
		t.Errorf("paragraph 2 text = %q, want %q", got[1].Text, wantSecond)
	}
	if got[1].Location.(paragraphLocation).Paragraph != 2 || got[1].Location.Human() != "Paragraph 2" {
		t.Errorf("paragraph 2 location = %+v, want Paragraph 2 (numbering must not shift due to the nested paragraph)", got[1].Location)
	}
}

// TestExtractNestedDrawingDepthTracked exercises a doubly-nested drawing
// wrapper (a w:pict inside a w:drawing, an unusual but not impossible
// shape) to confirm suppressDepth is a counter, not a boolean: the
// enclosing paragraph's text must survive intact even when the
// suppressed region itself contains another wrapper.
func TestExtractNestedDrawingDepthTracked(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?><w:document ` + wNS + `><w:body>` +
		`<w:p><w:r><w:t>outer-before</w:t>` +
		`<w:drawing><w:pict><w:txbxContent><w:p><w:r><w:t>nested</w:t></w:r></w:p></w:txbxContent></w:pict></w:drawing>` +
		`<w:t>outer-after</w:t></w:r></w:p>` +
		`</w:body></w:document>`
	data := buildDocx(t, map[string]string{"word/document.xml": doc})

	got := extractAll(t, data)
	if len(got) != 1 {
		t.Fatalf("got %d paragraphs, want 1: %+v", len(got), got)
	}
	want := "outer-beforeouter-after"
	if got[0].Text != want {
		t.Errorf("text = %q, want %q", got[0].Text, want)
	}
}

func TestSniffRenamedExtensionViaRegistryFallback(t *testing.T) {
	data := buildDocx(t, map[string]string{
		"word/document.xml": `<?xml version="1.0" encoding="UTF-8"?><w:document ` + wNS + `><w:body><w:p><w:r><w:t>hi</w:t></w:r></w:p></w:body></w:document>`,
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

	// ".bin" doesn't match Extractor.Extensions()'s hint (".docx"), so
	// this only succeeds via the registry's fallback pass over all
	// extractors, which is exactly what we want to exercise here: Sniff
	// recognizing the format from content alone.
	got, ok := reg.For(path, f, info.Size())
	if !ok {
		t.Fatal("expected registry.For to recognize the renamed docx via content sniffing")
	}
	if got.Name() != "docx" {
		t.Errorf("resolved extractor = %q, want docx", got.Name())
	}
}

func TestExtractContextCancellationStopsCleanly(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?><w:document ` + wNS + `><w:body>` +
		`<w:p><w:r><w:t>one</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>two</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>three</w:t></w:r></w:p>` +
		`</w:body></w:document>`
	data := buildDocx(t, map[string]string{"word/document.xml": doc})

	ctx, cancel := context.WithCancel(context.Background())
	r := bytes.NewReader(data)
	units, errc := (Extractor{}).Extract(ctx, r, int64(len(data)))

	// Take exactly one unit, then cancel; Extract's goroutine must
	// still close both channels rather than blocking forever.
	<-units
	cancel()
	for range units {
		// drain until closed
	}
	if err := <-errc; err != nil {
		t.Fatalf("unexpected error after cancellation: %v", err)
	}
}
