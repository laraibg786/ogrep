package tomldoc

import (
	"fmt"
	"strconv"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// tomlPathLocation implements domain.Location for a single key/value
// found while walking a TOML document. Path is a jq/yq-style path
// string (e.g. ".a.b[2]" or `.["foo-bar"]`) that can be pasted directly
// into jq/yq -- kept for Fields()/JSON output, where it's valuable
// machine-readable information, but deliberately NOT part of Human()
// (see below). Line and Column are the real 1-based source position of
// the value, from github.com/pelletier/go-toml/v2/unstable's Parser
// (see tomldoc.go's package doc comment for how -- unlike the
// BurntSushi/toml decoder this package used to use, whose exported
// MetaData surface had no way to recover this).
type tomlPathLocation struct {
	Path   string
	Line   int
	Column int
}

var _ domain.Location = tomlPathLocation{}

// Human implements domain.Location as just the jq path, e.g. ".a.b[2]".
// The terminal's OSC 8 hyperlink (see HyperlinkURI) already encodes the
// real line:column for click-to-navigate, so Human() doesn't need to
// duplicate that -- its job is the most identifying plain-text label,
// which for a TOML value is its path, not a line number.
func (l tomlPathLocation) Human() string {
	return l.Path
}

// Fields implements domain.Location. The path key is "tomlpath", not
// "path" -- the output.JSON sink always sets a top-level "path" field
// to the file's own path (see internal/adapters/output/json.go) and
// then overlays Location.Fields() on top of that same record; a
// Location field also named "path" would silently clobber the file
// path with this jq/yq path instead of appearing alongside it. spans
// is unused: the path/line/column identify the value itself, which is
// the same regardless of which part of the real source line a match's
// span falls in.
func (l tomlPathLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"tomlpath": l.Path, "line": l.Line, "col": l.Column}
}

// HyperlinkURI implements domain.Location, linking straight to the
// value's real position in the source file. Falls back to a bare
// file:// URI when Line is 0 (position unavailable) rather than
// claiming line 1 is where the match is; see the Human doc comment for
// why this is only a defensive branch in practice.
func (l tomlPathLocation) HyperlinkURI(path string, spans []domain.Span) string {
	if l.Line <= 0 {
		return domain.FileURI(path, "")
	}
	return fmt.Sprintf("%s:%d:%d", domain.FileURI(path, ""), l.Line, l.Column)
}

// tomlCommentLocation implements domain.Location for a "#" comment
// found while parsing a TOML document, mirroring jsoncdoc's
// commentLocation. Unlike tomlPathLocation, there is no jq path here --
// comments aren't addressable TOML values, only Line/Column identify
// where one is.
type tomlCommentLocation struct {
	Line, Column int
}

var _ domain.Location = tomlCommentLocation{}

// Human implements domain.Location as just the bare line number --
// the same as a comment location in a plain text file (see text.go's
// own Location), and consistent with tomlPathLocation.Human() carrying
// no line/column decoration either; the real position is still fully
// available via HyperlinkURI and Fields().
func (l tomlCommentLocation) Human() string {
	return strconv.Itoa(l.Line)
}

// Fields implements domain.Location. "comment" is set to true so a
// --json consumer can distinguish a comment match from a value match
// without having to infer it from the absence of a "tomlpath" key.
func (l tomlCommentLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"line": l.Line, "col": l.Column, "comment": true}
}

// HyperlinkURI implements domain.Location, linking to the comment's
// real position in the source file.
func (l tomlCommentLocation) HyperlinkURI(path string, spans []domain.Span) string {
	return fmt.Sprintf("%s:%d:%d", domain.FileURI(path, ""), l.Line, l.Column)
}
