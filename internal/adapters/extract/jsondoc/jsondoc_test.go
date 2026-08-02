package jsondoc

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// sniff is a small helper that wraps data in a bytes.Reader (which
// implements io.ReaderAt) and calls Sniff, matching the construction
// style used by text_test.go.
func sniff(data []byte) bool {
	r := bytes.NewReader(data)
	return (Extractor{}).Sniff("file.json", r, int64(len(data)))
}

// extract runs Extract to completion and returns the collected units and
// the final error (nil if none).
func extract(t *testing.T, data []byte) ([]domain.TextUnit, error) {
	t.Helper()
	r := bytes.NewReader(data)
	units, errc := (Extractor{}).Extract(context.Background(), r, int64(len(data)))

	var got []domain.TextUnit
	for u := range units {
		got = append(got, u)
	}
	err := <-errc
	return got, err
}

func texts(units []domain.TextUnit) []string {
	out := make([]string, len(units))
	for i, u := range units {
		out[i] = u.Text
	}
	return out
}

func containsText(units []domain.TextUnit, want string) bool {
	for _, u := range units {
		if u.Text == want {
			return true
		}
	}
	return false
}

// --- Sniff accept cases ---

func TestSniffAcceptsObject(t *testing.T) {
	if !sniff([]byte(`{"a":1}`)) {
		t.Error("expected Sniff to accept a JSON object")
	}
}

func TestSniffAcceptsArray(t *testing.T) {
	if !sniff([]byte(`[1,2,3]`)) {
		t.Error("expected Sniff to accept a JSON array")
	}
}

func TestSniffAcceptsTopLevelString(t *testing.T) {
	if !sniff([]byte(`"hello"`)) {
		t.Error("expected Sniff to accept a top-level JSON string")
	}
}

func TestSniffAcceptsTopLevelNumber(t *testing.T) {
	if !sniff([]byte(`42`)) {
		t.Error("expected Sniff to accept a top-level JSON number")
	}
	if !sniff([]byte(`-3.14`)) {
		t.Error("expected Sniff to accept a negative top-level JSON number")
	}
}

func TestSniffAcceptsTopLevelBool(t *testing.T) {
	if !sniff([]byte(`true`)) {
		t.Error("expected Sniff to accept top-level 'true'")
	}
	if !sniff([]byte(`false`)) {
		t.Error("expected Sniff to accept top-level 'false'")
	}
}

func TestSniffAcceptsTopLevelNull(t *testing.T) {
	if !sniff([]byte(`null`)) {
		t.Error("expected Sniff to accept top-level 'null'")
	}
}

func TestSniffAcceptsLeadingWhitespace(t *testing.T) {
	if !sniff([]byte("   \n\t {\"a\":1}")) {
		t.Error("expected Sniff to accept JSON preceded by whitespace")
	}
}

// TestSniffAcceptsUTF8BOM is a regression test for a real bug found by
// review: a leading UTF-8 byte order mark (common from Windows editors)
// made Sniff decline an otherwise perfectly valid JSON file, falling
// back to a plain-text grep of what is actually structured JSON.
func TestSniffAcceptsUTF8BOM(t *testing.T) {
	data := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"a":1}`)...)
	if !sniff(data) {
		t.Error("expected Sniff to accept BOM-prefixed JSON")
	}
}

// TestExtractSkipsUTF8BOM confirms Extract, not just Sniff, handles the
// BOM: before the fix, encoding/json would fail outright on the BOM
// bytes ("invalid character 'ï' looking for beginning of value") even
// though Sniff had already accepted the file, since that's a distinct
// code path with its own io.Reader construction. Also checks the
// reported position isn't thrown off by the 3 skipped bytes -- the
// value should still be "column 1 of line 1" territory, not offset by
// the BOM.
func TestExtractSkipsUTF8BOM(t *testing.T) {
	data := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"a":1}`)...)
	got, err := extract(t, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsText(got, ".a = 1") {
		t.Fatalf("expected %q among %v", ".a = 1", texts(got))
	}
	for _, u := range got {
		loc := u.Location.(jsonPathLocation)
		if loc.Line != 1 {
			t.Errorf("Line = %d, want 1 (BOM must not be counted as a newline)", loc.Line)
		}
	}
}

// --- Sniff reject cases ---

func TestSniffRejectsEmptyFile(t *testing.T) {
	if sniff([]byte("")) {
		t.Error("expected Sniff to reject an empty file")
	}
}

func TestSniffRejectsWhitespaceOnly(t *testing.T) {
	if sniff([]byte("   \n\t\r\n  ")) {
		t.Error("expected Sniff to reject a whitespace-only file")
	}
}

func TestSniffRejectsMalformedJSON(t *testing.T) {
	// Sniff only trial-decodes the *first* token (deliberately -- see
	// its doc comment: decoding further from a bounded sniffWindow
	// prefix would falsely reject large, valid documents that happen to
	// be truncated at the window boundary). So malformed content is
	// only caught here when the malformation is detectable within that
	// very first token, e.g. a truncated top-level scalar/literal. An
	// incomplete object like `{"a":` is NOT caught by Sniff (its first
	// token is just the opening "{", which is valid on its own); that
	// case is instead caught later by Extract's decoder error (see
	// TestExtractPropagatesDecoderErrorOnInvalidToken).
	if sniff([]byte(`"unterminated`)) {
		t.Error("expected Sniff to reject an unterminated top-level string")
	}
	if sniff([]byte(`tru`)) {
		t.Error("expected Sniff to reject a truncated top-level literal")
	}
}

func TestSniffRejectsPlainEnglishText(t *testing.T) {
	// Regression case: this exact input ("Jane Doe") is an MS Office
	// lock-file style plain-text fixture that must not be mistaken for
	// JSON, since it starts with a letter that is none of {[\"0-9-tfn.
	if sniff([]byte("Jane Doe")) {
		t.Error("expected Sniff to reject plain English text")
	}
}

func TestSniffRejectsXMLLike(t *testing.T) {
	if sniff([]byte(`<?xml version="1.0"?><root/>`)) {
		t.Error("expected Sniff to reject XML-like content")
	}
}

// --- Extract: path/value correctness ---

func TestExtractNestedObjectAndArray(t *testing.T) {
	units, err := extract(t, []byte(`{"a":{"b":[1,2]}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := texts(units)
	want := []string{".a.b[0] = 1", ".a.b[1] = 2"}
	if len(got) != len(want) {
		t.Fatalf("got %d units %v, want %d units %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("unit %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtractNonIdentifierKeyDash(t *testing.T) {
	units, err := extract(t, []byte(`{"foo-bar":1}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	want := `.["foo-bar"] = 1`
	if units[0].Text != want {
		t.Errorf("got %q, want %q (dash keys must be bracketed, not a bare .foo-bar which jq would misparse as subtraction)", units[0].Text, want)
	}
}

func TestExtractNonIdentifierKeySpace(t *testing.T) {
	units, err := extract(t, []byte(`{"my key":1}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	want := `.["my key"] = 1`
	if units[0].Text != want {
		t.Errorf("got %q, want %q", units[0].Text, want)
	}
}

func TestExtractNonIdentifierKeyLeadingDigit(t *testing.T) {
	units, err := extract(t, []byte(`{"1st":1}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	want := `.["1st"] = 1`
	if units[0].Text != want {
		t.Errorf("got %q, want %q", units[0].Text, want)
	}
}

func TestExtractEmptyStringKey(t *testing.T) {
	units, err := extract(t, []byte(`{"":1}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	want := `.[""] = 1`
	if units[0].Text != want {
		t.Errorf("got %q, want %q", units[0].Text, want)
	}
}

func TestExtractRootTopLevelScalarNumber(t *testing.T) {
	units, err := extract(t, []byte(`42`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	// Must be exactly ". = 42" -- not ".. = 42" and not "= 42".
	want := `. = 42`
	if units[0].Text != want {
		t.Errorf("got %q, want %q (root path must not be doubled)", units[0].Text, want)
	}
	loc, ok := units[0].Location.(jsonPathLocation)
	if !ok {
		t.Fatalf("location type = %T, want jsonPathLocation", units[0].Location)
	}
	if loc.Path != "." {
		t.Errorf("Path = %q, want %q", loc.Path, ".")
	}
}

func TestExtractRootTopLevelScalarString(t *testing.T) {
	units, err := extract(t, []byte(`"hi"`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	want := `. = "hi"`
	if units[0].Text != want {
		t.Errorf("got %q, want %q", units[0].Text, want)
	}
}

func TestExtractRootTopLevelArray(t *testing.T) {
	units, err := extract(t, []byte(`[1,2]`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := texts(units)
	// This was an actual bug: a naive root+"[0]" concatenation could
	// either double the dot (produce "..[0]") or drop it entirely
	// (produce a bare "[0]" with no leading dot, which is not valid jq
	// syntax on its own when pasted). The correct rendering is ".[0]".
	want := []string{".[0] = 1", ".[1] = 2"}
	if len(got) != len(want) {
		t.Fatalf("got %d units %v, want %d units %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("unit %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtractRootTopLevelObjectNonIdentifierKey(t *testing.T) {
	units, err := extract(t, []byte(`{"foo-bar":1}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	// Must be exactly '.["foo-bar"] = 1', not '..["foo-bar"] = 1'.
	want := `.["foo-bar"] = 1`
	if units[0].Text != want {
		t.Errorf("got %q, want %q (root path must not be doubled for bracketed keys either)", units[0].Text, want)
	}
}

func TestExtractLargeIntegerPrecision(t *testing.T) {
	// 1e19 is larger than float64's exact integer range (2^53); if the
	// decoder round-tripped through float64 instead of json.Number, the
	// digits would come out reformatted/incorrect.
	const big = "10000000000000000000"
	units, err := extract(t, []byte(fmt.Sprintf(`{"n":%s}`, big)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	want := ".n = " + big
	if units[0].Text != want {
		t.Errorf("got %q, want %q (large integer lost precision -- json.Number/UseNumber not wired up?)", units[0].Text, want)
	}
}

func TestExtractDecimalFidelity(t *testing.T) {
	units, err := extract(t, []byte(`{"n":0.1}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	want := `.n = 0.1`
	if units[0].Text != want {
		t.Errorf("got %q, want %q (decimal literal must match source text exactly)", units[0].Text, want)
	}
}

func TestExtractEmptyObjectContainer(t *testing.T) {
	units, err := extract(t, []byte(`{"empty":{}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	want := `.empty = {}`
	if units[0].Text != want {
		t.Errorf("got %q, want %q", units[0].Text, want)
	}
}

func TestExtractEmptyArrayContainer(t *testing.T) {
	units, err := extract(t, []byte(`{"empty":[]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	want := `.empty = []`
	if units[0].Text != want {
		t.Errorf("got %q, want %q", units[0].Text, want)
	}
}

func TestExtractDuplicateObjectKeysBothEmitted(t *testing.T) {
	units, err := extract(t, []byte(`{"a":1,"a":2}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Both duplicate keys must be emitted -- the streaming decoder
	// preserves duplicates rather than silently dropping one via a
	// map-based flatten. This is intentional; do not "fix" it.
	if !containsText(units, ".a = 1") {
		t.Errorf("missing unit %q among %v", ".a = 1", texts(units))
	}
	if !containsText(units, ".a = 2") {
		t.Errorf("missing unit %q among %v", ".a = 2", texts(units))
	}
	if len(units) != 2 {
		t.Errorf("got %d units, want 2", len(units))
	}
}

func TestExtractStringValueEscaping(t *testing.T) {
	units, err := extract(t, []byte(`{"s":"a\"b"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	// The value literal after " = " must be a valid, correctly quoted
	// JSON string literal for the value a"b.
	const prefix = ".s = "
	if !strings.HasPrefix(units[0].Text, prefix) {
		t.Fatalf("got %q, want prefix %q", units[0].Text, prefix)
	}
	literal := strings.TrimPrefix(units[0].Text, prefix)
	want := `"a\"b"`
	if literal != want {
		t.Errorf("value literal = %q, want %q", literal, want)
	}
}

func TestExtractStringValueBackslashEscaping(t *testing.T) {
	units, err := extract(t, []byte(`{"s":"a\\b"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	want := `.s = "a\\b"`
	if units[0].Text != want {
		t.Errorf("got %q, want %q", units[0].Text, want)
	}
}

// --- Context cancellation ---

func TestExtractRespectsContextCancellation(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < 200; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", i)
	}
	b.WriteByte(']')
	data := []byte(b.String())

	r := bytes.NewReader(data)
	ctx, cancel := context.WithCancel(context.Background())
	units, errc := (Extractor{}).Extract(ctx, r, int64(len(data)))

	<-units // read exactly one unit
	cancel()

	// Draining should terminate promptly once cancelled, and the
	// channels must be closed (not leaked), with no panic/hang.
	for range units {
	}
	<-errc
}

// --- Malformed JSON mid-stream ---

func TestExtractPropagatesDecoderErrorMidStream(t *testing.T) {
	// The prefix is valid-looking (an open array with one valid element)
	// but the stream is truncated/broken before a closing bracket -- the
	// decoder must surface an error on errc, not truncate silently or
	// panic.
	data := []byte(`[1,2,`)
	units, err := extract(t, data)
	if err == nil {
		t.Fatal("expected an error for malformed mid-stream JSON, got nil")
	}
	// Whatever was validly decoded before the break may have been
	// emitted, but there must be no panic and the units slice must not
	// contain a spurious/synthetic completion of the array.
	for _, u := range units {
		if strings.Contains(u.Text, "[2]") {
			t.Errorf("unexpected unit for a value beyond the truncation: %q", u.Text)
		}
	}
}

func TestExtractPropagatesDecoderErrorOnInvalidToken(t *testing.T) {
	data := []byte(`{"a": tru`) // "tru" is not a valid JSON token
	_, err := extract(t, data)
	if err == nil {
		t.Fatal("expected an error for an invalid token, got nil")
	}
}
