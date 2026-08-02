package ods

// colLetters renders a 1-based column number into its bijective
// base-26 letters (1=A, 26=Z, 27=AA, ...), used to synthesize an A1-style
// cell reference string for Location.Cell (ODF's own table:table-cell
// has no built-in "A1" address the way OOXML's r attribute does -- it's
// purely positional -- so this extractor computes one itself from the
// row/col cursor, matching the reference format users already expect
// from spreadsheet formulas). Mirrors xlsx's identical colLetters.
func colLetters(col int) string {
	if col <= 0 {
		return ""
	}
	var buf []byte
	for col > 0 {
		col--
		buf = append([]byte{byte('A' + col%26)}, buf...)
		col /= 26
	}
	return string(buf)
}
