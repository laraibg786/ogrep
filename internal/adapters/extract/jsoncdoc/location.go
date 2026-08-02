package jsoncdoc

import (
	"fmt"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// commentLocation implements domain.Location for a "//" or "/* */"
// comment found while parsing a JSONC document. Unlike jsonPathLocation,
// there is no jq path here -- comments aren't addressable JSON values,
// only Line/Column identify where one is.
type commentLocation struct {
	Line, Column int
}

var _ domain.Location = commentLocation{}

// Human implements domain.Location.
func (l commentLocation) Human() string {
	return fmt.Sprintf("line %d:%d (comment)", l.Line, l.Column)
}

// Fields implements domain.Location. "comment" is set to true so a
// --json consumer can distinguish a comment match from a value match
// without having to infer it from the absence of a "jsonpath" key.
// spans is unused: Line/Column already identify the comment itself,
// regardless of which part of its text a match's span falls in.
func (l commentLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"line": l.Line, "col": l.Column, "comment": true}
}

// HyperlinkURI implements domain.Location, linking to the comment's
// real position in the source file.
func (l commentLocation) HyperlinkURI(path string, spans []domain.Span) string {
	return fmt.Sprintf("%s:%d:%d", domain.FileURI(path, ""), l.Line, l.Column)
}
