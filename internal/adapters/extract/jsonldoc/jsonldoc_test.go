package jsonldoc

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
	return (Extractor{}).Sniff("file.jsonl", r, int64(len(data)))
}

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

func findByPath(units []domain.TextUnit, path string) (domain.TextUnit, bool) {
	for _, u := range units {
		if loc, ok := u.Location.(lineLocation); ok && loc.Path == path {
			return u, true
		}
	}
	return domain.TextUnit{}, false
}

// findAllByPath returns every unit whose path matches, in emission
// order -- used when the same jq path legitimately appears on more than
// one line (one JSONL record per line).
func findAllByPath(units []domain.TextUnit, path string) []domain.TextUnit {
	var out []domain.TextUnit
	for _, u := range units {
		if loc, ok := u.Location.(lineLocation); ok && loc.Path == path {
			out = append(out, u)
		}
	}
	return out
}

// --- Sniff accept cases ---

func TestSniffAcceptsFirstLineValidJSON(t *testing.T) {
	if !sniff([]byte(`{"a":1}` + "\n" + `{"a":2}` + "\n")) {
		t.Error("expected Sniff to accept a file whose first line is valid JSON")
	}
}

func TestSniffSkipsLeadingBlankLinesToFindFirstRealLine(t *testing.T) {
	if !sniff([]byte("\n   \n" + `{"a":1}` + "\n")) {
		t.Error("expected Sniff to skip leading blank lines and accept the first real one")
	}
}

// --- Sniff reject cases ---

func TestSniffRejectsEmptyFile(t *testing.T) {
	if sniff([]byte("")) {
		t.Error("expected Sniff to reject an empty file")
	}
}

func TestSniffRejectsBlankLinesOnly(t *testing.T) {
	if sniff([]byte("\n\n   \n\t\n")) {
		t.Error("expected Sniff to reject a file with no non-blank line at all")
	}
}

func TestSniffRejectsFirstLinePlainText(t *testing.T) {
	// Analogous to the MS Office lock-file regression other plugins guard
	// against: plain text on the first non-blank line must not be
	// claimed as JSONL.
	data := []byte("Jane Doe\n" + `{"a":1}` + "\n")
	if sniff(data) {
		t.Error("expected Sniff to reject a file whose first non-blank line isn't JSON")
	}
}

// --- Extract: real line numbers, not jsondoc's own "line 1" ---

func TestExtractSubstitutesRealLineNumbers(t *testing.T) {
	data := []byte(`{"user":"Ada"}` + "\n" + `{"user":"Grace"}` + "\n" + `{"user":"Ada"}` + "\n")
	got, err := extract(t, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	matches := findAllByPath(got, ".user")
	if len(matches) != 3 {
		t.Fatalf("got %d units at .user, want 3: %+v", len(matches), got)
	}
	wantLines := []int{1, 2, 3}
	for i, u := range matches {
		loc := u.Location.(lineLocation)
		if loc.Line != wantLines[i] {
			t.Errorf("unit %d: Line = %d, want %d", i, loc.Line, wantLines[i])
		}
	}
	if matches[0].Text != `.user = "Ada"` {
		t.Errorf("line 1 text = %q, want %q", matches[0].Text, `.user = "Ada"`)
	}
	if matches[1].Text != `.user = "Grace"` {
		t.Errorf("line 2 text = %q, want %q", matches[1].Text, `.user = "Grace"`)
	}
}

// TestExtractPathHasNoDocumentPrefix is the key design decision this
// package makes differently from yamldoc's multi-document ".document[N]"
// prefix: jq's default input mode already treats each top-level value in
// NDJSON input as a separate item to apply the same filter to (this is
// exactly what makes `jq '.foo'` a well-known NDJSON idiom), so a bare
// per-record path is directly correct pasted into jq against the whole
// file -- no synthetic prefix should be added.
func TestExtractPathHasNoDocumentPrefix(t *testing.T) {
	data := []byte(`{"a":1}` + "\n" + `{"a":2}` + "\n")
	got, err := extract(t, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	matches := findAllByPath(got, ".a")
	if len(matches) != 2 {
		t.Fatalf("got %d units at .a, want 2: %+v", len(matches), got)
	}
	for _, u := range matches {
		loc := u.Location.(lineLocation)
		if strings.Contains(loc.Path, "document") {
			t.Errorf("path = %q must not contain a document-index prefix", loc.Path)
		}
	}
}

func TestExtractSkipsBlankSeparatorLines(t *testing.T) {
	data := []byte(`{"a":1}` + "\n\n   \n" + `{"a":2}` + "\n")
	got, err := extract(t, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	matches := findAllByPath(got, ".a")
	if len(matches) != 2 {
		t.Fatalf("got %d units at .a, want 2 (blank lines must not error or produce phantom units): %+v", len(matches), got)
	}
	// Real line numbers must still count the blank lines, i.e. the
	// second record is genuinely on line 4, not line 2.
	if got := matches[1].Location.(lineLocation).Line; got != 4 {
		t.Errorf("second record's Line = %d, want 4", got)
	}
}

func TestExtractNonIdentifierKeyEscapesCorrectly(t *testing.T) {
	// Reuses jsondoc's own jq-escaping logic via composition -- this
	// confirms that reuse actually took effect, not just that jsonldoc
	// compiles.
	data := []byte(`{"foo-bar":1}` + "\n")
	got, err := extract(t, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := findByPath(got, `.["foo-bar"]`); !ok {
		t.Fatalf(`expected a unit at .["foo-bar"]; units: %+v`, got)
	}
}

// --- Malformed line aborts with context ---

func TestExtractReturnsErrorIdentifyingBadLine(t *testing.T) {
	data := []byte(`{"a":1}` + "\n" + `{"a":` + "\n") // line 2 is truncated
	_, err := extract(t, data)
	if err == nil {
		t.Fatal("expected an error for a malformed second line, got nil")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error = %q, want it to identify \"line 2\"", err.Error())
	}
}

// --- Context cancellation ---

func TestExtractRespectsContextCancellation(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&sb, `{"i":%d}`+"\n", i)
	}
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

// --- Location ---

func TestLineLocationHuman(t *testing.T) {
	loc := lineLocation{Path: ".a", Line: 3, Column: 6}
	if got, want := loc.Human(), ".a (line 3:6)"; got != want {
		t.Errorf("Human() = %q, want %q", got, want)
	}
}

func TestLineLocationFields(t *testing.T) {
	loc := lineLocation{Path: ".a", Line: 3, Column: 6}
	fields := loc.Fields(nil)
	if got, want := fields["jsonpath"], ".a"; got != want {
		t.Errorf(`Fields()["jsonpath"] = %v, want %q`, got, want)
	}
	if got, want := fields["line"], 3; got != want {
		t.Errorf(`Fields()["line"] = %v, want %v`, got, want)
	}
	if got, want := fields["col"], 6; got != want {
		t.Errorf(`Fields()["col"] = %v, want %v`, got, want)
	}
}

func TestLineLocationHyperlinkURI(t *testing.T) {
	loc := lineLocation{Path: ".a", Line: 3, Column: 6}
	got := loc.HyperlinkURI("/path/events.jsonl", nil)
	want := domain.FileURI("/path/events.jsonl", "") + ":3:6"
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}
