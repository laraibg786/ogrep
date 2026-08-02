package jsondoc

import (
	"fmt"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// jsonPathLocation implements domain.Location for a single value found
// while walking a JSON document. Path is a jq-style path string (e.g.
// ".a.b[2]" or `.["foo-bar"]`) that can be pasted directly into jq. Line
// and Column are the 1-based source position of the end of the value's
// token (see posTracker in jsondoc.go for why "end" rather than "start",
// and why it's still always on the correct line).
type jsonPathLocation struct {
	Path   string
	Line   int
	Column int
}

var _ domain.Location = jsonPathLocation{}

// Human implements domain.Location, rendering the jq-style path with a
// "(line N:M)" suffix when a position is known (Line is 0 for a value
// whose position couldn't be determined, which shouldn't happen in
// practice but is handled defensively).
func (l jsonPathLocation) Human() string {
	if l.Line > 0 {
		return fmt.Sprintf("%s (line %d:%d)", l.Path, l.Line, l.Column)
	}
	return l.Path
}

// Fields implements domain.Location. The path key is "jsonpath", not
// "path" -- the output.JSON sink always sets a top-level "path" field to
// the file's own path (see internal/adapters/output/json.go) and then
// overlays Location.Fields() on top of that same record; a Location
// field also named "path" would silently clobber the file path with
// this jq path instead of appearing alongside it. spans is unused: the
// path/line/column identify the value itself, which is the same
// regardless of which part of the "<path> = <value>" display text a
// match's span falls in.
func (l jsonPathLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"jsonpath": l.Path, "line": l.Line, "col": l.Column}
}

// HyperlinkURI implements domain.Location, linking straight to the
// value's real position in the source file -- not an offset derived
// from spans, which are byte ranges into the synthesized "<path> =
// <value>" display text, not the file itself, so they don't correspond
// to a real file position the way a text-format Span does. Falls back to
// a bare file:// URI when Line is 0 (position unavailable) rather than
// claiming line 1 is where the match is.
func (l jsonPathLocation) HyperlinkURI(path string, spans []domain.Span) string {
	if l.Line <= 0 {
		return domain.FileURI(path, "")
	}
	return fmt.Sprintf("%s:%d:%d", domain.FileURI(path, ""), l.Line, l.Column)
}
