package xlsx

// parseCellRef parses an OOXML cell reference like "B45" or "AA1" into
// its 1-based column and row numbers. Columns use bijective base-26
// (A=1, ..., Z=26, AA=27, AZ=52, ...). It returns ok=false for anything
// that isn't [letters][digits] with at least one of each, so callers
// can fall back gracefully instead of panicking on malformed input.
func parseCellRef(ref string) (col, row int, ok bool) {
	n := len(ref)
	i := 0
	for i < n && isAlpha(ref[i]) {
		i++
	}
	if i == 0 || i == n {
		return 0, 0, false
	}

	col = 0
	for j := 0; j < i; j++ {
		c := upper(ref[j])
		col = col*26 + int(c-'A'+1)
	}

	row = 0
	for j := i; j < n; j++ {
		c := ref[j]
		if c < '0' || c > '9' {
			return 0, 0, false
		}
		row = row*10 + int(c-'0')
	}
	if row == 0 {
		return 0, 0, false
	}
	return col, row, true
}

// colLetters renders a 1-based column number back into its bijective
// base-26 letters (1=A, 26=Z, 27=AA, ...). Used only to synthesize a
// Location.Cell string when a worksheet cell is missing its own r
// attribute (malformed input) but we still tracked its column via a
// running counter.
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

func isAlpha(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func upper(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - ('a' - 'A')
	}
	return b
}
