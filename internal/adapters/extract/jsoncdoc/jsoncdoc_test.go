package jsoncdoc

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

func sniff(data []byte) bool {
	r := bytes.NewReader(data)
	return (Extractor{}).Sniff("file.jsonc", r, int64(len(data)))
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
		t.Fatalf("unexpected error: %v", err)
	}
	return got
}

func findComment(units []domain.TextUnit, text string) (commentLocation, bool) {
	for _, u := range units {
		if loc, ok := u.Location.(commentLocation); ok && u.Text == text {
			return loc, true
		}
	}
	return commentLocation{}, false
}

// --- Sniff accept cases ---

func TestSniffAcceptsPlainJSON(t *testing.T) {
	if !sniff([]byte(`{"a":1}`)) {
		t.Error("expected Sniff to accept plain JSON (a subset of JWCC)")
	}
}

func TestSniffAcceptsCommentsAndTrailingCommas(t *testing.T) {
	data := []byte("// leading\n{\n  \"a\": 1, // trailing\n  \"b\": 2,\n}\n")
	if !sniff(data) {
		t.Error("expected Sniff to accept JSONC with comments and a trailing comma")
	}
}

// --- Sniff reject cases ---

func TestSniffRejectsEmptyFile(t *testing.T) {
	if sniff([]byte("")) {
		t.Error("expected Sniff to reject an empty file")
	}
}

func TestSniffRejectsPlainText(t *testing.T) {
	// Analogous to the MS Office lock-file regression: plain prose is not
	// valid JWCC (unlike YAML's permissive plain-scalar production, JSON's
	// grammar has no bare-word literal), so no separate "hasStructure"
	// guard is needed the way yamldoc's Sniff requires one.
	if sniff([]byte("Jane Doe")) {
		t.Error("expected Sniff to reject plain text")
	}
}

func TestSniffRejectsMalformedJSONC(t *testing.T) {
	if sniff([]byte(`{"a": `)) {
		t.Error("expected Sniff to reject truncated JSONC")
	}
}

func TestSniffRejectsOversizedInput(t *testing.T) {
	data := []byte(`{"a":1}`)
	r := bytes.NewReader(data)
	const largerThanMaxSniffSize = maxSniffSize + 1
	if (Extractor{}).Sniff("file.jsonc", r, largerThanMaxSniffSize) {
		t.Error("expected Sniff to reject input whose reported size exceeds maxSniffSize")
	}
}

// --- Extract: comments are greppable, with real positions ---

// TestExtractLeadingAndTrailingLineComments covers the two ends of the
// document a comment can attach to without any sibling node to claim it
// as BeforeExtra: a comment before the very first token (caught via the
// root node's own BeforeExtra) and one after the very last token (caught
// via the root node's own AfterExtra -- see collectComments's doc
// comment for why both need special-casing).
func TestExtractLeadingAndTrailingLineComments(t *testing.T) {
	data := []byte("// top comment\n{\n  \"a\": 1\n}\n// end comment\n")
	got := extractAll(t, data)

	loc, ok := findComment(got, "// top comment")
	if !ok {
		t.Fatalf("expected a comment unit for %q; units: %+v", "// top comment", got)
	}
	if loc.Line != 1 || loc.Column != 1 {
		t.Errorf("top comment position = %d:%d, want 1:1", loc.Line, loc.Column)
	}

	loc, ok = findComment(got, "// end comment")
	if !ok {
		t.Fatalf("expected a comment unit for %q; units: %+v", "// end comment", got)
	}
	if loc.Line != 5 {
		t.Errorf("end comment line = %d, want 5", loc.Line)
	}
}

// TestExtractTrailingLineCommentAfterValue and block comments interleaved
// with real values, verified against a real run (not hand-computed) to
// avoid an off-by-one baked into the test itself.
func TestExtractCommentsAmongValues(t *testing.T) {
	data := []byte("// top comment\n{\n  \"a\": 1, // trailing\n  \"b\": {},\n}\n")
	got := extractAll(t, data)

	top, ok := findComment(got, "// top comment")
	if !ok || top.Line != 1 || top.Column != 1 {
		t.Errorf("top comment = %+v, ok=%v, want line 1 col 1", top, ok)
	}
	trailing, ok := findComment(got, "// trailing")
	if !ok || trailing.Line != 3 || trailing.Column != 11 {
		t.Errorf("trailing comment = %+v, ok=%v, want line 3 col 11", trailing, ok)
	}

	// The value side must still work: "b": {} is jsondoc's own
	// empty-object leaf, forwarded unchanged.
	if !containsValueText(got, ".b = {}") {
		t.Errorf("expected %q among value units; units: %+v", ".b = {}", got)
	}
	if !containsValueText(got, ".a = 1") {
		t.Errorf("expected %q among value units; units: %+v", ".a = 1", got)
	}
}

// TestExtractCommentInEmptyContainer and TestExtractTrailingCommentBeforeClose
// cover the two cases that live in Object/Array's own AfterExtra field
// rather than any Value's Before/AfterExtra -- verified during design to
// be the one place hujson doesn't attach these to a traversable Value at
// all.
func TestExtractCommentInEmptyContainer(t *testing.T) {
	data := []byte(`{"empty": { /* nothing here */ }}`)
	got := extractAll(t, data)
	if _, ok := findComment(got, "/* nothing here */"); !ok {
		t.Fatalf("expected a comment unit for the empty-object comment; units: %+v", got)
	}
}

func TestExtractTrailingCommentBeforeClose(t *testing.T) {
	data := []byte("{\n  \"list\": [1, 2 /* mid */],\n}\n// end comment\n")
	got := extractAll(t, data)

	mid, ok := findComment(got, "/* mid */")
	if !ok || mid.Line != 2 || mid.Column != 17 {
		t.Errorf("mid comment = %+v, ok=%v, want line 2 col 17", mid, ok)
	}
	end, ok := findComment(got, "// end comment")
	if !ok || end.Line != 4 {
		t.Errorf("end comment = %+v, ok=%v, want line 4", end, ok)
	}
	if !containsValueText(got, ".list[0] = 1") || !containsValueText(got, ".list[1] = 2") {
		t.Errorf("expected both array-element values; units: %+v", got)
	}
}

// TestExtractSlashInStringIsNotMisreadAsComment is the single most
// important correctness test for this package: "//" or "/*" appearing
// inside a JSON string value must never be treated as a comment start.
// This works by construction, not by filtering -- hujson's own parser
// resolves the string-vs-comment ambiguity before ever populating an
// Extra field, so an Extra blob is guaranteed to never contain string
// content in the first place (see collectComments's doc comment).
func TestExtractSlashInStringIsNotMisreadAsComment(t *testing.T) {
	data := []byte(`{"url": "http://example.com/foo"}`)
	got := extractAll(t, data)

	for _, u := range got {
		if _, ok := u.Location.(commentLocation); ok {
			t.Errorf("expected no comment units at all, got one: %+v", u)
		}
	}
	if !containsValueText(got, `.url = "http://example.com/foo"`) {
		t.Errorf("expected the URL value intact; units: %+v", got)
	}
}

// --- Extract: value side reuses jsondoc unchanged ---

func TestExtractValuesMatchJSONDocOutputShape(t *testing.T) {
	data := []byte(`{"limits": {"foo-bar": 12}}`)
	got := extractAll(t, data)
	if !containsValueText(got, `.limits.["foo-bar"] = 12`) {
		t.Errorf(`expected jsondoc-style escaped path in output; units: %+v`, got)
	}
}

func TestExtractTrailingCommaTolerated(t *testing.T) {
	data := []byte("{\n  \"a\": 1,\n  \"b\": 2,\n}\n")
	got := extractAll(t, data)
	if !containsValueText(got, ".a = 1") || !containsValueText(got, ".b = 2") {
		t.Errorf("expected both values despite the trailing comma; units: %+v", got)
	}
}

func containsValueText(units []domain.TextUnit, text string) bool {
	for _, u := range units {
		if u.Text == text {
			return true
		}
	}
	return false
}

// --- Context cancellation ---

func TestExtractRespectsContextCancellation(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("{\n")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&sb, "  \"k%d\": %d, // comment %d\n", i, i, i)
	}
	sb.WriteString("  \"last\": true\n}\n")
	data := []byte(sb.String())

	r := bytes.NewReader(data)
	ctx, cancel := context.WithCancel(context.Background())
	units, errc := (Extractor{}).Extract(ctx, r, int64(len(data)))

	<-units // read exactly one unit
	cancel()

	for range units {
	}
	<-errc
}

// --- Malformed content (Extract, independent of Sniff) ---

func TestExtractReturnsErrorForMalformedJSONC(t *testing.T) {
	data := []byte(`{"a": `)
	r := bytes.NewReader(data)
	units, errc := (Extractor{}).Extract(context.Background(), r, int64(len(data)))
	for range units {
	}
	if err := <-errc; err == nil {
		t.Error("expected an error for malformed JSONC, got nil")
	}
}

// --- Location ---

func TestCommentLocationHuman(t *testing.T) {
	loc := commentLocation{Line: 4, Column: 3}
	if got, want := loc.Human(), "line 4:3 (comment)"; got != want {
		t.Errorf("Human() = %q, want %q", got, want)
	}
}

func TestCommentLocationFields(t *testing.T) {
	loc := commentLocation{Line: 4, Column: 3}
	fields := loc.Fields(nil)
	if got, want := fields["line"], 4; got != want {
		t.Errorf(`Fields()["line"] = %v, want %v`, got, want)
	}
	if got, want := fields["col"], 3; got != want {
		t.Errorf(`Fields()["col"] = %v, want %v`, got, want)
	}
	if got, want := fields["comment"], true; got != want {
		t.Errorf(`Fields()["comment"] = %v, want %v`, got, want)
	}
}

func TestCommentLocationHyperlinkURI(t *testing.T) {
	loc := commentLocation{Line: 4, Column: 3}
	got := loc.HyperlinkURI("/path/file.jsonc", nil)
	want := domain.FileURI("/path/file.jsonc", "") + ":4:3"
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}
