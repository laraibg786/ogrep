package xmldoc

import (
	"fmt"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// xmlPathLocation implements domain.Location for a single value (element
// text or attribute value) found while walking an XML document. XPath is
// an absolute, synthesized path such as "/root/item[3]" for an element's
// direct text or "/root/item[3]/@id" for an attribute; see xmldoc.go for
// how it's built. Line and Column are the 1-based source position at the
// end of the value's first direct-text run (element text) or of the
// owning element's opening tag (attribute), from
// encoding/xml.Decoder.InputPos() -- both real, not a line-only
// approximation: this is what makes a match locatable within a
// single-line (e.g. minified) XML file, where every match shares the
// same line number and a column is the only thing that distinguishes
// them.
type xmlPathLocation struct {
	XPath  string
	Line   int
	Column int
}

var _ domain.Location = xmlPathLocation{}

// Human implements domain.Location, rendering the XPath with a
// "(line N:M)" suffix when a position is known (Line is 0 for a value
// whose position couldn't be determined, which shouldn't happen in
// practice but is handled defensively).
func (l xmlPathLocation) Human() string {
	if l.Line > 0 {
		return fmt.Sprintf("%s (line %d:%d)", l.XPath, l.Line, l.Column)
	}
	return l.XPath
}

// Fields implements domain.Location. spans is unused: the
// xpath/line/column identify the matched node itself, which is the same
// regardless of whether the match landed in the element's text or (for
// an attribute) which part of the value matched.
func (l xmlPathLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"xpath": l.XPath, "line": l.Line, "col": l.Column}
}

// HyperlinkURI implements domain.Location, linking straight to the
// value's real position in the source file. spans is unused for the
// same reason as Fields: it's an offset into the match's own text (the
// element's direct text or an attribute's value), not a byte range into
// the file, so it doesn't correspond to a real file position the way a
// text-format Span does. Falls back to a bare file:// URI when Line is 0
// (position unavailable) rather than claiming line 1 is where the match
// is.
func (l xmlPathLocation) HyperlinkURI(path string, spans []domain.Span) string {
	if l.Line <= 0 {
		return domain.FileURI(path, "")
	}
	return fmt.Sprintf("%s:%d:%d", domain.FileURI(path, ""), l.Line, l.Column)
}
