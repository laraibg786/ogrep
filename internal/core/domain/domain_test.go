package domain

import "testing"

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
