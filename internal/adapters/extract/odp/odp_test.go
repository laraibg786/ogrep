package odp

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

func buildOdp(t *testing.T, mimetype string, parts map[string]string) []byte {
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

const odpNS = `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" ` +
	`xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" ` +
	`xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" ` +
	`xmlns:presentation="urn:oasis:names:tc:opendocument:xmlns:presentation:1.0"`

func wrapContent(body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><office:document-content ` + odpNS + `>` +
		`<office:body><office:presentation>` + body + `</office:presentation></office:body></office:document-content>`
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
	shape := shapeLocation{Slide: 3, Shape: "Title 1"}
	if got, want := shape.Human(), "Slide 3"; got != want {
		t.Errorf("shape Human() = %q, want %q", got, want)
	}
	if got, want := shape.HyperlinkURI("/p.odp", nil), "file:///p.odp#3"; got != want {
		t.Errorf("shape HyperlinkURI() = %q, want %q", got, want)
	}

	notes := notesLocation{Slide: 3}
	if got, want := notes.Human(), "Slide 3 (Notes)"; got != want {
		t.Errorf("notes Human() = %q, want %q", got, want)
	}
	if got := notes.HyperlinkURI("/p.odp", nil); got != "" {
		t.Errorf("notes HyperlinkURI() = %q, want \"\"", got)
	}
}

func TestSniffAcceptsOdpViaMimetype(t *testing.T) {
	data := buildOdp(t, "application/vnd.oasis.opendocument.presentation", map[string]string{
		"content.xml": wrapContent(`<draw:page></draw:page>`),
	})
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.odp", r, int64(len(data))) {
		t.Error("expected Sniff to accept an odp via its mimetype part")
	}
}

func TestSniffRejectsOdtViaMimetype(t *testing.T) {
	data := buildOdp(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(`<draw:page></draw:page>`),
	})
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.odp", r, int64(len(data))) {
		t.Error("expected Sniff to reject an odt package even though it has a content.xml")
	}
}

func TestSniffFallsBackToBodyKindWithoutMimetype(t *testing.T) {
	data := buildOdp(t, "", map[string]string{
		"content.xml": wrapContent(`<draw:page></draw:page>`),
	})
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.odp", r, int64(len(data))) {
		t.Error("expected Sniff to accept an odp lacking a mimetype part via office:body's child element")
	}
}

func TestSniffRejectsNonZipGarbage(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x00, 0xff, 0xfe}
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.odp", r, int64(len(data))) {
		t.Error("expected Sniff to reject non-zip garbage, not panic")
	}
}

func TestSniffRejectsEmptyFile(t *testing.T) {
	r := bytes.NewReader(nil)
	if (Extractor{}).Sniff("file.odp", r, 0) {
		t.Error("expected Sniff to reject an empty file")
	}
}

func TestExtractSlidesInDocumentOrder(t *testing.T) {
	data := buildOdp(t, "application/vnd.oasis.opendocument.presentation", map[string]string{
		"content.xml": wrapContent(
			`<draw:page>` +
				`<draw:frame draw:name="Title 1"><draw:text-box><text:p>Slide One Title</text:p></draw:text-box></draw:frame>` +
				`</draw:page>` +
				`<draw:page>` +
				`<draw:frame draw:name="Title 1"><draw:text-box><text:p>Slide Two Title</text:p></draw:text-box></draw:frame>` +
				`</draw:page>`),
	})

	got := extractAll(t, data)
	if len(got) != 2 {
		t.Fatalf("got %d units, want 2: %+v", len(got), got)
	}
	if got[0].Text != "Slide One Title" || got[0].Location.(shapeLocation).Slide != 1 {
		t.Errorf("unit 0 = %+v, want text 'Slide One Title' slide 1", got[0])
	}
	if got[1].Text != "Slide Two Title" || got[1].Location.(shapeLocation).Slide != 2 {
		t.Errorf("unit 1 = %+v, want text 'Slide Two Title' slide 2", got[1])
	}
	if got[0].Location.(shapeLocation).Shape != "Title 1" {
		t.Errorf("shape name = %q, want %q", got[0].Location.(shapeLocation).Shape, "Title 1")
	}
}

func TestExtractShapeFallbackNameWhenUnnamed(t *testing.T) {
	data := buildOdp(t, "application/vnd.oasis.opendocument.presentation", map[string]string{
		"content.xml": wrapContent(
			`<draw:page>` +
				`<draw:frame><draw:text-box><text:p>first shape</text:p></draw:text-box></draw:frame>` +
				`<draw:frame><draw:text-box><text:p>second shape</text:p></draw:text-box></draw:frame>` +
				`</draw:page>`),
	})

	got := extractAll(t, data)
	if len(got) != 2 {
		t.Fatalf("got %d units, want 2: %+v", len(got), got)
	}
	if got[0].Location.(shapeLocation).Shape != "Shape 1" {
		t.Errorf("shape 0 name = %q, want %q", got[0].Location.(shapeLocation).Shape, "Shape 1")
	}
	if got[1].Location.(shapeLocation).Shape != "Shape 2" {
		t.Errorf("shape 1 name = %q, want %q", got[1].Location.(shapeLocation).Shape, "Shape 2")
	}
}

func TestExtractSpeakerNotes(t *testing.T) {
	data := buildOdp(t, "application/vnd.oasis.opendocument.presentation", map[string]string{
		"content.xml": wrapContent(
			`<draw:page>` +
				`<draw:frame draw:name="Title 1"><draw:text-box><text:p>Slide Title</text:p></draw:text-box></draw:frame>` +
				`<presentation:notes>` +
				`<draw:frame presentation:class="notes"><draw:text-box><text:p>Speaker notes here</text:p></draw:text-box></draw:frame>` +
				`</presentation:notes>` +
				`</draw:page>`),
	})

	got := extractAll(t, data)

	var shapeUnits, notesUnits []domain.TextUnit
	for _, u := range got {
		switch u.Location.(type) {
		case shapeLocation:
			shapeUnits = append(shapeUnits, u)
		case notesLocation:
			notesUnits = append(notesUnits, u)
		}
	}

	if len(shapeUnits) != 1 || shapeUnits[0].Text != "Slide Title" {
		t.Fatalf("shape units = %+v, want single 'Slide Title'", shapeUnits)
	}
	if len(notesUnits) != 1 || notesUnits[0].Text != "Speaker notes here" {
		t.Fatalf("notes units = %+v, want single 'Speaker notes here'", notesUnits)
	}
	if notesUnits[0].Location.(notesLocation).Slide != 1 {
		t.Errorf("notes slide = %d, want 1", notesUnits[0].Location.(notesLocation).Slide)
	}
}

func TestExtractBlankParagraphsSkipped(t *testing.T) {
	data := buildOdp(t, "application/vnd.oasis.opendocument.presentation", map[string]string{
		"content.xml": wrapContent(
			`<draw:page><draw:frame><draw:text-box><text:p>one</text:p><text:p></text:p><text:p>three</text:p></draw:text-box></draw:frame></draw:page>`),
	})

	got := extractAll(t, data)
	if len(got) != 2 {
		t.Fatalf("got %d units, want 2 (blank paragraph skipped): %+v", len(got), got)
	}
	if got[0].Text != "one" || got[1].Text != "three" {
		t.Errorf("texts = %q, %q, want one, three", got[0].Text, got[1].Text)
	}
}

func TestExtractLineBreakSplitsParagraph(t *testing.T) {
	data := buildOdp(t, "application/vnd.oasis.opendocument.presentation", map[string]string{
		"content.xml": wrapContent(
			`<draw:page><draw:frame><draw:text-box><text:p>a<text:line-break/>b</text:p></draw:text-box></draw:frame></draw:page>`),
	})

	got := extractAll(t, data)
	if len(got) != 2 {
		t.Fatalf("got %d units, want 2: %+v", len(got), got)
	}
	if got[0].Text != "a" || got[1].Text != "b" {
		t.Errorf("texts = %q, %q, want a, b", got[0].Text, got[1].Text)
	}
	if got[0].Location.(shapeLocation).Slide != 1 || got[1].Location.(shapeLocation).Slide != 1 {
		t.Errorf("both segments must share slide 1: %+v %+v", got[0].Location, got[1].Location)
	}
}

// TestExtractShapeOrdinalIndependentFromNotes is a regression test for
// Fix #4: a notes-context draw:frame must not consume the slide-body
// shape-ordinal sequence (or vice versa). On the old model, a single
// shared shapeOrdinal counter incremented for every draw:frame
// regardless of whether it was inside presentation:notes, so a slide
// with an unnamed shape, followed by notes containing their own frame,
// followed by another unnamed slide-body shape, mislabeled the second
// slide shape "Shape 3" instead of "Shape 2".
func TestExtractShapeOrdinalIndependentFromNotes(t *testing.T) {
	data := buildOdp(t, "application/vnd.oasis.opendocument.presentation", map[string]string{
		"content.xml": wrapContent(
			`<draw:page>` +
				`<draw:frame><draw:text-box><text:p>first slide shape</text:p></draw:text-box></draw:frame>` +
				`<presentation:notes><draw:frame presentation:class="notes"><draw:text-box><text:p>a note</text:p></draw:text-box></draw:frame></presentation:notes>` +
				`<draw:frame><draw:text-box><text:p>second slide shape</text:p></draw:text-box></draw:frame>` +
				`</draw:page>`),
	})

	got := extractAll(t, data)

	var shapeUnits []domain.TextUnit
	for _, u := range got {
		if _, ok := u.Location.(shapeLocation); ok {
			shapeUnits = append(shapeUnits, u)
		}
	}
	if len(shapeUnits) != 2 {
		t.Fatalf("got %d shape units, want 2: %+v", len(shapeUnits), shapeUnits)
	}
	if got := shapeUnits[0].Location.(shapeLocation).Shape; got != "Shape 1" {
		t.Errorf("first slide shape name = %q, want %q", got, "Shape 1")
	}
	if got := shapeUnits[1].Location.(shapeLocation).Shape; got != "Shape 2" {
		t.Errorf("second slide shape name = %q, want %q (must not be consumed by the notes frame)", got, "Shape 2")
	}
}

// TestExtractImageAltTextDoesNotLeak is a regression test for Fix #1,
// applied to odp for consistency with odt/ods (see that fix's
// discussion): a draw:frame's svg:title/svg:desc (accessible alt-text)
// must not be captured as part of the enclosing shape paragraph's text.
func TestExtractImageAltTextDoesNotLeak(t *testing.T) {
	const svgNS = `xmlns:svg="urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0"`
	data := buildOdp(t, "application/vnd.oasis.opendocument.presentation", map[string]string{
		"content.xml": wrapContent(
			`<draw:page ` + svgNS + `><draw:frame><draw:text-box>` +
				`<text:p>before-image` +
				`<draw:frame><draw:image/><svg:title>Alt text for the picture</svg:title></draw:frame>` +
				`after-image</text:p>` +
				`</draw:text-box></draw:frame></draw:page>`),
	})

	got := extractAll(t, data)
	if len(got) != 1 {
		t.Fatalf("got %d units, want 1: %+v", len(got), got)
	}
	want := "before-imageafter-image"
	if got[0].Text != want {
		t.Errorf("text = %q, want %q (alt-text must not leak into the shape's paragraph)", got[0].Text, want)
	}
}

// TestExtractSpaceRunIsCapped is a regression test for Fix #2: text:s's
// text:c count attribute must be clamped to a reasonable upper bound
// rather than synthesizing an unbounded run of spaces.
func TestExtractSpaceRunIsCapped(t *testing.T) {
	data := buildOdp(t, "application/vnd.oasis.opendocument.presentation", map[string]string{
		"content.xml": wrapContent(
			`<draw:page><draw:frame><draw:text-box>` +
				`<text:p>a<text:s text:c="200000000"/>b</text:p>` +
				`</draw:text-box></draw:frame></draw:page>`),
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
	data := buildOdp(t, "application/vnd.oasis.opendocument.presentation", map[string]string{
		"content.xml": wrapContent(`<draw:page></draw:page>`),
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
		t.Fatal("expected registry.For to recognize the renamed odp via content sniffing")
	}
	if got.Name() != "odp" {
		t.Errorf("resolved extractor = %q, want odp", got.Name())
	}
}

func TestExtractContextCancellationStopsCleanly(t *testing.T) {
	data := buildOdp(t, "application/vnd.oasis.opendocument.presentation", map[string]string{
		"content.xml": wrapContent(
			`<draw:page><draw:frame><draw:text-box><text:p>one</text:p></draw:text-box></draw:frame></draw:page>` +
				`<draw:page><draw:frame><draw:text-box><text:p>two</text:p></draw:text-box></draw:frame></draw:page>` +
				`<draw:page><draw:frame><draw:text-box><text:p>three</text:p></draw:text-box></draw:frame></draw:page>`),
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
