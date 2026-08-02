package xmldoc

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// --- Sniff: accept cases ---

func TestSniffAcceptsSimpleXML(t *testing.T) {
	data := []byte(`<root><item>hello</item></root>`)
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.xml", r, int64(len(data))) {
		t.Error("expected Sniff to accept a simple well-formed XML document")
	}
}

func TestSniffAcceptsXMLDeclaration(t *testing.T) {
	data := []byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<root><item>hello</item></root>")
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.xml", r, int64(len(data))) {
		t.Error("expected Sniff to accept an XML document with a leading declaration")
	}
}

// TestSniffAcceptsLeadingCommentOrDoctype is a regression test for a
// real bug found by review: well-formed XML starting with a comment or
// a DOCTYPE declaration before the root element was rejected by the
// cheap prefix check ('<' followed by '!' failed the name-start-char
// test), incorrectly falling back to the text extractor.
func TestSniffAcceptsLeadingCommentOrDoctype(t *testing.T) {
	cases := []string{
		"<!-- a comment -->\n<root><item>hello</item></root>",
		"<!DOCTYPE root>\n<root><item>hello</item></root>",
	}
	for _, data := range cases {
		r := bytes.NewReader([]byte(data))
		if !(Extractor{}).Sniff("file.xml", r, int64(len(data))) {
			t.Errorf("expected Sniff to accept %q", data)
		}
	}
}

// TestSniffAcceptsNonASCIIElementName is a regression test for a real
// bug found by review: a root element name starting with a non-ASCII
// character (legal per the XML NameStartChar production, e.g. CJK
// characters) was rejected by the cheap prefix check -- which only
// recognized ASCII letters/'_'/':' as valid first bytes -- before the
// real, authoritative dec.Token() check ever ran, incorrectly falling
// back to the text extractor for well-formed XML.
func TestSniffAcceptsNonASCIIElementName(t *testing.T) {
	data := []byte(`<中文>hello</中文>`)
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.xml", r, int64(len(data))) {
		t.Error("expected Sniff to accept XML with a non-ASCII root element name")
	}
}

func TestSniffAcceptsLeadingWhitespaceAndBOM(t *testing.T) {
	data := append([]byte{0xEF, 0xBB, 0xBF}, []byte("   \n\t<root><item>hello</item></root>")...)
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.xml", r, int64(len(data))) {
		t.Error("expected Sniff to accept XML preceded by a BOM and leading whitespace")
	}
}

// TestSniffAcceptsLargeReportedSize confirms there is no size gate:
// unlike the antchfx/xmlquery-based implementation this package used to
// have (which built a full in-memory DOM and so had to decline anything
// above a 64 MiB threshold), streaming via encoding/xml.Decoder never
// buffers more than one element's own direct text at a time, so Sniff
// has no reason to decline a file just because it's reportedly huge.
func TestSniffAcceptsLargeReportedSize(t *testing.T) {
	data := []byte(`<root><item>hello</item></root>`)
	r := bytes.NewReader(data)
	const largerThanTheOldSizeGate = 65 * 1024 * 1024
	if !(Extractor{}).Sniff("file.xml", r, largerThanTheOldSizeGate) {
		t.Error("expected Sniff to accept a well-formed file regardless of its reported size")
	}
}

// --- Sniff: reject cases ---

func TestSniffRejectsEmptyFile(t *testing.T) {
	data := []byte{}
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.xml", r, int64(len(data))) {
		t.Error("expected Sniff to reject an empty file")
	}
}

func TestSniffRejectsPlainText(t *testing.T) {
	// Analogous to the MS Office lock-file regression: plain text that
	// doesn't start with '<' must not be claimed as XML.
	data := []byte("Jane Doe")
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.xml", r, int64(len(data))) {
		t.Error("expected Sniff to reject plain text not starting with '<'")
	}
}

// TestSniffRejectsInputBrokenAtTheFirstToken covers the case Sniff can
// actually detect cheaply: input that fails to decode even its very
// first token (here, EOF while still inside the opening tag, before the
// closing '>'). See TestSniffAcceptsButExtractErrorsOnXMLBrokenLater for
// the deliberate tradeoff on malformed XML deeper in the document.
func TestSniffRejectsInputBrokenAtTheFirstToken(t *testing.T) {
	data := []byte(`<root`)
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.xml", r, int64(len(data))) {
		t.Error("expected Sniff to reject input that fails to decode its first token")
	}
}

// TestSniffAcceptsButExtractErrorsOnXMLBrokenLater documents a
// deliberate tradeoff (mirroring jsondoc's identical one): Sniff only
// decodes the first token cheaply, rather than fully parsing the whole
// document just to decide dispatch -- doing a full parse in Sniff would
// cost as much as Extract itself on a large file, defeating the point of
// streaming. So XML whose problem is deeper than the first token (a
// well-formed opening tag, but a mismatched closing tag later on) is
// still claimed by Sniff, and the error surfaces from Extract instead of
// falling back to text. This is a dispatch-time tradeoff, not a bug.
func TestSniffAcceptsButExtractErrorsOnXMLBrokenLater(t *testing.T) {
	data := []byte(`<root><item>unclosed</root>`)
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.xml", r, int64(len(data))) {
		t.Fatal("expected Sniff to accept XML that starts well-formed (its problem is deeper in the document)")
	}

	units, errc := (Extractor{}).Extract(context.Background(), r, int64(len(data)))
	for range units {
	}
	if err := <-errc; err == nil {
		t.Error("expected Extract to report an error for the mismatched closing tag")
	}
}

func TestSniffRejectsInvalidNameStartChar(t *testing.T) {
	data := []byte(`<3 hours ago`)
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.xml", r, int64(len(data))) {
		t.Error("expected Sniff to reject input starting with '<' but not a valid XML name-start char")
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

func findByXPath(units []domain.TextUnit, xpath string) (domain.TextUnit, bool) {
	for _, u := range units {
		if loc, ok := u.Location.(xmlPathLocation); ok && loc.XPath == xpath {
			return u, true
		}
	}
	return domain.TextUnit{}, false
}

// --- Extract: XPath synthesis correctness ---

// TestExtractSimpleNestedElements also locks in the "always index, even
// a singleton" convention (see the package doc comment): item and name
// each get "[1]" despite having no siblings, since a single streaming
// pass can't know in advance that none will follow.
func TestExtractSimpleNestedElements(t *testing.T) {
	data := []byte(`<root><item><name>Ada</name></item></root>`)
	got := extractAll(t, data)

	u, ok := findByXPath(got, "/root/item[1]/name[1]")
	if !ok {
		t.Fatalf("no unit found at /root/item[1]/name[1]; units: %+v", got)
	}
	if u.Text != "Ada" {
		t.Errorf("text = %q, want %q", u.Text, "Ada")
	}
}

func TestExtractMultipleSameTagSiblingsGetIndices(t *testing.T) {
	data := []byte(`<root><item id="1"><name>Ada</name></item><item id="2"><name>Grace</name></item></root>`)
	got := extractAll(t, data)

	u1, ok := findByXPath(got, "/root/item[1]/name[1]")
	if !ok {
		t.Fatalf("no unit found at /root/item[1]/name[1]; units: %+v", got)
	}
	if u1.Text != "Ada" {
		t.Errorf("item[1]/name text = %q, want %q", u1.Text, "Ada")
	}

	u2, ok := findByXPath(got, "/root/item[2]/name[1]")
	if !ok {
		t.Fatalf("no unit found at /root/item[2]/name[1]; units: %+v", got)
	}
	if u2.Text != "Grace" {
		t.Errorf("item[2]/name text = %q, want %q", u2.Text, "Grace")
	}
}

func TestExtractMixedContentDoesNotCorruptSiblingIndices(t *testing.T) {
	// Interleaved text nodes between the two <item> elements must not be
	// counted as siblings, or the indices would shift/duplicate.
	data := []byte(`<root>some text<item>a</item>more text<item>b</item></root>`)
	got := extractAll(t, data)

	u1, ok := findByXPath(got, "/root/item[1]")
	if !ok {
		t.Fatalf("no unit found at /root/item[1]; units: %+v", got)
	}
	if u1.Text != "a" {
		t.Errorf("item[1] text = %q, want %q", u1.Text, "a")
	}

	u2, ok := findByXPath(got, "/root/item[2]")
	if !ok {
		t.Fatalf("no unit found at /root/item[2]; units: %+v", got)
	}
	if u2.Text != "b" {
		t.Errorf("item[2] text = %q, want %q", u2.Text, "b")
	}
}

func TestExtractAttribute(t *testing.T) {
	data := []byte(`<root><item id="42">text</item></root>`)
	got := extractAll(t, data)

	u, ok := findByXPath(got, "/root/item[1]/@id")
	if !ok {
		t.Fatalf("no unit found at /root/item[1]/@id; units: %+v", got)
	}
	if u.Text != "42" {
		t.Errorf("attribute text = %q, want %q", u.Text, "42")
	}
}

// TestExtractSkipsNamespaceDeclarations is a regression test for a real
// bug found by review: xmlns="..." and xmlns:prefix="..." are namespace
// declarations, not real attributes, but were being emitted as if they
// were -- and worse, a prefixed declaration like xmlns:a="urn:x" rendered
// as "/root/@a" (the "xmlns:" stripped away), which would collide with a
// genuine attribute actually named "a".
func TestExtractSkipsNamespaceDeclarations(t *testing.T) {
	data := []byte(`<root xmlns="urn:default" xmlns:a="urn:x" id="1"><item>x</item></root>`)
	got := extractAll(t, data)

	if _, ok := findByXPath(got, "/root/@xmlns"); ok {
		t.Error("expected no unit for the default xmlns declaration")
	}
	if _, ok := findByXPath(got, "/root/@a"); ok {
		t.Error("expected no unit for the xmlns:a declaration (must not collide with a real @a attribute)")
	}

	u, ok := findByXPath(got, "/root/@id")
	if !ok {
		t.Fatalf("expected the real id attribute to still be emitted; units: %+v", got)
	}
	if u.Text != "1" {
		t.Errorf("id attribute text = %q, want %q", u.Text, "1")
	}
}

// TestExtractDirectTextOnlyNotDescendant is the single most important
// regression test for this package: an ancestor element must never
// include its descendants' text in its own emitted unit. Streaming via
// encoding/xml.Decoder gets this right structurally, not by filtering: a
// child's CharData tokens only ever arrive while the child's own frame
// is on top of the stack, so they can never land in the parent's text
// builder in the first place (unlike the xmlquery.Node.InnerText() bug
// this package used to have to explicitly avoid).
func TestExtractDirectTextOnlyNotDescendant(t *testing.T) {
	data := []byte(`<a>outer<b>inner</b></a>`)
	got := extractAll(t, data)

	ub, ok := findByXPath(got, "/a/b[1]")
	if !ok {
		t.Fatalf("no unit found at /a/b[1]; units: %+v", got)
	}
	if ub.Text != "inner" {
		t.Errorf("/a/b[1] text = %q, want %q", ub.Text, "inner")
	}

	ua, ok := findByXPath(got, "/a")
	if !ok {
		t.Fatalf("no unit found at /a; units: %+v", got)
	}
	if ua.Text != "outer" {
		t.Errorf("/a text = %q, want %q (must not include descendant text)", ua.Text, "outer")
	}
	if strings.Contains(ua.Text, "inner") {
		t.Errorf("/a text = %q must not contain descendant text %q", ua.Text, "inner")
	}
}

func TestExtractWhitespaceOnlyDirectTextIsSkipped(t *testing.T) {
	data := []byte("<root>\n  <item>x</item>\n</root>")
	got := extractAll(t, data)

	if _, ok := findByXPath(got, "/root"); ok {
		t.Error("expected no unit at /root, since its only direct text is whitespace")
	}

	u, ok := findByXPath(got, "/root/item[1]")
	if !ok {
		t.Fatalf("no unit found at /root/item[1]; units: %+v", got)
	}
	if u.Text != "x" {
		t.Errorf("/root/item[1] text = %q, want %q", u.Text, "x")
	}
}

// TestExtractCDATA confirms CDATA sections are captured as direct text:
// encoding/xml.Decoder returns CDATA content as a CharData token like
// any other character data, so this works without any special-casing.
func TestExtractCDATA(t *testing.T) {
	data := []byte(`<root><item><![CDATA[raw <not-a-tag> text]]></item></root>`)
	got := extractAll(t, data)

	u, ok := findByXPath(got, "/root/item[1]")
	if !ok {
		t.Fatalf("no unit found at /root/item[1]; units: %+v", got)
	}
	if u.Text != "raw <not-a-tag> text" {
		t.Errorf("text = %q, want %q", u.Text, "raw <not-a-tag> text")
	}
}

// --- Extract: real line/column, the whole point of moving off xmlquery ---

// TestExtractDistinguishesColumnsOnASingleLine is the regression test
// that motivated dropping antchfx/xmlquery for encoding/xml: xmlquery
// only reports a line number, so every match in a single-line (e.g.
// minified) XML file reported the exact same location -- there was no
// way to tell two matches on line 1 apart. encoding/xml.Decoder.InputPos
// gives a real column, so they're now distinguishable.
func TestExtractDistinguishesColumnsOnASingleLine(t *testing.T) {
	data := []byte(`<root><item id="1">A</item><item id="2">B</item></root>`)
	got := extractAll(t, data)

	a, ok := findByXPath(got, "/root/item[1]")
	if !ok {
		t.Fatalf("no unit found at /root/item[1]; units: %+v", got)
	}
	b, ok := findByXPath(got, "/root/item[2]")
	if !ok {
		t.Fatalf("no unit found at /root/item[2]; units: %+v", got)
	}
	locA, locB := a.Location.(xmlPathLocation), b.Location.(xmlPathLocation)

	if locA.Line != 1 || locB.Line != 1 {
		t.Fatalf("expected both matches on line 1 (single-line document), got lines %d and %d", locA.Line, locB.Line)
	}
	if locA.Column == locB.Column {
		t.Fatalf("expected distinct columns for two matches sharing a line, both got column %d", locA.Column)
	}
	// Exact values verified against a real run, not hand-computed, to
	// avoid an off-by-one baked into the test itself.
	if locA.Column != 21 {
		t.Errorf("item[1] column = %d, want 21", locA.Column)
	}
	if locB.Column != 42 {
		t.Errorf("item[2] column = %d, want 42", locB.Column)
	}
}

// TestExtractPositionSkipsLeadingWhitespaceRun is a regression test for
// a real bug found by review: the position anchor used to latch onto an
// element's *first* CharData token even when that token was a
// whitespace-only run preceding a child element (common in pretty-
// printed XML), reporting a position near that leading whitespace rather
// than anywhere near the element's actual (later) text.
func TestExtractPositionSkipsLeadingWhitespaceRun(t *testing.T) {
	data := []byte(`<root>   <item>a</item>real text</root>`)
	got := extractAll(t, data)

	u, ok := findByXPath(got, "/root")
	if !ok {
		t.Fatalf("no unit found at /root; units: %+v", got)
	}
	if u.Text != "real text" {
		t.Fatalf("/root text = %q, want %q", u.Text, "real text")
	}
	loc := u.Location.(xmlPathLocation)
	// Column 33 is the position right after "real text" ends; column 10
	// (right after the leading "   ") was the bug.
	if loc.Column != 33 {
		t.Errorf("Column = %d, want 33 (end of the actual text, not the leading whitespace run)", loc.Column)
	}
}

func TestExtractPopulatesLineAndColumn(t *testing.T) {
	data := []byte("<root>\n  <item>x</item>\n</root>")
	got := extractAll(t, data)

	u, ok := findByXPath(got, "/root/item[1]")
	if !ok {
		t.Fatalf("no unit found at /root/item[1]; units: %+v", got)
	}
	loc, ok := u.Location.(xmlPathLocation)
	if !ok {
		t.Fatalf("location type = %T, want xmlPathLocation", u.Location)
	}
	if loc.Line != 2 {
		t.Errorf("Line = %d, want 2", loc.Line)
	}
	if loc.Column != 10 {
		t.Errorf("Column = %d, want 10", loc.Column)
	}
}

// --- Location ---

func TestXMLPathLocationHuman(t *testing.T) {
	loc := xmlPathLocation{XPath: "/root/item[3]", Line: 5, Column: 9}
	if got, want := loc.Human(), "/root/item[3] (line 5:9)"; got != want {
		t.Errorf("Human() = %q, want %q", got, want)
	}
}

func TestXMLPathLocationHumanNoLine(t *testing.T) {
	loc := xmlPathLocation{XPath: "/root/item[3]", Line: 0}
	if got, want := loc.Human(), "/root/item[3]"; got != want {
		t.Errorf("Human() = %q, want %q", got, want)
	}
}

func TestXMLPathLocationFields(t *testing.T) {
	loc := xmlPathLocation{XPath: "/root/item[3]/@id", Line: 7, Column: 15}
	fields := loc.Fields(nil)

	xpath, ok := fields["xpath"].(string)
	if !ok || xpath != "/root/item[3]/@id" {
		t.Errorf(`Fields()["xpath"] = %v (%T), want string %q`, fields["xpath"], fields["xpath"], "/root/item[3]/@id")
	}
	line, ok := fields["line"].(int)
	if !ok || line != 7 {
		t.Errorf(`Fields()["line"] = %v (%T), want int 7`, fields["line"], fields["line"])
	}
	col, ok := fields["col"].(int)
	if !ok || col != 15 {
		t.Errorf(`Fields()["col"] = %v (%T), want int 15`, fields["col"], fields["col"])
	}
}

// TestXMLPathLocationHyperlinkURIWithLine confirms a known position links
// straight to it -- editors that understand ":line:col" URIs land right
// on the match, not just somewhere in the file.
func TestXMLPathLocationHyperlinkURIWithLine(t *testing.T) {
	loc := xmlPathLocation{XPath: "/root/item[3]", Line: 5, Column: 9}
	got := loc.HyperlinkURI("/path/file.xml", nil)
	want := domain.FileURI("/path/file.xml", "") + ":5:9"
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

// TestXMLPathLocationHyperlinkURIWithoutLine covers the defensive
// fallback when no position is known (shouldn't happen from real
// extraction, since encoding/xml.Decoder.InputPos is always available,
// but HyperlinkURI must not fabricate a ":0:0" suffix if it did).
func TestXMLPathLocationHyperlinkURIWithoutLine(t *testing.T) {
	loc := xmlPathLocation{XPath: "/root/item[3]", Line: 0}
	got := loc.HyperlinkURI("/path/file.xml", nil)
	want := domain.FileURI("/path/file.xml", "")
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
	if strings.Contains(got, "#") {
		t.Errorf("HyperlinkURI() = %q, want no fragment", got)
	}
}

// --- Context cancellation ---

func TestExtractRespectsContextCancellation(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<root>")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&sb, "<item id=\"%d\"><name>value %d</name></item>", i, i)
	}
	sb.WriteString("</root>")
	data := []byte(sb.String())

	r := bytes.NewReader(data)
	ctx, cancel := context.WithCancel(context.Background())
	units, errc := (Extractor{}).Extract(ctx, r, int64(len(data)))

	<-units // read exactly one unit
	cancel()

	// Draining should terminate promptly once cancelled, and the
	// channels must be closed (not leaked).
	for range units {
	}
	<-errc
}

// --- Malformed XML mid-parse (Extract, independent of Sniff) ---

func TestExtractReturnsErrorForMalformedXML(t *testing.T) {
	data := []byte(`<root><item>unclosed</root>`)
	r := bytes.NewReader(data)
	units, errc := (Extractor{}).Extract(context.Background(), r, int64(len(data)))

	for range units {
	}
	if err := <-errc; err == nil {
		t.Error("expected an error for malformed XML, got nil")
	}
}

// --- Namespaced XML ---

func TestExtractNamespacedXMLUsesLocalNameOnly(t *testing.T) {
	data := []byte(`<root xmlns:a="urn:x"><a:item>x</a:item></root>`)
	got := extractAll(t, data)

	// Documented v1 scope decision: namespace prefixes are not rendered
	// in the synthesized XPath (xml.Name.Local is the local name only).
	if _, ok := findByXPath(got, "/root/a:item[1]"); ok {
		t.Error("expected synthesized XPath to omit the namespace prefix, but found /root/a:item[1]")
	}
	u, ok := findByXPath(got, "/root/item[1]")
	if !ok {
		t.Fatalf("no unit found at /root/item[1] (local name only); units: %+v", got)
	}
	if u.Text != "x" {
		t.Errorf("/root/item[1] text = %q, want %q", u.Text, "x")
	}
}
