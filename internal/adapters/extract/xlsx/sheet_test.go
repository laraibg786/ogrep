package xlsx

import "testing"

func TestCellLocationHyperlinkURI(t *testing.T) {
	loc := cellLocation{Sheet: "Sheet1", Cell: "B45"}
	if got, want := loc.HyperlinkURI("/path/data.xlsx"), "file:///path/data.xlsx#Sheet1!B45"; got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

func TestCellLocationHyperlinkURIEscapesPathAndSheetName(t *testing.T) {
	loc := cellLocation{Sheet: "My Sheet", Cell: "B45"}
	got := loc.HyperlinkURI("/path/my data.xlsx")
	want := "file:///path/my%20data.xlsx#%27My%20Sheet%27!B45"
	if got != want {
		t.Errorf("HyperlinkURI() = %q, want %q", got, want)
	}
}

func TestQuotedSheetRef(t *testing.T) {
	cases := []struct {
		sheet string
		want  string
	}{
		{"Sheet1", "Sheet1"},
		{"My_Sheet2", "My_Sheet2"},
		{"My Sheet", "'My Sheet'"},
		{"Q1'25", "'Q1''25'"},
		{"Sales/EMEA", "'Sales/EMEA'"},
	}
	for _, tc := range cases {
		if got := quotedSheetRef(tc.sheet); got != tc.want {
			t.Errorf("quotedSheetRef(%q) = %q, want %q", tc.sheet, got, tc.want)
		}
	}
}
