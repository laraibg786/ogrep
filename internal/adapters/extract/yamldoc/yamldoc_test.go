package yamldoc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// --- Sniff: accept cases ---

func TestSniffAcceptsMapping(t *testing.T) {
	data := []byte("name: Ada\n")
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.yaml", r, int64(len(data))) {
		t.Error("expected Sniff to accept a genuine mapping")
	}
}

func TestSniffAcceptsSequence(t *testing.T) {
	data := []byte("- a\n- b\n")
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.yaml", r, int64(len(data))) {
		t.Error("expected Sniff to accept a genuine sequence")
	}
}

func TestSniffAcceptsNestedStructure(t *testing.T) {
	data := []byte("a:\n  b:\n    - 1\n    - 2\n")
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.yaml", r, int64(len(data))) {
		t.Error("expected Sniff to accept nested structure")
	}
}

// --- Sniff: reject cases ---

// TestSniffRejectsPlainTextLockFile is the regression test for the bug
// where goccy/go-yaml happily parses ANY plain text -- even binary
// garbage -- as a single bare-scalar YAML document with no error. A
// naive "did it parse without error" Sniff would misclaim MS Office's
// transient "~$name.ext" lock files (whose content is a short
// plain-text string with no YAML syntax at all). The fix requires
// hasStructure/nodeHasStructure to find an actual non-empty mapping or
// sequence, not just a successful parse. This test would fail against
// the pre-fix "parsed without error" heuristic.
func TestSniffRejectsPlainTextLockFile(t *testing.T) {
	cases := []string{
		"Jane Doe",
		"~$lockfile owner data",
	}
	for _, data := range cases {
		r := bytes.NewReader([]byte(data))
		if (Extractor{}).Sniff("~$doc.docx", r, int64(len(data))) {
			t.Errorf("expected Sniff to reject plain text with no YAML structure: %q", data)
		}
	}
}

func TestSniffRejectsEmptyFile(t *testing.T) {
	r := bytes.NewReader(nil)
	if (Extractor{}).Sniff("empty.yaml", r, 0) {
		t.Error("expected Sniff to reject an empty file")
	}
}

func TestSniffRejectsWhitespaceOnly(t *testing.T) {
	data := []byte("   \n\t\n  \n")
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("ws.yaml", r, int64(len(data))) {
		t.Error("expected Sniff to reject a whitespace-only file")
	}
}

func TestSniffRejectsMalformedYAML(t *testing.T) {
	data := []byte("a:\n  b:\nc: - d\n  bad indent: [\n")
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("bad.yaml", r, int64(len(data))) {
		t.Error("expected Sniff to reject malformed YAML that fails to parse")
	}
}

// fakeReaderAt lies about the size of the data it holds, letting the
// oversized-input test exercise Sniff's size gate without allocating a
// real 64MiB buffer.
type fakeReaderAt struct {
	data []byte
}

func (f fakeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestSniffRejectsOversizedInput(t *testing.T) {
	ra := fakeReaderAt{data: []byte("name: Ada\n")}
	// size claims to exceed maxSniffSize; Sniff must decline based on
	// size alone, without attempting to read/parse.
	if (Extractor{}).Sniff("huge.yaml", ra, maxSniffSize+1) {
		t.Error("expected Sniff to reject input whose declared size exceeds maxSniffSize")
	}
}

// --- Extract: path/value correctness ---

func TestExtractNestedMappingAndSequence(t *testing.T) {
	data := []byte("a:\n  b:\n    - 1\n    - 2\n")
	got := extractTexts(t, data)
	want := []string{".a.b[0] = 1", ".a.b[1] = 2"}
	assertTexts(t, got, want)
}

func TestExtractNonIdentifierKeysEscape(t *testing.T) {
	data := []byte("foo-bar: 1\n\"has space\": 2\n")
	got := extractTexts(t, data)
	want := []string{
		`.["foo-bar"] = 1`,
		`.["has space"] = 2`,
	}
	assertTexts(t, got, want)
}

func TestExtractNonStringMapKeys(t *testing.T) {
	// YAML permits non-string mapping keys. "123" is not a bare jq
	// identifier (leading digit) so it must be bracketed/quoted; "true"
	// happens to consist only of letters, so it satisfies the same
	// bare-identifier rule as any other alphabetic key and renders
	// unbracketed, exactly like the "true"-as-a-word case for jsondoc's
	// identical helper.
	data := []byte("123: x\ntrue: y\n")
	got := extractTexts(t, data)
	want := []string{
		`.["123"] = x`,
		`.true = y`,
	}
	assertTexts(t, got, want)
}

// TestExtractRootPathDoubling is the regression test for the bug where a
// top-level (root) non-identifier key rendered as "..[\"foo-bar\"]"
// instead of ".[\"foo-bar\"]": the root path "." and jqSegment's own
// leading "." both got concatenated. The fix is the appendKey helper,
// which special-cases path == ".". This test would fail if that fix
// regressed.
func TestExtractRootPathDoubling(t *testing.T) {
	data := []byte("foo-bar: 1\n")
	got := extractTexts(t, data)
	want := []string{`.["foo-bar"] = 1`}
	assertTexts(t, got, want)
	for _, line := range got {
		if bytes.Contains([]byte(line), []byte("..")) {
			t.Errorf("path doubling regression: %q contains a double dot", line)
		}
	}
}

func TestExtractMultiDocumentGetsDocumentPrefix(t *testing.T) {
	data := []byte("name: first\n---\nname: second\n")
	got := extractTexts(t, data)
	want := []string{
		".document[0].name = first",
		".document[1].name = second",
	}
	assertTexts(t, got, want)
}

func TestExtractSingleDocumentHasNoDocumentPrefix(t *testing.T) {
	data := []byte("name: solo\n")
	got := extractTexts(t, data)
	want := []string{".name = solo"}
	assertTexts(t, got, want)
	for _, line := range got {
		if bytes.Contains([]byte(line), []byte("document[")) {
			t.Errorf("single-document file got a document[N] prefix: %q", line)
		}
	}
}

func TestExtractEmptyContainers(t *testing.T) {
	data := []byte("m: {}\ns: []\n")
	got := extractTexts(t, data)
	want := []string{
		".m = {}",
		".s = []",
	}
	assertTexts(t, got, want)
}

// TestExtractAliasesNotExpanded asserts the exact behavior the package
// doc comment documents as deliberate: anchors are transparent (the
// value they annotate is walked as if the anchor weren't there), while
// aliases are NOT expanded to their anchor's value -- an alias node is
// walked as a leaf whose value text is its literal reference form
// (e.g. "*greeting").
func TestExtractAliasesNotExpanded(t *testing.T) {
	data := []byte("anchored: &greeting hello\naliased: *greeting\n")
	got := extractTexts(t, data)
	want := []string{
		".anchored = hello",
		".aliased = *greeting",
	}
	assertTexts(t, got, want)
}

// TestExtractValueLiteralFidelity asserts that the emitted value text
// uses the node's original source representation (via Node.String())
// rather than a re-serialization, so a quoted string in the source
// stays quoted in the output.
func TestExtractValueLiteralFidelity(t *testing.T) {
	data := []byte(`greeting: "hello world"` + "\n")
	got := extractTexts(t, data)
	want := []string{`.greeting = "hello world"`}
	assertTexts(t, got, want)
}

// --- Location ---

func TestYamlPathLocationHuman(t *testing.T) {
	loc := yamlPathLocation{Path: ".a.b", Line: 3, Column: 6}
	if got, want := loc.Human(), ".a.b (line 3:6)"; got != want {
		t.Errorf("Human() = %q, want %q", got, want)
	}
}

func TestYamlPathLocationFields(t *testing.T) {
	loc := yamlPathLocation{Path: ".a.b", Line: 3, Column: 6}
	fields := loc.Fields(nil)
	path, ok := fields["yamlpath"].(string)
	if !ok || path != ".a.b" {
		t.Errorf(`Fields()["yamlpath"] = %v (%T), want string ".a.b"`, fields["yamlpath"], fields["yamlpath"])
	}
	line, ok := fields["line"].(int)
	if !ok || line != 3 {
		t.Errorf(`Fields()["line"] = %v (%T), want int 3`, fields["line"], fields["line"])
	}
	col, ok := fields["col"].(int)
	if !ok || col != 6 {
		t.Errorf(`Fields()["col"] = %v (%T), want int 6`, fields["col"], fields["col"])
	}
}

// TestYamlPathLocationHyperlinkURI confirms the hyperlink links straight
// to the value's real source position (goccy gives us both line and
// column directly from the AST), not just the bare file -- unlike a
// Span, which is an offset into the synthesized "<path> = <value>"
// display text and doesn't correspond to a real file position, Line and
// Column here always do.
func TestYamlPathLocationHyperlinkURI(t *testing.T) {
	loc := yamlPathLocation{Path: ".a.b", Line: 3, Column: 6}
	got := loc.HyperlinkURI("/path/file.yaml", []domain.Span{{Start: 0, End: 4}})
	want := domain.FileURI("/path/file.yaml", "") + ":3:6"
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

func TestYamlPathLocationLineNumbers(t *testing.T) {
	// "c" is on line 3; verify Extract reports that exact line, not 0
	// or 1, confirming line numbers track real source position.
	data := []byte("a: 1\nb: 2\nc: 3\n")
	units, errc := (Extractor{}).Extract(context.Background(), bytes.NewReader(data), int64(len(data)))

	var cLine int
	found := false
	for u := range units {
		if u.Text == ".c = 3" {
			loc, ok := u.Location.(yamlPathLocation)
			if !ok {
				t.Fatalf("location type = %T, want yamlPathLocation", u.Location)
			}
			cLine = loc.Line
			found = true
		}
	}
	if err := <-errc; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("did not find unit for .c")
	}
	if cLine != 3 {
		t.Errorf("line for .c = %d, want 3", cLine)
	}
}

// --- Context cancellation ---

func TestExtractRespectsContextCancellation(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("items:\n")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&buf, "  - item%d\n", i)
	}
	data := buf.Bytes()

	ctx, cancel := context.WithCancel(context.Background())
	units, errc := (Extractor{}).Extract(ctx, bytes.NewReader(data), int64(len(data)))

	<-units // read exactly one unit
	cancel()

	// Draining should terminate promptly once cancelled, and the
	// channels must be closed (not leaked).
	for range units {
	}
	<-errc
}

// --- Malformed YAML mid-parse ---

func TestExtractMalformedYAMLReturnsError(t *testing.T) {
	data := []byte("a:\n  b:\nc: - d\n  bad indent: [\n")
	units, errc := (Extractor{}).Extract(context.Background(), bytes.NewReader(data), int64(len(data)))

	for range units {
		// drain; malformed input should yield no units.
	}
	if err := <-errc; err == nil {
		t.Error("expected an error for malformed YAML, got nil")
	}
}

// --- test helpers ---

func extractTexts(t *testing.T, data []byte) []string {
	t.Helper()
	units, errc := (Extractor{}).Extract(context.Background(), bytes.NewReader(data), int64(len(data)))
	var got []string
	for u := range units {
		got = append(got, u.Text)
	}
	if err := <-errc; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return got
}

func assertTexts(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d units %q, want %d units %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("unit %d = %q, want %q", i, got[i], want[i])
		}
	}
}
