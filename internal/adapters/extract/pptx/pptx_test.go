package pptx

import (
	"bytes"
	"context"
	"strconv"
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

func TestLocationHyperlinkURI(t *testing.T) {
	const path = "/path/presentation.pptx"
	cases := []struct {
		name string
		loc  domain.Location
		want string
	}{
		{"shape", shapeLocation{Slide: 12, Shape: "Title"}, "file:///path/presentation.pptx#12"},
		{"notes", notesLocation{Slide: 12}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.loc.HyperlinkURI(path); got != tc.want {
				t.Errorf("HyperlinkURI() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShapeLocationHyperlinkURIEscapesPath(t *testing.T) {
	loc := shapeLocation{Slide: 2, Shape: "Title"}
	got := loc.HyperlinkURI("/path/my presentation.pptx")
	want := "file:///path/my%20presentation.pptx#2"
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

func TestSniffAcceptsValidPptx(t *testing.T) {
	data := buildPptxSimple(t)
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("deck.pptx", r, int64(len(data))) {
		t.Error("expected Sniff to accept a valid pptx package")
	}
}

func TestSniffAcceptsRenamedExtension(t *testing.T) {
	data := buildPptxSimple(t)
	r := bytes.NewReader(data)
	// The path claims a totally different extension; Sniff must still
	// recognize the package from its content (ppt/presentation.xml),
	// not the path.
	if !(Extractor{}).Sniff("deck.bin", r, int64(len(data))) {
		t.Error("expected Sniff to accept a pptx renamed to .bin, based on content inspection")
	}
}

func TestSniffRejectsCorruptNonZip(t *testing.T) {
	data := []byte("this is not a zip file at all, just garbage bytes")
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("deck.pptx", r, int64(len(data))) {
		t.Error("expected Sniff to reject non-zip content")
	}
}

func TestSniffRejectsZipWithoutPresentation(t *testing.T) {
	// A zip file that is NOT a pptx (e.g. it could be an xlsx or docx,
	// or just an arbitrary zip) must not be claimed.
	data := buildPptx(t, map[string]string{
		"[Content_Types].xml": "<Types/>",
		"some/other/part.xml": "<root/>",
	})
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("deck.pptx", r, int64(len(data))) {
		t.Error("expected Sniff to reject a zip lacking ppt/presentation.xml")
	}
}

// buildPptxSimple builds a minimal one-slide, one-shape pptx fixture.
func buildPptxSimple(t *testing.T) []byte {
	t.Helper()
	parts := baseParts()
	parts["ppt/presentation.xml"] = presentationXML([]string{"rId1"})
	parts["ppt/_rels/presentation.xml.rels"] = presentationRelsXML(map[string]string{
		"rId1": "slides/slide1.xml",
	})
	parts["ppt/slides/slide1.xml"] = slideXML([]shapeFixture{
		{name: "Title 1", paragraphs: []string{"Hello World"}},
	})
	return buildPptx(t, parts)
}

// TestExtractSlideOrderFollowsSldIdLstNotFilenames constructs a
// fixture where the slide part FILENAMES are deliberately out of
// order/name relative to the intended presentation order (slide7.xml
// is 1st, slide2.xml is 2nd, slide9.xml is 3rd), to prove slide
// numbering comes from resolving p:sldIdLst's r:id sequence through
// presentation.xml.rels, not from sorting ppt/slides/slideN.xml
// filenames (which would wrongly give slide2, slide7, slide9 order).
func TestExtractSlideOrderFollowsSldIdLstNotFilenames(t *testing.T) {
	parts := baseParts()
	// Document order: rId1 (-> slide7.xml), rId2 (-> slide2.xml), rId3 (-> slide9.xml).
	parts["ppt/presentation.xml"] = presentationXML([]string{"rId1", "rId2", "rId3"})
	parts["ppt/_rels/presentation.xml.rels"] = presentationRelsXML(map[string]string{
		"rId1": "slides/slide7.xml",
		"rId2": "slides/slide2.xml",
		"rId3": "slides/slide9.xml",
	})
	parts["ppt/slides/slide7.xml"] = slideXML([]shapeFixture{
		{name: "Title 1", paragraphs: []string{"first slide text"}},
	})
	parts["ppt/slides/slide2.xml"] = slideXML([]shapeFixture{
		{name: "Title 1", paragraphs: []string{"second slide text"}},
	})
	parts["ppt/slides/slide9.xml"] = slideXML([]shapeFixture{
		{name: "Content Placeholder 2", paragraphs: []string{"para one", "para two"}},
	})
	data := buildPptx(t, parts)

	units := extractAll(t, data)

	var got []domain.TextUnit
	for _, u := range units {
		if _, ok := u.Location.(shapeLocation); ok {
			got = append(got, u)
		}
	}

	want := []struct {
		slide int
		shape string
		text  string
	}{
		{1, "Title 1", "first slide text"},
		{2, "Title 1", "second slide text"},
		{3, "Content Placeholder 2", "para one"},
		{3, "Content Placeholder 2", "para two"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d shape units, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		u := got[i]
		loc, ok := u.Location.(shapeLocation)
		if !ok {
			t.Fatalf("unit %d location type = %T, want shapeLocation", i, u.Location)
		}
		if loc.Slide != w.slide {
			t.Errorf("unit %d: Slide = %d, want %d", i, loc.Slide, w.slide)
		}
		if loc.Shape != w.shape {
			t.Errorf("unit %d: Shape = %q, want %q", i, loc.Shape, w.shape)
		}
		if u.Text != w.text {
			t.Errorf("unit %d: Text = %q, want %q", i, u.Text, w.text)
		}
	}

	wantHuman3 := `Slide 3 (Shape "Content Placeholder 2")`
	if got[2].Location.Human() != wantHuman3 {
		t.Errorf("Location.Human() = %q, want %q", got[2].Location.Human(), wantHuman3)
	}
}

// TestExtractUnnamedShapeFallsBackToOrdinal exercises the case where a
// shape has no cNvPr name attribute: the shape name should fall back
// to "Shape N" using its 1-based document-order ordinal.
func TestExtractUnnamedShapeFallsBackToOrdinal(t *testing.T) {
	parts := baseParts()
	parts["ppt/presentation.xml"] = presentationXML([]string{"rId1"})
	parts["ppt/_rels/presentation.xml.rels"] = presentationRelsXML(map[string]string{
		"rId1": "slides/slide1.xml",
	})
	parts["ppt/slides/slide1.xml"] = slideXML([]shapeFixture{
		{name: "Title 1", paragraphs: []string{"named shape"}},
		{name: "", paragraphs: []string{"unnamed shape"}},
	})
	data := buildPptx(t, parts)

	units := extractAll(t, data)
	if len(units) != 2 {
		t.Fatalf("got %d units, want 2: %+v", len(units), units)
	}
	loc, ok := units[1].Location.(shapeLocation)
	if !ok {
		t.Fatalf("unit 1 location type = %T, want shapeLocation", units[1].Location)
	}
	if loc.Shape != "Shape 2" {
		t.Errorf("unnamed shape's Shape = %q, want %q", loc.Shape, "Shape 2")
	}
}

// TestExtractNotesAssociatedViaSlideRels constructs a slide whose
// notes part is resolved via the SLIDE's own .rels file, deliberately
// using a notes part filename with a mismatched numeric suffix
// (slide3.xml's notes live at notesSlide9.xml, not notesSlide3.xml) to
// prove notes association isn't just assumed from matching numeric
// suffixes.
func TestExtractNotesAssociatedViaSlideRels(t *testing.T) {
	parts := baseParts()
	parts["ppt/presentation.xml"] = presentationXML([]string{"rId1"})
	parts["ppt/_rels/presentation.xml.rels"] = presentationRelsXML(map[string]string{
		"rId1": "slides/slide3.xml",
	})
	parts["ppt/slides/slide3.xml"] = slideXML([]shapeFixture{
		{name: "Title 1", paragraphs: []string{"slide body text"}},
	})
	parts["ppt/slides/_rels/slide3.xml.rels"] = slideRelsXML("../notesSlides/notesSlide9.xml")
	parts["ppt/notesSlides/notesSlide9.xml"] = notesXML([]shapeFixture{
		{name: "Notes Placeholder 2", paragraphs: []string{"speaker notes text"}},
	})
	data := buildPptx(t, parts)

	units := extractAll(t, data)

	var shapeUnits, notesUnits []domain.TextUnit
	for _, u := range units {
		switch u.Location.(type) {
		case shapeLocation:
			shapeUnits = append(shapeUnits, u)
		case notesLocation:
			notesUnits = append(notesUnits, u)
		}
	}

	if len(shapeUnits) != 1 || shapeUnits[0].Text != "slide body text" {
		t.Fatalf("unexpected shape units: %+v", shapeUnits)
	}
	if len(notesUnits) != 1 {
		t.Fatalf("got %d notes units, want 1: %+v", len(notesUnits), notesUnits)
	}
	note := notesUnits[0]
	if note.Text != "speaker notes text" {
		t.Errorf("notes Text = %q, want %q", note.Text, "speaker notes text")
	}
	noteLoc, ok := note.Location.(notesLocation)
	if !ok {
		t.Fatalf("notes location type = %T, want notesLocation", note.Location)
	}
	if noteLoc.Slide != 1 {
		t.Errorf("notes Location.Slide = %d, want 1 (the slide's presentation-order position)", noteLoc.Slide)
	}
	wantHuman := "Slide 1 (Notes)"
	if note.Location.Human() != wantHuman {
		t.Errorf("notes Location.Human() = %q, want %q", note.Location.Human(), wantHuman)
	}
}

// TestExtractMultipleParagraphsInOneShape confirms a single shape's
// txBody containing several <a:p> paragraphs produces one TextUnit per
// paragraph, in document order.
func TestExtractMultipleParagraphsInOneShape(t *testing.T) {
	parts := baseParts()
	parts["ppt/presentation.xml"] = presentationXML([]string{"rId1"})
	parts["ppt/_rels/presentation.xml.rels"] = presentationRelsXML(map[string]string{
		"rId1": "slides/slide1.xml",
	})
	parts["ppt/slides/slide1.xml"] = slideXML([]shapeFixture{
		{name: "Body", paragraphs: []string{"first bullet", "second bullet", "third bullet"}},
	})
	data := buildPptx(t, parts)

	units := extractAll(t, data)
	if len(units) != 3 {
		t.Fatalf("got %d units, want 3: %+v", len(units), units)
	}
	want := []string{"first bullet", "second bullet", "third bullet"}
	for i, w := range want {
		if units[i].Text != w {
			t.Errorf("unit %d Text = %q, want %q", i, units[i].Text, w)
		}
		loc, ok := units[i].Location.(shapeLocation)
		if !ok {
			t.Fatalf("unit %d location type = %T, want shapeLocation", i, units[i].Location)
		}
		if loc.Shape != "Body" {
			t.Errorf("unit %d Shape = %q, want %q", i, loc.Shape, "Body")
		}
	}
}

// TestExtractEmptyParagraphsSkipped confirms paragraphs with no text
// content don't produce empty TextUnits.
func TestExtractEmptyParagraphsSkipped(t *testing.T) {
	parts := baseParts()
	parts["ppt/presentation.xml"] = presentationXML([]string{"rId1"})
	parts["ppt/_rels/presentation.xml.rels"] = presentationRelsXML(map[string]string{
		"rId1": "slides/slide1.xml",
	})
	parts["ppt/slides/slide1.xml"] = slideXML([]shapeFixture{
		{name: "Body", paragraphs: []string{"", "has text", ""}},
	})
	data := buildPptx(t, parts)

	units := extractAll(t, data)
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1 (empty paragraphs should be skipped): %+v", len(units), units)
	}
	if units[0].Text != "has text" {
		t.Errorf("Text = %q, want %q", units[0].Text, "has text")
	}
}

// TestExtractOnCorruptZipReportsError confirms Extract reports an
// error (not a panic) when the input isn't a valid zip at all.
func TestExtractOnCorruptZipReportsError(t *testing.T) {
	data := []byte("not a zip")
	r := bytes.NewReader(data)
	units, errc := (Extractor{}).Extract(context.Background(), r, int64(len(data)))

	for range units {
		t.Error("expected no units from a corrupt zip")
	}
	if err := <-errc; err == nil {
		t.Error("expected an error extracting a corrupt zip, got nil")
	}
}

// TestExtractRespectsContextCancellation confirms the units and error
// channels are both closed promptly, without leaking, once the caller
// cancels the context partway through a multi-slide extraction.
func TestExtractRespectsContextCancellation(t *testing.T) {
	parts := baseParts()
	parts["ppt/presentation.xml"] = presentationXML([]string{"rId1", "rId2", "rId3"})
	parts["ppt/_rels/presentation.xml.rels"] = presentationRelsXML(map[string]string{
		"rId1": "slides/slide1.xml",
		"rId2": "slides/slide2.xml",
		"rId3": "slides/slide3.xml",
	})
	for i := 1; i <= 3; i++ {
		parts[sliceSlidePath(i)] = slideXML([]shapeFixture{
			{name: "Title 1", paragraphs: []string{"text"}},
		})
	}
	data := buildPptx(t, parts)

	ctx, cancel := context.WithCancel(context.Background())
	units, errc := (Extractor{}).Extract(ctx, bytes.NewReader(data), int64(len(data)))

	<-units // read exactly one unit
	cancel()

	for range units {
	}
	<-errc
}

func sliceSlidePath(n int) string {
	return "ppt/slides/slide" + strconv.Itoa(n) + ".xml"
}

// extractAll drains Extract fully and fails the test on any error.
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
