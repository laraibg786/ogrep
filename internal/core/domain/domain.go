// Package domain contains the core value types shared across ogrep:
// the text units extracted from documents, the locations that describe
// where a piece of text came from, and the matches produced by searching
// that text. These types are the stable contract between the extraction
// plugins (docx/pptx/xlsx/text), the matcher adapters, and the output
// sinks — keep them free of any adapter-specific logic.
package domain

import (
	"net/url"
	"path/filepath"
)

// StdinPath is the grep/rg-style pseudo-path meaning "read standard
// input" -- either given explicitly as a PATH argument, or substituted
// implicitly when no PATH is given and stdin isn't a terminal. It's
// never a real filesystem path: the walker strips it out before doing
// any real path handling, and output sinks use it to recognize there's
// no backing file to build a hyperlink to.
const StdinPath = "-"

// FileURI builds a file:// URI for path, with fragment (if non-empty)
// appended as the URI fragment. Both are percent-encoded: POSIX
// filenames and sheet/shape names may legally contain spaces, '#',
// '%', tabs, or even newlines, none of which are valid unescaped in a
// URI, so this must never be built by plain string concatenation.
// Extractors call this from HyperlinkURI rather than each hand-rolling
// their own escaping.
//
// path is resolved to an absolute path first: callers (the walker, via
// Match.Path) pass whatever the user typed on the command line, which
// is relative whenever the search root itself was relative (the
// default root is "."), and a file:// URI with a relative Path is not
// well-formed -- a terminal or browser has no reliable base to resolve
// it against, so the hyperlink either does nothing or opens the wrong
// file. If Abs fails (only possible if os.Getwd fails), path is used
// as given rather than losing the link entirely.
func FileURI(path, fragment string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	u := &url.URL{Scheme: "file", Path: path}
	if fragment != "" {
		u.Fragment = fragment
	}
	return u.String()
}

// Location describes where within a file a TextUnit or Match came from.
// It is implemented by each extraction plugin (docx/pptx/xlsx/text), not
// by core: a docx paragraph location knows about paragraph numbers, an
// xlsx cell location knows about sheet/cell references, and so on. Core
// only ever calls Human, Fields, and HyperlinkURI, so adding a new
// format's location shape never requires changing domain, the
// orchestrator, or the output sinks.
//
// Fields and HyperlinkURI both take the Match's Spans: a Location is
// shared by every Match drawn from the same TextUnit (e.g. every match on
// one text line, or within one docx paragraph), but where exactly the
// match starts within that unit's Text varies per Match. A format that
// can turn a byte offset into a real column (currently just text, via
// spans[0].Start) uses spans to report and link to the precise match
// position rather than always the start of the unit; a format with
// nothing byte-addressable (docx, pptx, xlsx) simply ignores spans, same
// as it always could ignore path if it had no use for it.
type Location interface {
	// Human returns a pre-rendered, format-appropriate description (e.g.
	// `Slide 12`, `Sheet1!B45`, `Paragraph 88`, `line 42:9`) that output
	// sinks can print directly.
	Human() string

	// Fields returns format-specific structured data for machine-readable
	// output (e.g. JSON), keyed by field name (e.g. "slide", "shape").
	// spans is the triggering Match's Spans (see the interface doc for
	// why); pass nil when calling outside of a specific match (e.g. a
	// Location-only unit test) to get the unit-level default.
	Fields(spans []Span) map[string]any

	// HyperlinkURI returns the URI a terminal should open (via an OSC 8
	// hyperlink) when this location is clicked, given the file's path and
	// the triggering Match's Spans (see the interface doc). Each format
	// decides its own scheme (e.g. a text line links to
	// `file://path:line:col` using spans[0].Start, an xlsx cell to
	// `file://path#Sheet!Cell`). Return "" if the format has nothing
	// sensible to link to (e.g. a docx header/footer); output sinks then
	// print the location plain.
	HyperlinkURI(path string, spans []Span) string
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
//
// Text must never contain an embedded newline. The orchestrator's
// -A/-B/-C context-line handling and per-match Location reporting both
// operate at TextUnit granularity, so a unit spanning more than one
// visual line would make "one line of context" actually mean "the rest
// of this unit" and collapse what a reader sees as separate lines into
// one reported location. A format whose underlying structure can put
// more than one line in one logical place (e.g. a table cell with
// several paragraphs, or a paragraph with a manual line break) must
// split those into separate TextUnits sharing the same Location instead
// of joining them — see internal/adapters/extract/docx's package doc
// comment for a worked example.
type TextUnit struct {
	Location Location
	Text     string
}

// TextUnitChannelBuffer is the capacity every extractor should give the
// channel it streams TextUnits over. An unbuffered channel makes every
// single unit a full goroutine rendezvous between the extractor's
// producer goroutine and the orchestrator's consumer, which profiling
// showed costs a large share of total CPU time (mostly in the runtime's
// channel/select machinery) independent of the actual matching work.
// Buffering lets a producer get ahead of a consumer that's momentarily
// busy (e.g. running the matcher against a unit) instead of blocking on
// every send. 256 was chosen from a buffer-size sweep (0 to 2048) against
// a realistic-density search pattern: wall-clock time improves sharply up
// to roughly this size, then plateaus, then very large buffers (2048)
// start giving it back -- likely GC/allocator pressure from holding that
// many in-flight units' string data live at once outweighing any further
// reduction in channel-send contention.
const TextUnitChannelBuffer = 256

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
