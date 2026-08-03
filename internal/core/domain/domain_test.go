package domain

import (
	"path/filepath"
	"testing"
)

func TestFileURIEscapesPathAndFragment(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		fragment string
		want     string
	}{
		{"space in path", "/tmp/my file.txt", "", "file:///tmp/my%20file.txt"},
		{"tab in path", "/tmp/a\tb.txt", "", "file:///tmp/a%09b.txt"},
		{"newline in path", "/tmp/a\nb.txt", "", "file:///tmp/a%0Ab.txt"},
		{"hash in path", "/tmp/a#b.txt", "", "file:///tmp/a%23b.txt"},
		{"no fragment omits #", "/tmp/plain.txt", "", "file:///tmp/plain.txt"},
		{"fragment with space", "/tmp/data.xlsx", "Sheet 1!B45", "file:///tmp/data.xlsx#Sheet%201!B45"},
		{"fragment with tab and hash", "/tmp/data.xlsx", "she#et\t1", "file:///tmp/data.xlsx#she%23et%091"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FileURI(tc.path, tc.fragment); got != tc.want {
				t.Errorf("FileURI(%q, %q) = %q, want %q", tc.path, tc.fragment, got, tc.want)
			}
		})
	}
}

// TestFileURIResolvesRelativePathToAbsolute is a regression test: every
// format's HyperlinkURI is handed whatever path the walker discovered a
// match under, which is relative whenever the search root itself was
// relative (the CLI's default root is "."). A file:// URI with a
// relative Path isn't well-formed -- there's no base for a terminal or
// browser to resolve it against -- so FileURI must always resolve to an
// absolute path before building the URI, regardless of what the caller
// passed in.
func TestFileURIResolvesRelativePathToAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	got := FileURI("sub/report.docx", "")

	want := "file://" + filepath.Join(dir, "sub", "report.docx")
	if got != want {
		t.Errorf("FileURI(%q, %q) = %q, want %q (relative path was not resolved to absolute)", "sub/report.docx", "", got, want)
	}
}
