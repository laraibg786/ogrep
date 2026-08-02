package htmldoc

import (
	"fmt"
	"strconv"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// htmlPathLocation implements domain.Location for a single text node
// found while walking an HTML document. Path is a synthesized,
// CSS-selector-like tag path from the document (or fragment) root down
// to the owning element, e.g. "html>body>div:nth-of-type(2)>p" -- see
// htmldoc.go's package doc comment for exactly how it's built. Line is
// the 1-based source line the node's own text begins on (not the line
// its enclosing start tag appeared on -- see htmldoc.go's "Position
// tracking" paragraph for why those two differ for typical
// pretty-printed markup, and for why counting newlines across
// golang.org/x/net/html.Tokenizer's Raw() calls is a reliable way to
// track it: Raw() hands back a token's exact, unmodified source bytes,
// before any entity-unescaping, and consecutive Raw() calls cover the
// source contiguously with no gaps).
//
// Line follows the same "path alone, no primary console position"
// contract every other line-oriented format in this repo (text, docx,
// etc.) uses: Human() renders the line number alone, the same
// grep-style "path:N" convention text.go's lineLocation uses, so the
// CSS tag path (still valuable structural information) moves to being
// supplementary data in Fields()/JSON under the "htmlpath" key -- never
// "path", which internal/adapters/output/json.go's WriteMatch already
// uses for the match's own file path, and would silently be overwritten
// by a same-named field. Line is 0 only if extraction genuinely
// couldn't determine a position (this shouldn't happen in practice,
// since Raw() covers the entire tokenized stream, but is handled
// defensively -- mirroring xmldoc's xmlPathLocation, whose Line is 0 for
// the analogous "position couldn't be determined" case).
type htmlPathLocation struct {
	Path string
	Line int
}

var _ domain.Location = htmlPathLocation{}

// Human implements domain.Location, rendering just the line number (so
// domain.LocationString produces the familiar grep "path:N" prefix,
// exactly like text.go's lineLocation.Human) when a line is known,
// falling back to the tag path alone in the defensive "position
// unavailable" case (see the type doc comment).
func (l htmlPathLocation) Human() string {
	if l.Line > 0 {
		return strconv.Itoa(l.Line)
	}
	return l.Path
}

// Fields implements domain.Location. "htmlpath" carries the
// CSS-selector-like tag path (see the type doc comment for why not
// "path"); "line" is the node's real source line. "col" is always 1,
// deliberately -- unlike text.go's lineLocation, whose Text is the raw
// source line itself (so a span's byte offset into it is a real source
// column), this package's Text is the *decoded, whitespace-collapsed*
// content of a text node (see htmldoc.go's writeCollapsed and its use of
// the tokenizer's "cooked" Text() accessor): an HTML entity anywhere
// before a match shortens the decoded text relative to the source bytes
// it came from, and collapsed runs of source whitespace do the same, so
// a span offset into Text has no reliable relationship to a source byte
// column. Reporting 1 keeps HyperlinkURI honest about what this package
// actually knows (a real line, not a real column) rather than deriving
// a column-looking number that would silently be wrong whenever the
// text node isn't a rare best case (no entities, no collapsed
// whitespace, node starts at column 1). spans is otherwise unused.
func (l htmlPathLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"htmlpath": l.Path, "line": l.Line, "col": 1}
}

// HyperlinkURI implements domain.Location. Once a line is known, the
// suffix is always ":line:1" -- see Fields's doc comment for why column
// 1 is the honest answer here, not a real per-match column; falls back
// to a bare file:// URI (no ":line:col" suffix at all) in the defensive
// case where even the line isn't known -- see the type doc comment.
func (l htmlPathLocation) HyperlinkURI(path string, spans []domain.Span) string {
	if l.Line <= 0 {
		return domain.FileURI(path, "")
	}
	return fmt.Sprintf("%s:%d:1", domain.FileURI(path, ""), l.Line)
}
