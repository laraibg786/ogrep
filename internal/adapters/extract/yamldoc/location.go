package yamldoc

import (
	"fmt"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// yamlPathLocation implements domain.Location for a single value found
// while walking a YAML document. Path is a jq/yq-style path (e.g.
// ".a.b[2]", or ".document[1].a" for the second document of a multi-
// document file); Line and Column are the 1-based source position the
// value's token starts on (goccy's AST reports both directly, unlike
// the streaming JSON decoder jsondoc has to derive them from).
type yamlPathLocation struct {
	Path   string
	Line   int
	Column int
}

var _ domain.Location = yamlPathLocation{}

// Human implements domain.Location.
func (l yamlPathLocation) Human() string {
	return fmt.Sprintf("%s (line %d:%d)", l.Path, l.Line, l.Column)
}

// Fields implements domain.Location. The path key is "yamlpath", not
// "path" -- the output.JSON sink always sets a top-level "path" field to
// the file's own path (see internal/adapters/output/json.go) and then
// overlays Location.Fields() on top of that same record; a Location
// field also named "path" would silently clobber the file path with
// this jq/yq path instead of appearing alongside it. spans is unused:
// the path/line/column identify the value itself, which is the same
// regardless of which part of the "<path> = <value>" display text a
// match's span falls in.
func (l yamlPathLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"yamlpath": l.Path, "line": l.Line, "col": l.Column}
}

// HyperlinkURI implements domain.Location, linking straight to the
// value's real position in the source file -- not an offset derived
// from spans, which are byte ranges into the synthesized "<path> =
// <value>" display text, not the file itself, so they don't correspond
// to a real file position the way a text-format Span does. An editor
// opening this URI lands on the actual line/column of the value goccy
// parsed, which is what an editor jump is for.
func (l yamlPathLocation) HyperlinkURI(path string, spans []domain.Span) string {
	return fmt.Sprintf("%s:%d:%d", domain.FileURI(path, ""), l.Line, l.Column)
}
