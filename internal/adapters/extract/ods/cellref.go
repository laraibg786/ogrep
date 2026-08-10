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
		buf = append(buf, byte('A'+col%26))
		col /= 26
	}
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}
