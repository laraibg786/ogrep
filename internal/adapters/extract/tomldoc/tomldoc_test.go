package tomldoc

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// sniff is a small helper that wraps data in a bytes.Reader (which
// implements io.ReaderAt) and calls Sniff, matching the construction
// style used by jsondoc_test.go.
func sniff(data []byte) bool {
	r := bytes.NewReader(data)
	return (Extractor{}).Sniff("file.toml", r, int64(len(data)))
}

// extract runs Extract to completion and returns the collected units
// and the final error (nil if none).
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

// paths returns the jq/yq path of every non-comment unit, in order,
// panicking (via t.Fatalf) if any unit's Location isn't a
// tomlPathLocation -- used by tests that care about path/order rather
// than exact line text.
func paths(t *testing.T, units []domain.TextUnit) []string {
	t.Helper()
	out := make([]string, len(units))
	for i, u := range units {
		loc, ok := u.Location.(tomlPathLocation)
		if !ok {
			t.Fatalf("unit %d Location type = %T, want tomlPathLocation", i, u.Location)
		}
		out[i] = loc.Path
	}
	return out
}

// --- Sniff accept cases ---

func TestSniffAcceptsSimpleTable(t *testing.T) {
	if !sniff([]byte("title = \"hello\"\n")) {
		t.Error("expected Sniff to accept a simple TOML key/value")
	}
}

func TestSniffAcceptsNestedTable(t *testing.T) {
	data := "[owner]\nname = \"Tom\"\n"
	if !sniff([]byte(data)) {
		t.Error("expected Sniff to accept a TOML file with a table header")
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

// TestSniffRejectsCommentOnly is a regression-shaped test: a TOML
// document containing only comments parses "successfully" (zero
// top-level expressions, no error) when KeepComments is left false,
// which would falsely claim a plain-text file full of "# ..." lines as
// TOML without the explicit "at least one real expression" guard in
// Sniff.
func TestSniffRejectsCommentOnly(t *testing.T) {
	if sniff([]byte("# just a comment\n# another one\n")) {
		t.Error("expected Sniff to reject a comment-only document (no real structure)")
	}
}

// TestSniffRejectsPlainEnglishText is a regression-shaped test
// mirroring the equivalent case in json/yaml: an MS Office lock-file
// style plain-text fixture must not be mistaken for TOML.
func TestSniffRejectsPlainEnglishText(t *testing.T) {
	if sniff([]byte("Jane Doe")) {
		t.Error("expected Sniff to reject plain English text")
	}
}

func TestSniffRejectsMalformedTOML(t *testing.T) {
	if sniff([]byte("key = = =")) {
		t.Error("expected Sniff to reject malformed TOML")
	}
	if sniff([]byte("bareword = notquoted")) {
		t.Error("expected Sniff to reject an unquoted bare-word value (not valid TOML)")
	}
}

func TestSniffRejectsOversizedFile(t *testing.T) {
	// Not actually generating maxSniffSize+1 bytes of data; just confirm
	// the size gate itself is wired up by checking Sniff declines when
	// size claims to exceed maxSniffSize even though the reader is tiny.
	// Sniff reads via ra.ReadAt bounded by the passed size, so a
	// mismatched size alone is enough to exercise the size-gate branch
	// without allocating a huge buffer.
	data := []byte("a = 1\n")
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.toml", r, maxSniffSize+1) {
		t.Error("expected Sniff to reject a file reporting a size over maxSniffSize")
	}
}

// --- Extract: path/text/position correctness ---

func TestExtractSimpleKeyValue(t *testing.T) {
	units, err := extract(t, []byte(`title = "hello"`+"\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1: %v", len(units), texts(units))
	}
	if want := `title = "hello"`; units[0].Text != want {
		t.Errorf("Text = %q, want %q (verbatim source line)", units[0].Text, want)
	}
	loc, ok := units[0].Location.(tomlPathLocation)
	if !ok {
		t.Fatalf("Location type = %T, want tomlPathLocation", units[0].Location)
	}
	if loc.Path != ".title" {
		t.Errorf("Path = %q, want %q", loc.Path, ".title")
	}
	if loc.Line != 1 {
		t.Errorf("Line = %d, want 1", loc.Line)
	}
	if loc.Column <= 0 {
		t.Errorf("Column = %d, want > 0", loc.Column)
	}
}

func TestExtractNestedTable(t *testing.T) {
	data := "[owner]\nname = \"Tom\"\n"
	units, err := extract(t, []byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1: %v", len(units), texts(units))
	}
	if want := `name = "Tom"`; units[0].Text != want {
		t.Errorf("Text = %q, want %q", units[0].Text, want)
	}
	loc := units[0].Location.(tomlPathLocation)
	if loc.Path != ".owner.name" {
		t.Errorf("Path = %q, want %q", loc.Path, ".owner.name")
	}
	if loc.Line != 2 {
		t.Errorf("Line = %d, want 2 (the second physical line)", loc.Line)
	}
}

func TestExtractDottedKeys(t *testing.T) {
	units, err := extract(t, []byte("a.b.c = 1\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1: %v", len(units), texts(units))
	}
	if want := "a.b.c = 1"; units[0].Text != want {
		t.Errorf("Text = %q, want %q", units[0].Text, want)
	}
	if loc := units[0].Location.(tomlPathLocation); loc.Path != ".a.b.c" {
		t.Errorf("Path = %q, want %q", loc.Path, ".a.b.c")
	}
}

func TestExtractArray(t *testing.T) {
	units, err := extract(t, []byte("nums = [1, 2, 3]\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantPaths := []string{".nums[0]", ".nums[1]", ".nums[2]"}
	gotPaths := paths(t, units)
	if len(gotPaths) != len(wantPaths) {
		t.Fatalf("got %d units %v, want %d units %v", len(gotPaths), texts(units), len(wantPaths), wantPaths)
	}
	for i := range wantPaths {
		if gotPaths[i] != wantPaths[i] {
			t.Errorf("unit %d path = %q, want %q", i, gotPaths[i], wantPaths[i])
		}
		// All three elements share one physical source line: each unit's
		// Text is that whole verbatim line, disambiguated only by its
		// distinct Location (path/column), not by Text -- see the
		// package doc comment on TextUnit granularity.
		if want := "nums = [1, 2, 3]"; units[i].Text != want {
			t.Errorf("unit %d Text = %q, want %q", i, units[i].Text, want)
		}
	}
}

// TestExtractMultilineArray confirms each element of a multi-line array
// gets its own real line number and its own distinct verbatim physical
// line as Text, including a trailing per-element comment showing up as
// a separate, searchable comment TextUnit.
func TestExtractMultilineArray(t *testing.T) {
	data := "nums = [\n  1, # one\n  2,\n  3,\n]\n"
	units, err := extract(t, []byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got []struct {
		path string
		line int
		text string
	}
	var comment struct {
		found bool
		line  int
		text  string
	}
	for _, u := range units {
		switch loc := u.Location.(type) {
		case tomlPathLocation:
			got = append(got, struct {
				path string
				line int
				text string
			}{loc.Path, loc.Line, u.Text})
		case tomlCommentLocation:
			comment = struct {
				found bool
				line  int
				text  string
			}{true, loc.Line, u.Text}
		}
	}

	want := []struct {
		path string
		line int
		text string
	}{
		{".nums[0]", 2, "  1, # one"},
		{".nums[1]", 3, "  2,"},
		{".nums[2]", 4, "  3,"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d value units %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("unit %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !comment.found {
		t.Fatal("expected the inline \"# one\" comment to be its own searchable TextUnit")
	}
	if comment.line != 2 || comment.text != "  1, # one" {
		t.Errorf("comment = %+v, want line 2, text %q", comment, "  1, # one")
	}
}

func TestExtractArrayOfTables(t *testing.T) {
	data := "[[servers]]\nname = \"alpha\"\n\n[[servers]]\nname = \"beta\"\n"
	units, err := extract(t, []byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantPaths := []string{".servers[0].name", ".servers[1].name"}
	gotPaths := paths(t, units)
	if len(gotPaths) != len(wantPaths) {
		t.Fatalf("got %d units %v, want %d units %v", len(gotPaths), texts(units), len(wantPaths), wantPaths)
	}
	for i := range wantPaths {
		if gotPaths[i] != wantPaths[i] {
			t.Errorf("unit %d path = %q, want %q", i, gotPaths[i], wantPaths[i])
		}
	}
	if units[0].Text != `name = "alpha"` {
		t.Errorf("unit 0 Text = %q, want %q", units[0].Text, `name = "alpha"`)
	}
	if units[1].Text != `name = "beta"` {
		t.Errorf("unit 1 Text = %q, want %q", units[1].Text, `name = "beta"`)
	}
}

func TestExtractInlineTable(t *testing.T) {
	units, err := extract(t, []byte("point = { x = 1, y = 2 }\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gotPaths := paths(t, units)
	sort.Strings(gotPaths)
	want := []string{".point.x", ".point.y"}
	if len(gotPaths) != len(want) {
		t.Fatalf("got %d units %v, want %d units %v", len(gotPaths), texts(units), len(want), want)
	}
	for i := range want {
		if gotPaths[i] != want[i] {
			t.Errorf("unit %d path = %q, want %q", i, gotPaths[i], want[i])
		}
	}
	for _, u := range units {
		if u.Text != "point = { x = 1, y = 2 }" {
			t.Errorf("Text = %q, want %q", u.Text, "point = { x = 1, y = 2 }")
		}
	}
}

func TestExtractNonIdentifierKeyDash(t *testing.T) {
	units, err := extract(t, []byte(`"foo-bar" = 1`+"\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	if loc := units[0].Location.(tomlPathLocation); loc.Path != `.["foo-bar"]` {
		t.Errorf("Path = %q, want %q (dash keys must be bracketed, not a bare .foo-bar which jq would misparse as subtraction)", loc.Path, `.["foo-bar"]`)
	}
	if want := `"foo-bar" = 1`; units[0].Text != want {
		t.Errorf("Text = %q, want %q", units[0].Text, want)
	}
}

func TestExtractNonIdentifierKeySpace(t *testing.T) {
	units, err := extract(t, []byte(`"my key" = 1`+"\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	if loc := units[0].Location.(tomlPathLocation); loc.Path != `.["my key"]` {
		t.Errorf("Path = %q, want %q", loc.Path, `.["my key"]`)
	}
}

func TestExtractNonIdentifierKeyLeadingDigit(t *testing.T) {
	units, err := extract(t, []byte(`"1st" = 1`+"\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	if loc := units[0].Location.(tomlPathLocation); loc.Path != `.["1st"]` {
		t.Errorf("Path = %q, want %q", loc.Path, `.["1st"]`)
	}
}

// TestExtractEmptyTable exercises a table header that never receives
// any key: this can only be resolved once the whole document has been
// scanned (see the package doc comment), and, unlike the old
// implementation's reconstructed "= {}" text, its Text is the header
// line's own real, verbatim source ("[empty]"), not a synthesized
// value.
func TestExtractEmptyTable(t *testing.T) {
	data := "[empty]\n"
	units, err := extract(t, []byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1: %v", len(units), texts(units))
	}
	if want := "[empty]"; units[0].Text != want {
		t.Errorf("Text = %q, want %q", units[0].Text, want)
	}
	loc := units[0].Location.(tomlPathLocation)
	if loc.Path != ".empty" {
		t.Errorf("Path = %q, want %q", loc.Path, ".empty")
	}
	if loc.Line != 1 {
		t.Errorf("Line = %d, want 1", loc.Line)
	}
}

// TestExtractNonEmptyTableHasNoHeaderUnit is the counterpart of
// TestExtractEmptyTable: a table header that DOES receive a key must
// not also get its own placeholder-resolved unit -- only the real
// key/value line should be emitted.
func TestExtractNonEmptyTableHasNoHeaderUnit(t *testing.T) {
	data := "[owner]\nname = \"Tom\"\n"
	units, err := extract(t, []byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1 (no separate unit for the non-empty [owner] header): %v", len(units), texts(units))
	}
}

// TestExtractNestedEmptyTableUnderPopulatedAncestor confirms an
// ancestor table that gains a subtable (but no scalar key of its own)
// counts as populated, so only the truly-empty leaf table gets a unit.
func TestExtractNestedEmptyTableUnderPopulatedAncestor(t *testing.T) {
	data := "[a]\n[a.b]\n"
	units, err := extract(t, []byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1: %v", len(units), texts(units))
	}
	loc := units[0].Location.(tomlPathLocation)
	if loc.Path != ".a.b" {
		t.Errorf("Path = %q, want %q (only the innermost, truly-empty table)", loc.Path, ".a.b")
	}
}

func TestExtractEmptyArray(t *testing.T) {
	units, err := extract(t, []byte("empty = []\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1: %v", len(units), texts(units))
	}
	if want := "empty = []"; units[0].Text != want {
		t.Errorf("Text = %q, want %q", units[0].Text, want)
	}
	if loc := units[0].Location.(tomlPathLocation); loc.Path != ".empty" {
		t.Errorf("Path = %q, want %q", loc.Path, ".empty")
	}
}

func TestExtractBoolean(t *testing.T) {
	units, err := extract(t, []byte("flag = true\nother = false\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsText(units, "flag = true") {
		t.Errorf("missing unit %q among %v", "flag = true", texts(units))
	}
	if !containsText(units, "other = false") {
		t.Errorf("missing unit %q among %v", "other = false", texts(units))
	}
}

func TestExtractFloat(t *testing.T) {
	units, err := extract(t, []byte("pi = 3.14\nround = 100.0\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsText(units, "pi = 3.14") {
		t.Errorf("missing unit %q among %v", "pi = 3.14", texts(units))
	}
	if !containsText(units, "round = 100.0") {
		t.Errorf("missing unit %q among %v (float literal must keep a decimal point)", "round = 100.0", texts(units))
	}
}

func TestExtractSpecialFloats(t *testing.T) {
	units, err := extract(t, []byte("a = inf\nb = -inf\nc = nan\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsText(units, "a = inf") {
		t.Errorf("missing unit %q among %v", "a = inf", texts(units))
	}
	if !containsText(units, "b = -inf") {
		t.Errorf("missing unit %q among %v", "b = -inf", texts(units))
	}
	if !containsText(units, "c = nan") {
		t.Errorf("missing unit %q among %v", "c = nan", texts(units))
	}
}

func TestExtractLargeInteger(t *testing.T) {
	units, err := extract(t, []byte("big = 9223372036854775807\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "big = 9223372036854775807"
	if !containsText(units, want) {
		t.Errorf("missing unit %q among %v", want, texts(units))
	}
}

// --- Extract: verbatim source preservation (the new decoder's whole
// reason for existing over the old, re-encoding one) ---

func TestExtractHexIntegerVerbatim(t *testing.T) {
	units, err := extract(t, []byte("hex = 0xdeadBEEF\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "hex = 0xdeadBEEF"
	if !containsText(units, want) {
		t.Errorf("missing unit %q among %v (hex integer must be preserved verbatim, not re-encoded to decimal)", want, texts(units))
	}
}

func TestExtractOctalIntegerVerbatim(t *testing.T) {
	units, err := extract(t, []byte("oct = 0o755\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "oct = 0o755"
	if !containsText(units, want) {
		t.Errorf("missing unit %q among %v (octal integer must be preserved verbatim)", want, texts(units))
	}
}

func TestExtractBinaryIntegerVerbatim(t *testing.T) {
	units, err := extract(t, []byte("bin = 0b1010\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "bin = 0b1010"
	if !containsText(units, want) {
		t.Errorf("missing unit %q among %v (binary integer must be preserved verbatim)", want, texts(units))
	}
}

// TestExtractLegacyEscapeSequenceVerbatim confirms a search for the raw
// escape-sequence bytes actually in the file (e.g. `\n` as two literal
// source characters, backslash-n) finds a match -- the old
// BurntSushi/toml-based implementation decoded the string first and
// then re-escaped it for display, which happened to produce the same
// text for this particular case, but did so via decode-then-re-encode
// rather than ever touching the original bytes. This asserts the new
// implementation's Text is the literal source slice, not a re-escaping.
func TestExtractLegacyEscapeSequenceVerbatim(t *testing.T) {
	units, err := extract(t, []byte(`esc = "line1\nline2\ttab"`+"\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `esc = "line1\nline2\ttab"`
	if !containsText(units, want) {
		t.Errorf("missing unit %q among %v", want, texts(units))
	}
}

func TestExtractOffsetDatetime(t *testing.T) {
	units, err := extract(t, []byte("d = 1979-05-27T07:32:00Z\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "d = 1979-05-27T07:32:00Z"
	if !containsText(units, want) {
		t.Errorf("missing unit %q among %v", want, texts(units))
	}
}

func TestExtractLocalDate(t *testing.T) {
	units, err := extract(t, []byte("d = 1979-05-27\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "d = 1979-05-27"
	if !containsText(units, want) {
		t.Errorf("missing unit %q among %v", want, texts(units))
	}
}

func TestExtractLocalTime(t *testing.T) {
	units, err := extract(t, []byte("t = 07:32:00\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "t = 07:32:00"
	if !containsText(units, want) {
		t.Errorf("missing unit %q among %v", want, texts(units))
	}
}

func TestExtractLocalDatetime(t *testing.T) {
	units, err := extract(t, []byte("dt = 1979-05-27T07:32:00\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "dt = 1979-05-27T07:32:00"
	if !containsText(units, want) {
		t.Errorf("missing unit %q among %v", want, texts(units))
	}
}

func TestExtractStringEscaping(t *testing.T) {
	units, err := extract(t, []byte(`s = "a\"b"`+"\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	want := `s = "a\"b"`
	if units[0].Text != want {
		t.Errorf("got %q, want %q", units[0].Text, want)
	}
}

// TestExtractMultilineStringVerbatim confirms a multi-line basic string
// spans multiple TextUnits (Text never embeds a newline -- see
// domain.TextUnit's doc comment), each with its own real, verbatim
// physical line and the same jq path (see the package doc comment on
// TextUnit granularity for values spanning more than one line).
func TestExtractMultilineStringVerbatim(t *testing.T) {
	data := "multi = \"\"\"\nhello\nworld\"\"\"\n"
	units, err := extract(t, []byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1 (one leaf value, positioned at the string's start): %v", len(units), texts(units))
	}
	for _, u := range units {
		if strings.Contains(u.Text, "\n") {
			t.Errorf("Text = %q contains an embedded newline, which domain.TextUnit forbids", u.Text)
		}
	}
	loc := units[0].Location.(tomlPathLocation)
	if loc.Path != ".multi" {
		t.Errorf("Path = %q, want %q", loc.Path, ".multi")
	}
	if loc.Line != 1 {
		t.Errorf("Line = %d, want 1 (the string's own start line)", loc.Line)
	}
	if want := `multi = """`; units[0].Text != want {
		t.Errorf("Text = %q, want %q (the physical line the string starts on)", units[0].Text, want)
	}
}

// TestExtractStringNotHTMLEscaped is a regression test: the value's
// verbatim source text must never be HTML-escaped -- there's no
// json.Marshal step left in this path at all now, but this guards
// against ever reintroducing one.
func TestExtractStringNotHTMLEscaped(t *testing.T) {
	units, err := extract(t, []byte(`u = "https://example.com/a<b&c>d"`+"\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1: %v", len(units), texts(units))
	}
	want := `u = "https://example.com/a<b&c>d"`
	if units[0].Text != want {
		t.Errorf("got %q, want %q (value must not be HTML-escaped)", units[0].Text, want)
	}
	for _, escaped := range []string{"\\u003c", "\\u0026", "\\u003e"} {
		if strings.Contains(units[0].Text, escaped) {
			t.Errorf("text contains HTML-escaped unicode sequence %q: %q", escaped, units[0].Text)
		}
	}
	if !strings.Contains(units[0].Text, "a<b&c>d") {
		t.Errorf("expected rendered text to contain literal %q, got %q", "a<b&c>d", units[0].Text)
	}
}

// TestExtractNonIdentifierKeyNotHTMLEscaped is the jqSegment-side
// counterpart of TestExtractStringNotHTMLEscaped: a bracketed key
// segment (used for Path, not Text) must also not HTML-escape '<',
// '>', or '&'.
func TestExtractNonIdentifierKeyNotHTMLEscaped(t *testing.T) {
	units, err := extract(t, []byte(`"a<b&c>d" = 1`+"\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	loc := units[0].Location.(tomlPathLocation)
	want := `.["a<b&c>d"]`
	if loc.Path != want {
		t.Errorf("Path = %q, want %q (key must not be HTML-escaped)", loc.Path, want)
	}
}

// --- Comments ---

func TestExtractStandaloneCommentIsSearchable(t *testing.T) {
	data := "# a standalone comment\na = 1\n"
	units, err := extract(t, []byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, u := range units {
		loc, ok := u.Location.(tomlCommentLocation)
		if !ok {
			continue
		}
		found = true
		if loc.Line != 1 {
			t.Errorf("comment Line = %d, want 1", loc.Line)
		}
		if u.Text != "# a standalone comment" {
			t.Errorf("comment Text = %q, want %q", u.Text, "# a standalone comment")
		}
		if f := loc.Fields(nil); f["comment"] != true {
			t.Errorf(`Fields()["comment"] = %v, want true`, f["comment"])
		}
	}
	if !found {
		t.Fatalf("expected a comment TextUnit among %v", texts(units))
	}
}

func TestExtractInlineTrailingCommentIsSearchable(t *testing.T) {
	data := "name = \"Tom\" # inline comment\n"
	units, err := extract(t, []byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var sawValue, sawComment bool
	for _, u := range units {
		switch u.Location.(type) {
		case tomlPathLocation:
			sawValue = true
		case tomlCommentLocation:
			sawComment = true
			if u.Text != `name = "Tom" # inline comment` {
				t.Errorf("comment Text = %q, want the full physical line", u.Text)
			}
		}
	}
	if !sawValue {
		t.Error("expected the key/value itself to still be emitted as its own unit")
	}
	if !sawComment {
		t.Error("expected the trailing comment to be emitted as its own searchable unit")
	}
}

func TestExtractTableHeaderTrailingCommentIsSearchable(t *testing.T) {
	data := "[owner] # header trailing comment\nname = \"Tom\"\n"
	units, err := extract(t, []byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, u := range units {
		if loc, ok := u.Location.(tomlCommentLocation); ok {
			found = true
			if loc.Line != 1 {
				t.Errorf("comment Line = %d, want 1", loc.Line)
			}
		}
	}
	if !found {
		t.Fatalf("expected the [owner] header's trailing comment to be searchable, among %v", texts(units))
	}
}

// --- isBareIdent (regexp-replacement equivalence) ---

func TestIsBareIdent(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"", false},
		{"a", true},
		{"_", true},
		{"A", true},
		{"foo", true},
		{"foo_bar", true},
		{"FooBar123", true},
		{"_leading_underscore", true},
		{"1st", false},
		{"9", false},
		{"foo-bar", false},
		{"my key", false},
		{"foo.bar", false},
		{"héllo", false},
		{"日本語", false},
		{"foo\tbar", false},
		{"foo\nbar", false},
	}
	for _, c := range cases {
		if got := isBareIdent(c.key); got != c.want {
			t.Errorf("isBareIdent(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

// --- Location: real position, and Human()'s bare-path format ---

// TestExtractLocationHasRealPosition is the direct replacement for the
// old TestExtractLocationHasNoPosition: unstable.Parser gives every
// value a real source position, so Line/Column are no longer
// permanently 0. Human() renders just the jq path with no line/column
// decoration -- the terminal's OSC 8 hyperlink (see HyperlinkURI)
// already carries the real line:column for click-to-navigate, and
// Fields()/JSON carries it for scripting, so Human() doesn't need to
// duplicate it.
func TestExtractLocationHasRealPosition(t *testing.T) {
	units, err := extract(t, []byte("a = 1\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	loc, ok := units[0].Location.(tomlPathLocation)
	if !ok {
		t.Fatalf("location type = %T, want tomlPathLocation", units[0].Location)
	}
	if loc.Line != 1 {
		t.Errorf("Line = %d, want 1", loc.Line)
	}
	if loc.Column != 5 {
		t.Errorf("Column = %d, want 5 (the '1' starts at column 5 in \"a = 1\")", loc.Column)
	}
	if loc.Path != ".a" {
		t.Errorf("Path = %q, want %q", loc.Path, ".a")
	}
	if got, want := loc.Human(), ".a"; got != want {
		t.Errorf("Human() = %q, want %q (bare jq path, no line/column decoration)", got, want)
	}
	if got, want := loc.HyperlinkURI("/tmp/file.toml", nil), "file:///tmp/file.toml:1:5"; got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
	fields := loc.Fields(nil)
	if fields["tomlpath"] != ".a" {
		t.Errorf(`Fields()["tomlpath"] = %v, want %q`, fields["tomlpath"], ".a")
	}
	if fields["line"] != 1 {
		t.Errorf(`Fields()["line"] = %v, want 1`, fields["line"])
	}
	if fields["col"] != 5 {
		t.Errorf(`Fields()["col"] = %v, want 5`, fields["col"])
	}
}

// TestExtractConsoleOutputShowsPathOnceAndRealText is a regression-shaped
// test for the exact bug the rewrite was meant to fix -- but not by
// removing the path from the console line entirely. The old,
// permanently-position-less implementation rendered
// "config.toml:.path .path = value": the jq path duplicated, once from
// Human() (which had nothing else to show), once from a synthesized
// Text. Now Human() shows just the bare jq path, and Text is the real,
// verbatim source line -- two genuinely different pieces of
// information, so the path legitimately appears once, not duplicated.
// LocationString(path, loc) + " " + Text is what Terminal.WriteMatch
// actually prints (see internal/adapters/output/terminal.go).
func TestExtractConsoleOutputShowsPathOnceAndRealText(t *testing.T) {
	data := "[[servers]]\nname = \"alpha\"\n"
	units, err := extract(t, []byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1: %v", len(units), texts(units))
	}
	loc := units[0].Location.(tomlPathLocation)
	line := domain.LocationString("config.toml", loc) + " " + units[0].Text
	want := `config.toml:.servers[0].name name = "alpha"`
	if line != want {
		t.Errorf("console line = %q, want %q", line, want)
	}
	if strings.Count(line, ".servers[0].name") != 1 {
		t.Errorf("console line %q must contain the jq path exactly once, not zero or duplicated", line)
	}
}

// --- Context cancellation ---

func TestExtractRespectsContextCancellation(t *testing.T) {
	data := []byte("a = 1\nb = 2\nc = 3\nd = 4\ne = 5\n")

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

// TestExtractCancellationProducesNoError is a regression test: a
// consumer stopping early (e.g. "-m 1" on a huge file) must not
// surface a spurious "context canceled" error/warning. yamldoc had
// exactly this bug (returning ctx.Err() up through errc on ordinary
// early cancellation); jsoncdoc handles it correctly by treating
// context cancellation as a silent early exit, not a reportable
// failure. This asserts tomldoc matches jsoncdoc's behavior: after
// reading one unit and cancelling, errc must yield nil, never
// context.Canceled.
func TestExtractCancellationProducesNoError(t *testing.T) {
	data := []byte("a = 1\nb = 2\nc = 3\nd = 4\ne = 5\n")

	r := bytes.NewReader(data)
	ctx, cancel := context.WithCancel(context.Background())
	units, errc := (Extractor{}).Extract(ctx, r, int64(len(data)))

	<-units // read exactly one unit, like "-m 1" would
	cancel()

	for range units {
	}
	if err := <-errc; err != nil {
		t.Errorf("errc = %v, want nil (context cancellation must not be reported as an error)", err)
	}
}

// TestExtractAlreadyCancelledContextProducesNoError is the same
// regression, exercised via a context that is already cancelled before
// Extract is even called (the other common shape of early exit).
func TestExtractAlreadyCancelledContextProducesNoError(t *testing.T) {
	data := []byte("a = 1\nb = 2\nc = 3\n")

	r := bytes.NewReader(data)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	units, errc := (Extractor{}).Extract(ctx, r, int64(len(data)))
	for range units {
	}
	if err := <-errc; err != nil {
		t.Errorf("errc = %v, want nil (an already-cancelled context must not be reported as an error)", err)
	}
}

// --- Malformed TOML ---

func TestExtractPropagatesDecoderError(t *testing.T) {
	data := []byte("key = = =")
	_, err := extract(t, data)
	if err == nil {
		t.Fatal("expected an error for malformed TOML, got nil")
	}
}
