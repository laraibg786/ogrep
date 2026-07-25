package text

import (
	"bytes"
	"context"
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

func TestLineLocationFields(t *testing.T) {
	loc := lineLocation{Line: 42}
	fields := loc.Fields()
	line, ok := fields["line"].(int)
	if !ok || line != 42 {
		t.Errorf("Fields()[\"line\"] = %v (%T), want int 42", fields["line"], fields["line"])
	}
	col, ok := fields["col"].(int)
	if !ok || col != 1 {
		t.Errorf("Fields()[\"col\"] = %v (%T), want int 1", fields["col"], fields["col"])
	}
}

func TestLineLocationHyperlinkURI(t *testing.T) {
	loc := lineLocation{Line: 42}
	if got, want := loc.HyperlinkURI("/path/file.txt"), "file:///path/file.txt:42:1"; got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

func TestLineLocationHyperlinkURIEscapesPath(t *testing.T) {
	loc := lineLocation{Line: 3}
	got := loc.HyperlinkURI("/path/my file\twith\nspecial chars.txt")
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
