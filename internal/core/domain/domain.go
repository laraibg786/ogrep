// Package domain contains the core value types shared across ogrep:
// the text units extracted from documents, the locations that describe
// where a piece of text came from, and the matches produced by searching
// that text. These types are the stable contract between the extraction
// plugins (docx/pptx/xlsx/text), the matcher adapters, and the output
// sinks — keep them free of any adapter-specific logic.
package domain

// UnitKind identifies the kind of document construct a TextUnit was
// extracted from. Extractors should pick the most specific kind that
// applies; the text plugin uses UnitPlainLine.
type UnitKind int

const (
	UnitParagraph UnitKind = iota
	UnitTableCell
	UnitSlideShape
	UnitSlideNotes
	UnitSheetCell
	UnitHeaderFooter
	UnitFootnote
	UnitComment
	UnitPlainLine // for the text plugin
)

// String returns a short human-readable name for the unit kind, mostly
// useful for debugging/logging.
func (k UnitKind) String() string {
	switch k {
	case UnitParagraph:
		return "paragraph"
	case UnitTableCell:
		return "table-cell"
	case UnitSlideShape:
		return "slide-shape"
	case UnitSlideNotes:
		return "slide-notes"
	case UnitSheetCell:
		return "sheet-cell"
	case UnitHeaderFooter:
		return "header-footer"
	case UnitFootnote:
		return "footnote"
	case UnitComment:
		return "comment"
	case UnitPlainLine:
		return "plain-line"
	default:
		return "unknown"
	}
}

// Location describes where within a file a TextUnit or Match came from.
// It is implemented by each extraction plugin (docx/pptx/xlsx/text), not
// by core: a docx paragraph location knows about paragraph numbers, an
// xlsx cell location knows about sheet/cell references, and so on. Core
// only ever calls Human and Fields, so adding a new format's location
// shape never requires changing domain, the orchestrator, or the output
// sinks.
type Location interface {
	// Human returns a pre-rendered, format-appropriate description (e.g.
	// `Slide 12 (Shape "Title")`, `Sheet1!B45`, `Paragraph 88`, `line
	// 42`) that output sinks can print directly.
	Human() string

	// Fields returns format-specific structured data for machine-readable
	// output (e.g. JSON), keyed by field name (e.g. "slide", "shape").
	Fields() map[string]any
}

// Span is a byte-offset range [Start, End) into the Text of the TextUnit
// or Match it belongs to.
type Span struct {
	Start, End int
}

// TextUnit is a single chunk of extractable text from a document, along
// with the Location describing where it came from. Extractors stream
// TextUnits over a channel rather than building a full in-memory
// document tree.
type TextUnit struct {
	Kind     UnitKind
	Location Location
	Text     string
}

// Match is a single matched TextUnit, carrying the byte-offset Spans (in
// Text) that the matcher found. Path and Format are set by the
// orchestrator (which is the only layer that knows the file path and
// which extractor produced the unit); Location is carried over from the
// originating TextUnit unchanged.
type Match struct {
	Path     string
	Format   string
	Location Location
	Text     string
	Spans    []Span
}

// LocationString renders a "path:Human" location string, matching the
// conventional grep-style "file:location" prefix used in terminal output.
func LocationString(path string, loc Location) string {
	human := loc.Human()
	if human == "" {
		return path
	}
	return path + ":" + human
}

// SearchOptions carries all user-facing search configuration. Not every
// field is wired up by the v1 CLI (see cmd/ogrep), but the field is
// present so ports/adapters have a stable place to read it from once a
// later phase adds the corresponding flag.
type SearchOptions struct {
	IgnoreCase   bool
	FixedStrings bool
	WholeWord    bool
	InvertMatch  bool
	NoIgnore     bool

	MaxCount int

	ContextBefore int
	ContextAfter  int

	IncludeGlobs []string
	ExcludeGlobs []string
	Types        []string

	Threads int

	// FilesWithMatches and CountOnly change what the orchestrator writes
	// out for a matching file (-l/--files-with-matches and
	// -c/--count, respectively): when either is set, per-match
	// WriteMatch calls are skipped and only a single WriteFileSummary
	// call is made per matching file. They are mutually exclusive; the
	// CLI layer is responsible for rejecting both being set at once.
	FilesWithMatches bool
	CountOnly        bool
}
