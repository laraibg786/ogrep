package jsonldoc

import (
	"fmt"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// lineLocation implements domain.Location for a single value found while
// flattening one line of a JSON Lines document. Path is the same
// jq-style path jsondoc would report for this value within its own
// line (e.g. ".a.b[2]"); Line is the value's real 1-based line number in
// the JSONL file (not the "line 1" jsondoc itself reports, since it only
// ever sees one line at a time); Column is jsondoc's own column, which
// is already accurate since it's relative to that line's content.
type lineLocation struct {
	Path   string
	Line   int
	Column int
}

var _ domain.Location = lineLocation{}

// Human implements domain.Location.
func (l lineLocation) Human() string {
	return fmt.Sprintf("%s (line %d:%d)", l.Path, l.Line, l.Column)
}

// Fields implements domain.Location. The path key is "jsonpath", matching
// jsondoc's own field name (a jsonl match is, structurally, just a json
// match with a corrected line number) -- not "path", which the
// output.JSON sink always sets at the top level to the file's own path;
// see jsondoc.jsonPathLocation.Fields for why that collision matters.
// spans is unused for the same reason jsondoc's own Location ignores it:
// a match's Span is an offset into the synthesized "<path> = <value>"
// display text, not the file, so it doesn't correspond to a real file
// position.
func (l lineLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"jsonpath": l.Path, "line": l.Line, "col": l.Column}
}

// HyperlinkURI implements domain.Location, linking to the value's real
// line/column in the source file.
func (l lineLocation) HyperlinkURI(path string, spans []domain.Span) string {
	return fmt.Sprintf("%s:%d:%d", domain.FileURI(path, ""), l.Line, l.Column)
}
