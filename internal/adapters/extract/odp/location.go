package odp

import (
	"fmt"
	"strconv"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// shapeLocation implements domain.Location for text found within a
// named shape (draw:frame) on a slide, mirroring pptx's shapeLocation
// exactly.
type shapeLocation struct {
	Slide int
	Shape string
}

// Human deliberately omits the shape name; see pptx.shapeLocation.Human
// for the identical rationale (it's useful machine-readable metadata,
// carried in Fields, but clutters the console line for every match).
func (l shapeLocation) Human() string {
	return fmt.Sprintf("Slide %d", l.Slide)
}

func (l shapeLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"slide": l.Slide, "shape": l.Shape}
}

// HyperlinkURI uses a bare slide-number fragment, matching pptx's
// identical choice (the convention presentation viewers honor for
// jumping to a specific slide in a local file).
func (l shapeLocation) HyperlinkURI(path string, spans []domain.Span) string {
	return domain.FileURI(path, strconv.Itoa(l.Slide))
}

// notesLocation implements domain.Location for text found in a slide's
// speaker notes (presentation:notes).
type notesLocation struct {
	Slide int
}

func (l notesLocation) Human() string {
	return fmt.Sprintf("Slide %d (Notes)", l.Slide)
}

func (l notesLocation) Fields(spans []domain.Span) map[string]any {
	return map[string]any{"slide": l.Slide}
}

// HyperlinkURI returns "": ODF has no addressable location for speaker
// notes, same as pptx.
func (l notesLocation) HyperlinkURI(path string, spans []domain.Span) string { return "" }
