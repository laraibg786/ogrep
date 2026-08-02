package jsondoc

import (
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

func TestJSONPathLocationHuman(t *testing.T) {
	loc := jsonPathLocation{Path: `.a.b[2]`}
	if got, want := loc.Human(), `.a.b[2]`; got != want {
		t.Errorf("Human() = %q, want %q", got, want)
	}
}

func TestJSONPathLocationHumanWithPosition(t *testing.T) {
	loc := jsonPathLocation{Path: `.a.b[2]`, Line: 4, Column: 12}
	if got, want := loc.Human(), `.a.b[2] (line 4:12)`; got != want {
		t.Errorf("Human() = %q, want %q", got, want)
	}
}

func TestJSONPathLocationFields(t *testing.T) {
	loc := jsonPathLocation{Path: `.["foo-bar"]`, Line: 4, Column: 12}
	fields := loc.Fields(nil)
	path, ok := fields["jsonpath"].(string)
	if !ok || path != `.["foo-bar"]` {
		t.Errorf(`Fields()["jsonpath"] = %v (%T), want string %q`, fields["jsonpath"], fields["jsonpath"], `.["foo-bar"]`)
	}
	if got, want := fields["line"], 4; got != want {
		t.Errorf(`Fields()["line"] = %v, want %v`, got, want)
	}
	if got, want := fields["col"], 12; got != want {
		t.Errorf(`Fields()["col"] = %v, want %v`, got, want)
	}
}

// TestJSONPathLocationHyperlinkURIWithoutPosition covers the defensive
// fallback for a Location with no known position (Line: 0, the zero
// value) -- shouldn't happen from real extraction, but HyperlinkURI must
// not fabricate a line number if it did.
func TestJSONPathLocationHyperlinkURIWithoutPosition(t *testing.T) {
	loc := jsonPathLocation{Path: ".a.b[2]"}
	got := loc.HyperlinkURI("/path/file.json", nil)
	want := domain.FileURI("/path/file.json", "")
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
	// No known position, so this must be a bare file:// URI (no '#', no
	// ":line:col" suffix).
	if want != "file:///path/file.json" {
		t.Fatalf("sanity check failed: domain.FileURI produced %q, expected no fragment for empty fragment input", want)
	}
}

// TestJSONPathLocationHyperlinkURIWithPosition covers the real case: a
// value with a known source position links straight to it, not just the
// bare file -- this is the entire point of tracking Line/Column, and
// spans is ignored (a JSON match's Span is an offset into the
// synthesized "<path> = <value>" display text, not the file, so it
// can't be used to build a file position the way a text-format Span
// can).
func TestJSONPathLocationHyperlinkURIWithPosition(t *testing.T) {
	loc := jsonPathLocation{Path: ".a.b[2]", Line: 4, Column: 12}
	got := loc.HyperlinkURI("/path/file.json", []domain.Span{{Start: 5, End: 9}})
	want := domain.FileURI("/path/file.json", "") + ":4:12"
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

func TestJSONPathLocationHyperlinkURIEscapesPath(t *testing.T) {
	loc := jsonPathLocation{Path: "."}
	got := loc.HyperlinkURI("/path/my file.json", nil)
	want := domain.FileURI("/path/my file.json", "")
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}
