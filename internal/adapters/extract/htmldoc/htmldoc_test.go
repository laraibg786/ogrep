package htmldoc

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// --- Sniff: accept cases ---

func TestSniffAcceptsSimpleHTML(t *testing.T) {
	data := []byte(`<html><body><p>hello</p></body></html>`)
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.html", r, int64(len(data))) {
		t.Error("expected Sniff to accept a simple HTML document")
	}
}

func TestSniffAcceptsDoctype(t *testing.T) {
	data := []byte("<!DOCTYPE html>\n<html><body><p>hello</p></body></html>")
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.html", r, int64(len(data))) {
		t.Error("expected Sniff to accept a document with a leading doctype")
	}
}

func TestSniffAcceptsLeadingComment(t *testing.T) {
	data := []byte("<!-- a comment -->\n<p>hello</p>")
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.html", r, int64(len(data))) {
		t.Error("expected Sniff to accept a document starting with a comment")
	}
}

func TestSniffAcceptsFragment(t *testing.T) {
	data := []byte(`<p>hello</p><p>world</p>`)
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.html", r, int64(len(data))) {
		t.Error("expected Sniff to accept a bare HTML fragment with no <html> wrapper")
	}
}

func TestSniffAcceptsLeadingWhitespaceAndBOM(t *testing.T) {
	data := append([]byte{0xEF, 0xBB, 0xBF}, []byte("   \n\t<p>hello</p>")...)
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.html", r, int64(len(data))) {
		t.Error("expected Sniff to accept HTML preceded by a BOM and leading whitespace")
	}
}

func TestSniffAcceptsLargeReportedSize(t *testing.T) {
	data := []byte(`<p>hello</p>`)
	r := bytes.NewReader(data)
	const largerThanTypical = 65 * 1024 * 1024
	if !(Extractor{}).Sniff("file.html", r, largerThanTypical) {
		t.Error("expected Sniff to accept regardless of reported size")
	}
}

// --- Sniff: reject cases ---

func TestSniffRejectsEmptyFile(t *testing.T) {
	data := []byte{}
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.html", r, int64(len(data))) {
		t.Error("expected Sniff to reject an empty file")
	}
}

func TestSniffRejectsPlainText(t *testing.T) {
	data := []byte("Jane Doe")
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.html", r, int64(len(data))) {
		t.Error("expected Sniff to reject plain text not starting with '<'")
	}
}

func TestSniffRejectsInvalidTagStartChar(t *testing.T) {
	data := []byte(`<3 hours ago`)
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.html", r, int64(len(data))) {
		t.Error("expected Sniff to reject input starting with '<' but not a valid tag/comment/doctype start")
	}
}

// --- Extract: helpers ---

func extractAll(t *testing.T, data []byte) []domain.TextUnit {
	t.Helper()
	r := bytes.NewReader(data)
	units, errc := (Extractor{}).Extract(context.Background(), r, int64(len(data)))

	var got []domain.TextUnit
	for u := range units {
		got = append(got, u)
	}
	if err := <-errc; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return got
}

func findByPath(units []domain.TextUnit, path string) (domain.TextUnit, bool) {
	for _, u := range units {
		if loc, ok := u.Location.(htmlPathLocation); ok && loc.Path == path {
			return u, true
		}
	}
	return domain.TextUnit{}, false
}

func allTexts(units []domain.TextUnit) []string {
	var out []string
	for _, u := range units {
		out = append(out, u.Text)
	}
	return out
}

// --- Extract: path synthesis correctness ---

func TestExtractSimpleNestedElements(t *testing.T) {
	data := []byte(`<html><body><p>Hello</p></body></html>`)
	got := extractAll(t, data)

	u, ok := findByPath(got, "html:nth-of-type(1)>body:nth-of-type(1)>p:nth-of-type(1)")
	if !ok {
		t.Fatalf("no unit found at expected path; units: %+v", got)
	}
	if u.Text != "Hello" {
		t.Errorf("text = %q, want %q", u.Text, "Hello")
	}
}

func TestExtractMultipleSameTagSiblingsGetIndices(t *testing.T) {
	data := []byte(`<div><p>one</p><p>two</p></div>`)
	got := extractAll(t, data)

	u1, ok := findByPath(got, "div:nth-of-type(1)>p:nth-of-type(1)")
	if !ok {
		t.Fatalf("no unit at div/p[1]; units: %+v", got)
	}
	if u1.Text != "one" {
		t.Errorf("p[1] text = %q, want %q", u1.Text, "one")
	}

	u2, ok := findByPath(got, "div:nth-of-type(1)>p:nth-of-type(2)")
	if !ok {
		t.Fatalf("no unit at div/p[2]; units: %+v", got)
	}
	if u2.Text != "two" {
		t.Errorf("p[2] text = %q, want %q", u2.Text, "two")
	}
}

// TestExtractTopLevelFragmentSiblingsGetIndices confirms top-level
// elements are indexed too, unlike xmldoc's XPath root (which never
// gets an index, since XML guarantees exactly one root): a bare HTML
// fragment has no such guarantee.
func TestExtractTopLevelFragmentSiblingsGetIndices(t *testing.T) {
	data := []byte(`<p>one</p><p>two</p>`)
	got := extractAll(t, data)

	if _, ok := findByPath(got, "p:nth-of-type(1)"); !ok {
		t.Fatalf("no unit at p[1]; units: %+v", got)
	}
	if _, ok := findByPath(got, "p:nth-of-type(2)"); !ok {
		t.Fatalf("no unit at p[2]; units: %+v", got)
	}
}

// TestExtractTextSplitAcrossSiblingInlineTags confirms an element's
// direct text is assembled from every run before/between/after a
// nested inline element, and that the inline element gets its own
// separate unit rather than being folded into its parent's text.
func TestExtractTextSplitAcrossSiblingInlineTags(t *testing.T) {
	data := []byte(`<p>Hello <b>World</b> Goodbye</p>`)
	got := extractAll(t, data)

	up, ok := findByPath(got, "p:nth-of-type(1)")
	if !ok {
		t.Fatalf("no unit at p; units: %+v", got)
	}
	if up.Text != "Hello Goodbye" {
		t.Errorf("p text = %q, want %q", up.Text, "Hello Goodbye")
	}

	ub, ok := findByPath(got, "p:nth-of-type(1)>b:nth-of-type(1)")
	if !ok {
		t.Fatalf("no unit at p>b; units: %+v", got)
	}
	if ub.Text != "World" {
		t.Errorf("b text = %q, want %q", ub.Text, "World")
	}
}

func TestExtractDirectTextOnlyNotDescendant(t *testing.T) {
	data := []byte(`<a>outer<b>inner</b></a>`)
	got := extractAll(t, data)

	ub, ok := findByPath(got, "a:nth-of-type(1)>b:nth-of-type(1)")
	if !ok {
		t.Fatalf("no unit at a>b; units: %+v", got)
	}
	if ub.Text != "inner" {
		t.Errorf("a>b text = %q, want %q", ub.Text, "inner")
	}

	ua, ok := findByPath(got, "a:nth-of-type(1)")
	if !ok {
		t.Fatalf("no unit at a; units: %+v", got)
	}
	if ua.Text != "outer" {
		t.Errorf("a text = %q, want %q (must not include descendant text)", ua.Text, "outer")
	}
	if strings.Contains(ua.Text, "inner") {
		t.Errorf("a text = %q must not contain descendant text %q", ua.Text, "inner")
	}
}

func TestExtractWhitespaceOnlyDirectTextIsSkipped(t *testing.T) {
	data := []byte("<div>\n  <p>x</p>\n</div>")
	got := extractAll(t, data)

	if _, ok := findByPath(got, "div:nth-of-type(1)"); ok {
		t.Error("expected no unit at div, since its only direct text is whitespace")
	}

	u, ok := findByPath(got, "div:nth-of-type(1)>p:nth-of-type(1)")
	if !ok {
		t.Fatalf("no unit at div>p; units: %+v", got)
	}
	if u.Text != "x" {
		t.Errorf("div>p text = %q, want %q", u.Text, "x")
	}
}

// --- Extract: script/style exclusion ---

func TestExtractSkipsScriptContent(t *testing.T) {
	data := []byte(`<html><body><script>var x = "matchme";</script><p>matchme too</p></body></html>`)
	got := extractAll(t, data)

	for _, text := range allTexts(got) {
		if strings.Contains(text, "var x") {
			t.Errorf("script content leaked into extracted text: %q", text)
		}
	}
	u, ok := findByPath(got, "html:nth-of-type(1)>body:nth-of-type(1)>p:nth-of-type(1)")
	if !ok {
		t.Fatalf("no unit at html>body>p; units: %+v", got)
	}
	if u.Text != "matchme too" {
		t.Errorf("p text = %q, want %q", u.Text, "matchme too")
	}
}

func TestExtractSkipsStyleContent(t *testing.T) {
	data := []byte(`<html><head><style>body { color: red; }</style></head><body><p>content</p></body></html>`)
	got := extractAll(t, data)

	for _, text := range allTexts(got) {
		if strings.Contains(text, "color") {
			t.Errorf("style content leaked into extracted text: %q", text)
		}
	}
	if len(got) != 1 || got[0].Text != "content" {
		t.Errorf("got units %+v, want exactly one unit with text %q", got, "content")
	}
}

// TestExtractScriptWithMarkupLikeContentIsNotParsedAsTags confirms
// script/style content is treated as opaque raw text by the
// tokenizer, not scanned for nested tags -- e.g. a "</p>" inside a JS
// string literal must not be misread as a real closing tag.
func TestExtractScriptWithMarkupLikeContentIsNotParsedAsTags(t *testing.T) {
	data := []byte(`<div><script>var s = "<p>not a tag</p>";</script><p>real</p></div>`)
	got := extractAll(t, data)

	u, ok := findByPath(got, "div:nth-of-type(1)>p:nth-of-type(1)")
	if !ok {
		t.Fatalf("no unit at div>p; units: %+v", got)
	}
	if u.Text != "real" {
		t.Errorf("div>p text = %q, want %q", u.Text, "real")
	}
}

// --- Extract: <br> as a manual line break ---

func TestExtractBrSplitsIntoSeparateUnitsSharingLocation(t *testing.T) {
	data := []byte(`<p>Line one<br>Line two</p>`)
	got := extractAll(t, data)

	var matches []domain.TextUnit
	for _, u := range got {
		if loc, ok := u.Location.(htmlPathLocation); ok && loc.Path == "p:nth-of-type(1)" {
			matches = append(matches, u)
		}
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 units sharing the p location, got %d: %+v", len(matches), matches)
	}
	if matches[0].Text != "Line one" || matches[1].Text != "Line two" {
		t.Errorf("texts = %q, %q; want %q, %q", matches[0].Text, matches[1].Text, "Line one", "Line two")
	}
	for _, u := range matches {
		if strings.Contains(u.Text, "\n") {
			t.Errorf("unit text %q must not contain an embedded newline", u.Text)
		}
	}
}

// --- Extract: void elements ---

func TestExtractVoidElementsProduceNoUnit(t *testing.T) {
	data := []byte(`<div><img src="x.png"><input type="text"><hr><p>text</p></div>`)
	got := extractAll(t, data)

	if len(got) != 1 || got[0].Text != "text" {
		t.Errorf("got units %+v, want exactly one unit with text %q", got, "text")
	}
}

// --- Extract: malformed-but-browser-tolerable HTML ---

// TestExtractUnclosedTagIsTolerated is the key regression proving this
// package isn't just wrapping a strict XML parser: encoding/xml would
// error on a mismatched closing tag, but real browsers (and this
// tokenizer) recover from an unclosed <p> just fine.
func TestExtractUnclosedTagIsTolerated(t *testing.T) {
	data := []byte(`<div><p>Hello<span>World</span></div>`)
	units, errc := (Extractor{}).Extract(context.Background(), bytes.NewReader(data), int64(len(data)))

	var got []domain.TextUnit
	for u := range units {
		got = append(got, u)
	}
	if err := <-errc; err != nil {
		t.Fatalf("expected no error for browser-tolerable malformed HTML, got: %v", err)
	}

	if _, ok := findByPath(got, "div:nth-of-type(1)>p:nth-of-type(1)"); !ok {
		t.Errorf("expected div>p unit; units: %+v", got)
	}
	u, ok := findByPath(got, "div:nth-of-type(1)>p:nth-of-type(1)>span:nth-of-type(1)")
	if !ok {
		t.Fatalf("expected div>p>span unit; units: %+v", got)
	}
	if u.Text != "World" {
		t.Errorf("span text = %q, want %q", u.Text, "World")
	}
}

// TestExtractUnclosedTagAtEndOfDocument confirms an element left open
// all the way to EOF (no closing tag anywhere) still gets its
// accumulated text flushed, rather than being silently dropped.
func TestExtractUnclosedTagAtEndOfDocument(t *testing.T) {
	data := []byte(`<div><p>Hello`)
	got := extractAll(t, data)

	u, ok := findByPath(got, "div:nth-of-type(1)>p:nth-of-type(1)")
	if !ok {
		t.Fatalf("expected div>p unit even though never closed; units: %+v", got)
	}
	if u.Text != "Hello" {
		t.Errorf("p text = %q, want %q", u.Text, "Hello")
	}
}

// TestExtractStrayEndTagIsIgnored confirms an end tag with no matching
// open element on the stack (e.g. a typo, or content copy-pasted out of
// context) is simply ignored, as a browser would, rather than causing
// an error or corrupting the stack.
func TestExtractStrayEndTagIsIgnored(t *testing.T) {
	data := []byte(`</span><p>Hello</p>`)
	got := extractAll(t, data)

	u, ok := findByPath(got, "p:nth-of-type(1)")
	if !ok {
		t.Fatalf("expected p unit despite the leading stray end tag; units: %+v", got)
	}
	if u.Text != "Hello" {
		t.Errorf("p text = %q, want %q", u.Text, "Hello")
	}
}

// --- Extract: entities ---

func TestExtractUnescapesEntities(t *testing.T) {
	data := []byte(`<p>A &amp; B &lt;tag&gt;</p>`)
	got := extractAll(t, data)

	u, ok := findByPath(got, "p:nth-of-type(1)")
	if !ok {
		t.Fatalf("no unit at p; units: %+v", got)
	}
	if u.Text != "A & B <tag>" {
		t.Errorf("text = %q, want %q", u.Text, "A & B <tag>")
	}
}

// --- Extract: real line/column position tracking ---

// TestExtractSimpleCaseReportsRealLine confirms a trivial single-line
// document reports a real, correct source line via Human() -- i.e. the
// "position unknown" limitation this package used to have (fragile,
// per an earlier, incorrect version of the package doc comment) is
// actually fixed, not just re-described.
func TestExtractSimpleCaseReportsRealLine(t *testing.T) {
	data := []byte(`<html><body><p>Hello</p></body></html>`)
	got := extractAll(t, data)

	u, ok := findByPath(got, "html:nth-of-type(1)>body:nth-of-type(1)>p:nth-of-type(1)")
	if !ok {
		t.Fatalf("no unit found; units: %+v", got)
	}
	loc := u.Location.(htmlPathLocation)
	if got, want := loc.Human(), "1"; got != want {
		t.Errorf("Human() = %q, want %q (single-line document)", got, want)
	}
}

// TestExtractMultiLineDocumentReportsDistinctLines confirms different
// matches in a multi-line, pretty-printed document report genuinely
// different, correct source line numbers -- and specifically that the
// reported line is the line the text itself begins on, not the line its
// enclosing start tag appeared on (line 3's "<p>" opens a tag whose own
// text "First" doesn't start until line 4).
func TestExtractMultiLineDocumentReportsDistinctLines(t *testing.T) {
	data := []byte("<html>\n<body>\n<p>\nFirst\n</p>\n<p>Second</p>\n</body>\n</html>")
	got := extractAll(t, data)

	u1, ok := findByPath(got, "html:nth-of-type(1)>body:nth-of-type(1)>p:nth-of-type(1)")
	if !ok {
		t.Fatalf("no unit at p[1]; units: %+v", got)
	}
	if u1.Text != "First" {
		t.Errorf("p[1] text = %q, want %q", u1.Text, "First")
	}
	if line := u1.Location.(htmlPathLocation).Line; line != 4 {
		t.Errorf("p[1] line = %d, want 4 (the line \"First\" itself starts on, not line 3 where <p> opened)", line)
	}

	u2, ok := findByPath(got, "html:nth-of-type(1)>body:nth-of-type(1)>p:nth-of-type(2)")
	if !ok {
		t.Fatalf("no unit at p[2]; units: %+v", got)
	}
	if u2.Text != "Second" {
		t.Errorf("p[2] text = %q, want %q", u2.Text, "Second")
	}
	if line := u2.Location.(htmlPathLocation).Line; line != 6 {
		t.Errorf("p[2] line = %d, want 6", line)
	}

	if u1.Location.(htmlPathLocation).Line == u2.Location.(htmlPathLocation).Line {
		t.Errorf("p[1] and p[2] must report distinct lines, both got %d", u1.Location.(htmlPathLocation).Line)
	}
}

// TestExtractBrSplitLinesReportDistinctSourceLines confirms the two
// units a <br> splits one element's text into (see
// TestExtractBrSplitsIntoSeparateUnitsSharingLocation) report their own,
// individually-correct source lines rather than sharing one -- since a
// <br> is itself a real break, moving to a genuinely different line.
func TestExtractBrSplitLinesReportDistinctSourceLines(t *testing.T) {
	data := []byte("<p>Line one\n<br>\nLine two</p>")
	got := extractAll(t, data)

	var matches []domain.TextUnit
	for _, u := range got {
		if loc, ok := u.Location.(htmlPathLocation); ok && loc.Path == "p:nth-of-type(1)" {
			matches = append(matches, u)
		}
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 units sharing the p location, got %d: %+v", len(matches), matches)
	}
	line1 := matches[0].Location.(htmlPathLocation).Line
	line2 := matches[1].Location.(htmlPathLocation).Line
	if line1 != 1 {
		t.Errorf("first br-split unit line = %d, want 1", line1)
	}
	if line2 != 3 {
		t.Errorf("second br-split unit line = %d, want 3", line2)
	}
}

// TestExtractPreLinesReportDistinctSourceLines confirms <pre>'s two
// preserved literal lines (see TestExtractPreservesWhitespaceVerbatimInsidePre)
// each report their own correct source line, even though both literal
// newlines arrive within a single z.Text() token (pre's content is not
// interrupted by any child element here).
func TestExtractPreLinesReportDistinctSourceLines(t *testing.T) {
	data := []byte("<div>\n<pre>col1    col2\nrow2    val2</pre>\n</div>")
	got := extractAll(t, data)

	var matches []domain.TextUnit
	for _, u := range got {
		if loc, ok := u.Location.(htmlPathLocation); ok && loc.Path == "div:nth-of-type(1)>pre:nth-of-type(1)" {
			matches = append(matches, u)
		}
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 units for the two pre lines, got %d: %+v", len(matches), matches)
	}
	if line := matches[0].Location.(htmlPathLocation).Line; line != 2 {
		t.Errorf("first pre line = %d, want 2", line)
	}
	if line := matches[1].Location.(htmlPathLocation).Line; line != 3 {
		t.Errorf("second pre line = %d, want 3", line)
	}
}

// TestExtractHyperlinkURIHasRealLineColSuffix confirms HyperlinkURI now
// produces a genuine ":line:col" suffix (mirroring text.go's
// lineLocation.HyperlinkURI), not the old unconditional bare-URI
// fallback -- with a real line and a column of 1 (see location.go's
// Fields doc comment for why column 1, not a span-derived offset, is
// the honest answer for this package).
func TestExtractHyperlinkURIHasRealLineColSuffix(t *testing.T) {
	data := []byte("<html>\n<body>\n<p>Hello world</p>\n</body>\n</html>")
	got := extractAll(t, data)

	u, ok := findByPath(got, "html:nth-of-type(1)>body:nth-of-type(1)>p:nth-of-type(1)")
	if !ok {
		t.Fatalf("no unit found; units: %+v", got)
	}
	loc := u.Location.(htmlPathLocation)
	spans := []domain.Span{{Start: 6, End: 11}} // "world" within "Hello world"
	got2 := loc.HyperlinkURI("/tmp/page.html", spans)
	want := domain.FileURI("/tmp/page.html", "") + ":3:1"
	if got2 != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got2, want)
	}
}

// --- Location ---

// TestHTMLPathLocationHumanKnownLine confirms Human() renders just the
// line number -- the same grep-style "path:N" convention text.go's
// lineLocation uses (see domain.LocationString) -- once a real line is
// known, restoring the contract every other line-oriented format in
// this repo honors. The CSS tag path itself moves to Fields()/JSON (see
// TestHTMLPathLocationFieldsUsesHtmlpathNotPath below), not Human().
func TestHTMLPathLocationHumanKnownLine(t *testing.T) {
	loc := htmlPathLocation{Path: "html>body>p:nth-of-type(2)", Line: 47}
	if got, want := loc.Human(), "47"; got != want {
		t.Errorf("Human() = %q, want %q", got, want)
	}
}

// TestHTMLPathLocationHumanUnknownLine confirms the defensive fallback
// (Line == 0, which shouldn't happen in practice -- see the type doc
// comment) still renders something useful: the tag path, rather than
// "0" or an empty string.
func TestHTMLPathLocationHumanUnknownLine(t *testing.T) {
	loc := htmlPathLocation{Path: "html>body>p:nth-of-type(2)", Line: 0}
	if got, want := loc.Human(), "html>body>p:nth-of-type(2)"; got != want {
		t.Errorf("Human() = %q, want %q", got, want)
	}
}

// TestHTMLPathLocationFieldsUsesHtmlpathNotPath is a focused regression
// for the JSON field-name fix: internal/adapters/output/json.go's
// WriteMatch seeds the record with a top-level "path" key holding the
// match's own file path, then merges every key Location.Fields()
// returns directly on top of that same map -- so a Location using the
// literal key "path" for anything else would silently overwrite the
// file path in JSON output (exactly the collision jsondoc/yamldoc/
// xmldoc's own path-shaped Fields() keys -- "jsonpath"/"yamlpath"/
// "xpath" -- are named to avoid). This asserts the html tag path is
// keyed "htmlpath", and that no "path" key is present at all in what
// Fields() itself returns (WriteMatch supplies that key from m.Path,
// never from Location).
func TestHTMLPathLocationFieldsUsesHtmlpathNotPath(t *testing.T) {
	loc := htmlPathLocation{Path: "html>body>p:nth-of-type(2)", Line: 47}
	fields := loc.Fields(nil)

	htmlpath, ok := fields["htmlpath"].(string)
	if !ok || htmlpath != "html>body>p:nth-of-type(2)" {
		t.Errorf(`Fields()["htmlpath"] = %v (%T), want string %q`, fields["htmlpath"], fields["htmlpath"], "html>body>p:nth-of-type(2)")
	}
	if _, present := fields["path"]; present {
		t.Errorf(`Fields() must not return a "path" key (collides with the match's own file path in json.go's WriteMatch), got %v`, fields)
	}
}

// TestHTMLPathLocationFieldsLineAndCol confirms col is always 1
// regardless of spans -- see location.go's Fields doc comment for why:
// this package's Text is decoded/whitespace-collapsed, so a span offset
// into it has no reliable relationship to a real source column.
func TestHTMLPathLocationFieldsLineAndCol(t *testing.T) {
	loc := htmlPathLocation{Path: "html>body>p:nth-of-type(2)", Line: 47}

	fields := loc.Fields(nil)
	if line, ok := fields["line"].(int); !ok || line != 47 {
		t.Errorf(`Fields()["line"] = %v (%T), want int 47`, fields["line"], fields["line"])
	}
	if col, ok := fields["col"].(int); !ok || col != 1 {
		t.Errorf(`Fields()["col"] = %v (%T), want int 1 (no spans given)`, fields["col"], fields["col"])
	}

	fields = loc.Fields([]domain.Span{{Start: 4, End: 9}})
	if col, ok := fields["col"].(int); !ok || col != 1 {
		t.Errorf(`Fields()["col"] with a span given = %v (%T), want int 1 still`, fields["col"], fields["col"])
	}
}

func TestHTMLPathLocationHyperlinkURIWithLine(t *testing.T) {
	loc := htmlPathLocation{Path: "html>body>p:nth-of-type(2)", Line: 47}
	got := loc.HyperlinkURI("/path/file.html", []domain.Span{{Start: 4, End: 9}})
	want := domain.FileURI("/path/file.html", "") + ":47:1"
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

// TestHTMLPathLocationHyperlinkURIUnknownLine confirms the defensive
// "position unavailable" fallback (Line == 0) still produces a bare
// file:// URI with no ":line:col" suffix, rather than claiming line 0
// (or line 1) is where the match is.
func TestHTMLPathLocationHyperlinkURIUnknownLine(t *testing.T) {
	loc := htmlPathLocation{Path: "html>body>p:nth-of-type(2)", Line: 0}
	got := loc.HyperlinkURI("/path/file.html", nil)
	want := domain.FileURI("/path/file.html", "")
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
	if strings.Contains(got, "#") {
		t.Errorf("HyperlinkURI() = %q, want no fragment", got)
	}
}

// --- Extract: expanded raw-text element list (fix #1) ---

// TestExtractSkipsNoscriptContent confirms <noscript> content -- which
// the tokenizer treats as raw text just like <script>/<style> -- never
// becomes searchable text.
func TestExtractSkipsNoscriptContent(t *testing.T) {
	data := []byte(`<div><noscript><img src="x"><p>fallback markup</p></noscript><p>real</p></div>`)
	got := extractAll(t, data)

	for _, text := range allTexts(got) {
		if strings.Contains(text, "fallback") {
			t.Errorf("noscript content leaked into extracted text: %q", text)
		}
	}
	u, ok := findByPath(got, "div:nth-of-type(1)>p:nth-of-type(1)")
	if !ok {
		t.Fatalf("expected div>p unit; units: %+v", got)
	}
	if u.Text != "real" {
		t.Errorf("div>p text = %q, want %q", u.Text, "real")
	}
}

// TestExtractSkipsPlaintextContentAndTrailingContent confirms
// <plaintext> content is excluded from extraction, and that -- per the
// HTML5 spec, plaintext consumes everything up to end-of-document, with
// no possible closing tag -- content written after it in the source
// (here, what looks like a <script> and a <p>) is not crashed on and
// does not leak into any emitted unit either, since it's all part of
// the same raw plaintext span.
func TestExtractSkipsPlaintextContentAndTrailingContent(t *testing.T) {
	data := []byte(`<div><p>before</p><plaintext>raw <script>var x = "matchme"</script> <p>not a real tag</p></div>`)
	got := extractAll(t, data)

	u, ok := findByPath(got, "div:nth-of-type(1)>p:nth-of-type(1)")
	if !ok {
		t.Fatalf("expected div>p (before plaintext) unit; units: %+v", got)
	}
	if u.Text != "before" {
		t.Errorf("div>p text = %q, want %q", u.Text, "before")
	}
	for _, text := range allTexts(got) {
		if strings.Contains(text, "matchme") || strings.Contains(text, "raw") || strings.Contains(text, "not a real tag") {
			t.Errorf("plaintext content leaked into extracted text: %q", text)
		}
	}
}

// --- Extract: self-closing tag handling (fix #2) ---

// TestExtractSelfClosingNonVoidElementIsTreatedAsOpenTag confirms a
// self-closing token on an ordinary (non-void) element like <div/> is
// treated exactly like <div> per HTML5 -- NOT as an empty element with
// no content -- so subsequent content (here, "text inside" and a later
// <p>) is not silently dropped.
func TestExtractSelfClosingNonVoidElementIsTreatedAsOpenTag(t *testing.T) {
	data := []byte(`<div/>text inside<p>para</p></div>`)
	got := extractAll(t, data)

	u, ok := findByPath(got, "div:nth-of-type(1)")
	if !ok {
		t.Fatalf("expected a div unit; units: %+v", got)
	}
	if u.Text != "text inside" {
		t.Errorf("div text = %q, want %q", u.Text, "text inside")
	}
	up, ok := findByPath(got, "div:nth-of-type(1)>p:nth-of-type(1)")
	if !ok {
		t.Fatalf("expected div>p unit; units: %+v", got)
	}
	if up.Text != "para" {
		t.Errorf("div>p text = %q, want %q", up.Text, "para")
	}
}

// TestExtractSelfClosingVoidElementStillProducesNoUnit confirms actual
// void elements are unaffected by the fix above: a self-closed (or not)
// void element still gets no frame/unit and doesn't disturb sibling
// counters.
func TestExtractSelfClosingVoidElementStillProducesNoUnit(t *testing.T) {
	data := []byte(`<div><br/><img src="x.png"/><p>one</p><p>two</p></div>`)
	got := extractAll(t, data)

	if _, ok := findByPath(got, "div:nth-of-type(1)>img:nth-of-type(1)"); ok {
		t.Error("expected no unit/frame for a self-closed void <img/>")
	}
	if _, ok := findByPath(got, "div:nth-of-type(1)>p:nth-of-type(1)"); !ok {
		t.Errorf("expected div>p[1] unit despite preceding void elements; units: %+v", got)
	}
	if _, ok := findByPath(got, "div:nth-of-type(1)>p:nth-of-type(2)"); !ok {
		t.Errorf("expected div>p[2] unit despite preceding void elements; units: %+v", got)
	}
}

// --- Extract: whitespace collapsing vs. real <br> breaks (fix #3) ---

// TestExtractWrappedSourceLineIsOneUnit confirms a text node that merely
// wraps across multiple source lines (a literal '\n' that is not a
// <br>) is collapsed to a single space, not split into separate units --
// so a phrase spanning the wrap is still searchable as one phrase.
func TestExtractWrappedSourceLineIsOneUnit(t *testing.T) {
	data := []byte("<p>The quick\nbrown fox</p>")
	got := extractAll(t, data)

	if len(got) != 1 {
		t.Fatalf("expected exactly one unit for a source-wrapped line, got %d: %+v", len(got), got)
	}
	if got[0].Text != "The quick brown fox" {
		t.Errorf("text = %q, want %q", got[0].Text, "The quick brown fox")
	}
}

// TestExtractBrStillSplitsAfterWhitespaceCollapseFix re-confirms the
// existing <br>-splits-into-separate-units behavior still holds now
// that ordinary embedded newlines are collapsed instead of split: a
// literal source newline around the <br> must collapse, while the <br>
// itself must still produce two units.
func TestExtractBrStillSplitsAfterWhitespaceCollapseFix(t *testing.T) {
	data := []byte("<p>Line one\n<br>\nLine two</p>")
	got := extractAll(t, data)

	var matches []domain.TextUnit
	for _, u := range got {
		if loc, ok := u.Location.(htmlPathLocation); ok && loc.Path == "p:nth-of-type(1)" {
			matches = append(matches, u)
		}
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 units sharing the p location, got %d: %+v", len(matches), matches)
	}
	if matches[0].Text != "Line one" || matches[1].Text != "Line two" {
		t.Errorf("texts = %q, %q; want %q, %q", matches[0].Text, matches[1].Text, "Line one", "Line two")
	}
}

// TestExtractPreservesWhitespaceVerbatimInsidePre confirms <pre>'s
// significant whitespace (indentation, line breaks) is preserved
// exactly, rather than being collapsed like ordinary flow text -- each
// literal line still becomes its own unit, the same convention used for
// a <br>.
func TestExtractPreservesWhitespaceVerbatimInsidePre(t *testing.T) {
	data := []byte("<pre>col1    col2\nrow2    val2</pre>")
	got := extractAll(t, data)

	var matches []string
	for _, unit := range got {
		if loc, ok := unit.Location.(htmlPathLocation); ok && loc.Path == "pre:nth-of-type(1)" {
			matches = append(matches, unit.Text)
		}
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 units (one per literal pre line), got %d: %+v", len(matches), matches)
	}
	// The literal internal run of 4 spaces must survive uncollapsed --
	// flush still trims each line's own leading/trailing whitespace (the
	// same as it does for every element), but that's independent of
	// verbatim vs. collapsed handling of *internal* runs.
	if matches[0] != "col1    col2" || matches[1] != "row2    val2" {
		t.Errorf("pre lines = %q, %q; want internal whitespace runs preserved verbatim (not collapsed to one space)", matches[0], matches[1])
	}
}

// --- Extract: implied end tags (fix #4) ---

// TestExtractImpliedCloseForUnclosedTableCells confirms
// "<tr><td>a<td>b<td>c" (a common minifier/shorthand pattern with no
// explicit closing tags) produces three SIBLING td's under one tr,
// rather than nesting td inside td inside td.
func TestExtractImpliedCloseForUnclosedTableCells(t *testing.T) {
	data := []byte(`<table><tr><td>a<td>b<td>c</table>`)
	got := extractAll(t, data)

	wantPaths := []string{
		"table:nth-of-type(1)>tr:nth-of-type(1)>td:nth-of-type(1)",
		"table:nth-of-type(1)>tr:nth-of-type(1)>td:nth-of-type(2)",
		"table:nth-of-type(1)>tr:nth-of-type(1)>td:nth-of-type(3)",
	}
	wantTexts := []string{"a", "b", "c"}
	for i, p := range wantPaths {
		u, ok := findByPath(got, p)
		if !ok {
			t.Fatalf("expected sibling unit at %q; units: %+v", p, got)
		}
		if u.Text != wantTexts[i] {
			t.Errorf("text at %q = %q, want %q", p, u.Text, wantTexts[i])
		}
	}
	for _, u := range got {
		loc := u.Location.(htmlPathLocation)
		if strings.Contains(loc.Path, "td:nth-of-type(1)>td") {
			t.Errorf("td's nested instead of siblings: path %q", loc.Path)
		}
	}
}

// TestExtractImpliedCloseForUnclosedListItems confirms "<li>a<li>b<li>c"
// produces sibling li's, not nested ones.
func TestExtractImpliedCloseForUnclosedListItems(t *testing.T) {
	data := []byte(`<ul><li>a<li>b<li>c</ul>`)
	got := extractAll(t, data)

	wantPaths := []string{
		"ul:nth-of-type(1)>li:nth-of-type(1)",
		"ul:nth-of-type(1)>li:nth-of-type(2)",
		"ul:nth-of-type(1)>li:nth-of-type(3)",
	}
	wantTexts := []string{"a", "b", "c"}
	for i, p := range wantPaths {
		u, ok := findByPath(got, p)
		if !ok {
			t.Fatalf("expected sibling unit at %q; units: %+v", p, got)
		}
		if u.Text != wantTexts[i] {
			t.Errorf("text at %q = %q, want %q", p, u.Text, wantTexts[i])
		}
	}
}

// TestExtractImpliedCloseKeepsPathDepthBounded confirms a large number
// of unclosed rows/cells (the shape that, without implied-end-tag
// handling, causes O(n^2) path-string growth) stays shallow: every
// emitted unit's path has at most 3 ">"-separated segments
// (table>tr>td), regardless of how many rows there are.
func TestExtractImpliedCloseKeepsPathDepthBounded(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<table>")
	const rows = 200
	for i := 0; i < rows; i++ {
		fmt.Fprintf(&sb, "<tr><td>a<td>b<td>c")
	}
	sb.WriteString("</table>")
	data := []byte(sb.String())

	got := extractAll(t, data)
	if len(got) == 0 {
		t.Fatal("expected units for a table with many unclosed rows")
	}
	for _, u := range got {
		loc := u.Location.(htmlPathLocation)
		depth := strings.Count(loc.Path, ">")
		if depth > 2 {
			t.Fatalf("path %q has depth %d, want <= 2 (table>tr>td)", loc.Path, depth)
		}
	}
}

// --- Extract: tokenizer usage / path building (fix #5) ---

// TestExtractPathBuildingByteIdenticalOutput is a broad regression that
// the switch away from z.Token()/fmt.Sprintf to z.TagName()/z.Text() and
// manual path-byte-building produces byte-identical text and paths to
// what the previous implementation produced, across a document
// exercising nesting, siblings, attributes (unused but present), and
// entities.
func TestExtractPathBuildingByteIdenticalOutput(t *testing.T) {
	data := []byte(`<html lang="en"><body class="x"><div id="a"><p title="t">Hello &amp; World</p><p>two</p></div></body></html>`)
	got := extractAll(t, data)

	u1, ok := findByPath(got, "html:nth-of-type(1)>body:nth-of-type(1)>div:nth-of-type(1)>p:nth-of-type(1)")
	if !ok {
		t.Fatalf("expected p[1] unit; units: %+v", got)
	}
	if u1.Text != "Hello & World" {
		t.Errorf("p[1] text = %q, want %q", u1.Text, "Hello & World")
	}
	u2, ok := findByPath(got, "html:nth-of-type(1)>body:nth-of-type(1)>div:nth-of-type(1)>p:nth-of-type(2)")
	if !ok {
		t.Fatalf("expected p[2] unit; units: %+v", got)
	}
	if u2.Text != "two" {
		t.Errorf("p[2] text = %q, want %q", u2.Text, "two")
	}
}

// --- Sniff: tightened heuristic (fix #6) ---

// TestSniffRejectsXSLTStylesheetWithHTMLOutputMarkup confirms an XSLT
// stylesheet whose *output* happens to contain <html> markup is not
// misclaimed by a non-.html/.htm/.xhtml-extensioned file: it starts
// with "<?xml", which is unambiguously rejected.
func TestSniffRejectsXSLTStylesheetWithHTMLOutputMarkup(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<xsl:stylesheet xmlns:xsl="http://www.w3.org/1999/XSL/Transform" version="1.0">
  <xsl:template match="/">
    <html><body>output</body></html>
  </xsl:template>
</xsl:stylesheet>`)
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("stylesheet.xsl", r, int64(len(data))) {
		t.Error("expected Sniff to reject an XSLT stylesheet merely outputting <html> markup")
	}
}

// TestSniffRejectsXMLCommentMentioningHTML confirms an XML comment that
// happens to mention <html> (but whose first real tag is not doctype or
// <html>) is not misclaimed.
func TestSniffRejectsXMLCommentMentioningHTML(t *testing.T) {
	data := []byte(`<!-- this document describes <html> output elsewhere -->
<data><item>x</item></data>`)
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("doc.xml", r, int64(len(data))) {
		t.Error("expected Sniff to reject content whose first real tag isn't doctype/<html>, despite an <html>-mentioning comment")
	}
}

// TestSniffRejectsHtmlBodyElementSubstringMatch confirms an XML element
// merely named with an "<html" prefix, like <htmlBody>, is not
// misclaimed via a raw substring match -- "<html" must be followed by a
// tag-terminating character.
func TestSniffRejectsHtmlBodyElementSubstringMatch(t *testing.T) {
	data := []byte(`<htmlBody><item>x</item></htmlBody>`)
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("doc.xml", r, int64(len(data))) {
		t.Error("expected Sniff to reject <htmlBody> as not a real <html> signal")
	}
}

// TestSniffStillAcceptsRealHTMLWithoutHTMLExtension confirms the
// tightened heuristic doesn't overcorrect: real HTML content, with no
// .html/.htm/.xhtml extension, preceded by a comment, is still
// accepted via the doctype signal being the first real tag.
func TestSniffStillAcceptsRealHTMLWithoutHTMLExtension(t *testing.T) {
	data := []byte(`<!-- generated -->
<!DOCTYPE html>
<html><body>content</body></html>`)
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("doc.txt", r, int64(len(data))) {
		t.Error("expected Sniff to accept real HTML content despite a non-html extension")
	}
}

// --- Extensions (fix #7) ---

func TestExtensionsIncludesXHTML(t *testing.T) {
	exts := (Extractor{}).Extensions()
	found := false
	for _, e := range exts {
		if e == ".xhtml" {
			found = true
		}
	}
	if !found {
		t.Errorf("Extensions() = %v, want it to include %q", exts, ".xhtml")
	}
}

// --- Context cancellation ---

func TestExtractRespectsContextCancellation(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<div>")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&sb, "<p>value %d</p>", i)
	}
	sb.WriteString("</div>")
	data := []byte(sb.String())

	ctx, cancel := context.WithCancel(context.Background())
	units, errc := (Extractor{}).Extract(ctx, bytes.NewReader(data), int64(len(data)))

	<-units // read exactly one unit
	cancel()

	for range units {
	}
	<-errc
}
