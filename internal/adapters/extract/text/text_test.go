package text

import (
	"bytes"
	"context"
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

func TestLineLocationFields(t *testing.T) {
	loc := lineLocation{Line: 42}
	fields := loc.Fields(nil)
	line, ok := fields["line"].(int)
	if !ok || line != 42 {
		t.Errorf("Fields()[\"line\"] = %v (%T), want int 42", fields["line"], fields["line"])
	}
	col, ok := fields["col"].(int)
	if !ok || col != 1 {
		t.Errorf("Fields(nil)[\"col\"] = %v (%T), want int 1 (no span given)", fields["col"], fields["col"])
	}
}

// TestLineLocationFieldsUsesSpanStart is the whole point of threading
// spans into Fields: the reported column must be where the match
// actually starts on the line, not always 1.
func TestLineLocationFieldsUsesSpanStart(t *testing.T) {
	loc := lineLocation{Line: 42}
	fields := loc.Fields([]domain.Span{{Start: 21, End: 27}})
	if got, want := fields["col"], 22; got != want {
		t.Errorf(`Fields([span 21:27])["col"] = %v, want %v (1-based span start)`, got, want)
	}
}

// TestLineLocationFieldsUsesFirstSpanWhenMultiple covers a line with more
// than one match: the reported column is the first match's start, the
// same convention rg-style tools use.
func TestLineLocationFieldsUsesFirstSpanWhenMultiple(t *testing.T) {
	loc := lineLocation{Line: 1}
	fields := loc.Fields([]domain.Span{{Start: 10, End: 14}, {Start: 20, End: 24}})
	if got, want := fields["col"], 11; got != want {
		t.Errorf(`Fields()["col"] = %v, want %v (first span's start)`, got, want)
	}
}

func TestLineLocationHyperlinkURI(t *testing.T) {
	loc := lineLocation{Line: 42}
	if got, want := loc.HyperlinkURI("/path/file.txt", nil), "file:///path/file.txt:42:1"; got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

// TestLineLocationHyperlinkURIUsesSpanStart confirms the hyperlink lands
// on the actual match, not just the start of the line -- this is what
// makes an editor's OSC-8-hyperlink jump put the cursor at the match.
func TestLineLocationHyperlinkURIUsesSpanStart(t *testing.T) {
	loc := lineLocation{Line: 42}
	got := loc.HyperlinkURI("/path/file.txt", []domain.Span{{Start: 21, End: 27}})
	want := "file:///path/file.txt:42:22"
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

func TestLineLocationHyperlinkURIEscapesPath(t *testing.T) {
	loc := lineLocation{Line: 3}
	got := loc.HyperlinkURI("/path/my file\twith\nspecial chars.txt", nil)
	want := "file:///path/my%20file%09with%0Aspecial%20chars.txt:3:1"
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

func TestSniffRejectsBinary(t *testing.T) {
	data := []byte("hello\x00world")
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.bin", r, int64(len(data))) {
		t.Error("expected Sniff to reject data containing a null byte")
	}
}

func TestSniffAcceptsPlainText(t *testing.T) {
	data := []byte("line one\nline two\n")
	r := bytes.NewReader(data)
	if !(Extractor{}).Sniff("file.txt", r, int64(len(data))) {
		t.Error("expected Sniff to accept plain text")
	}
}

func TestSniffRejectsOOXMLExtensions(t *testing.T) {
	data := []byte("PK\x03\x04 not really text but no null bytes either")
	r := bytes.NewReader(data)
	if (Extractor{}).Sniff("file.docx", r, int64(len(data))) {
		t.Error("expected Sniff to defer to office extractors for .docx")
	}
}

func TestExtractStreamsLines(t *testing.T) {
	data := []byte("first\nsecond\nthird")
	r := bytes.NewReader(data)
	units, errc := (Extractor{}).Extract(context.Background(), r, int64(len(data)))

	var got []domain.TextUnit
	for u := range units {
		got = append(got, u)
	}
	if err := <-errc; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d units, want 3", len(got))
	}
	wantTexts := []string{"first", "second", "third"}
	for i, u := range got {
		if u.Text != wantTexts[i] {
			t.Errorf("unit %d text = %q, want %q", i, u.Text, wantTexts[i])
		}
		loc, ok := u.Location.(lineLocation)
		if !ok {
			t.Fatalf("unit %d location type = %T, want lineLocation", i, u.Location)
		}
		if loc.Line != i+1 {
			t.Errorf("unit %d line = %d, want %d", i, loc.Line, i+1)
		}
	}
}

func TestExtractRespectsContextCancellation(t *testing.T) {
	data := []byte("a\nb\nc\nd\ne\n")
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
