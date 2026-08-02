package odt

import (
	"github.com/laraibg786/ogrep/internal/core/domain"
)

// paragraphLocation implements domain.Location for a top-level body
// paragraph (a text:p or text:h found directly under office:text, not
// inside a table cell). Paragraph is retained for Fields() /
// document-order purposes only; Human deliberately does not render it
// -- see Human's own doc comment, mirroring docx's identical rationale.
type paragraphLocation struct {
	Paragraph int
	// Heading is the nearest preceding text:h's own text (see
	// headingAccum in content.go), or "" if none has been seen yet.
	Heading string
}

// Human renders the nearest preceding heading only, or "" if the
// document has no heading before this point. A bare paragraph number
// ("Paragraph 56") isn't something a reader can act on the way a
// heading title is -- mirrors docx's paragraphLocation.Human, whose
// reasoning applies identically here since ODF text:h serves the exact
// same "navigable section boundary" role as a Word Heading style.
func (l paragraphLocation) Human() string { return l.Heading }

func (l paragraphLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"paragraph": l.Paragraph, "heading": l.Heading}
}

// HyperlinkURI opens the file with no location fragment: a heading's
// title text isn't reliably a real bookmark target, so it's omitted
// rather than looking clickable while silently doing nothing. spans is
// unused for the same reason.
func (l paragraphLocation) HyperlinkURI(path string, spans []domain.Span) string {
	return domain.FileURI(path, "")
}

// cellLocation implements domain.Location for a table cell. Table/Row/Col
// are retained for Fields() only; see paragraphLocation.Human for why
// Human renders the nearest heading instead of an address.
type cellLocation struct {
	Table, Row, Col int
	Heading         string
}

func (l cellLocation) Human() string { return l.Heading }

func (l cellLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"table": l.Table, "row": l.Row, "col": l.Col, "heading": l.Heading}
}

// HyperlinkURI opens the file with no location fragment; see
// paragraphLocation.HyperlinkURI for why.
func (l cellLocation) HyperlinkURI(path string, spans []domain.Span) string {
	return domain.FileURI(path, "")
}
