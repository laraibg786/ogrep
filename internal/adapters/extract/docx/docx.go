// Package docx implements a DocumentExtractor for WordprocessingML
// (.docx) files.
//
// A .docx file is a zip package. The parts we care about all share the
// same paragraph/run/text shape (w:p > w:r > w:t, in the
// "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
// namespace, referred to as "w:" below):
//
//   - word/document.xml: the main body (w:body). Body paragraphs are
//     numbered sequentially (1-based) in document order and reported
//     with a paragraphLocation whose Human is "Paragraph N". Paragraphs
//     nested inside a table cell (w:tbl > w:tr > w:tc > w:p) do NOT bump
//     this counter — they are addressed instead via Table/Row/Col and
//     reported with a cellLocation whose Human is
//     "Table T, Row R, Cell C". A cell with more than one paragraph
//     emits one unit PER non-blank paragraph, all sharing that same
//     cellLocation, rather than concatenating them into a single unit —
//     see "Line splitting" below for why.
//   - word/header*.xml, word/footer*.xml: one or more headers/footers
//     (w:hdr / w:ftr root elements). Every non-blank paragraph found in
//     each part is reported with a headerFooterLocation, sharing that
//     part's Human label ("Header 1", "Footer 2", ...) derived from the
//     numeric suffix in the part's file name (falling back to a
//     position counter if a part is oddly named). Table structure
//     inside headers/footers is intentionally NOT tracked (unlike the
//     body): headers/footers are rare to contain tables, and since there
//     is only one emission path here (one unit per w:p, whether or not
//     it happens to sit inside a table cell) there's no double-counting
//     risk that would otherwise justify the extra complexity.
//   - word/footnotes.xml, word/comments.xml: each w:footnote / w:comment
//     element's non-blank paragraphs are emitted one unit each, all
//     sharing a footnoteLocation / commentLocation for that element.
//     Human uses the element's own w:id attribute ("Footnote 3",
//     "Comment 2"), falling back to a running counter if the attribute
//     is missing or unparsable.
//
// Fidelity choices for run content: w:t text is concatenated verbatim;
// w:tab inserts a literal tab character. These are only honored while
// inside a w:r (run) element — w:tab can also appear inside w:pPr/w:tabs
// as a tab-STOP definition (unrelated to actual inserted text), so
// tracking "are we inside a run" is what keeps that case from being
// misread as inserted text.
//
// Line splitting: a paragraph containing a manual line break (w:br/w:cr)
// is split into multiple TextUnits at that point, one per segment,
// rather than joining them with an embedded "\n" into a single unit —
// and likewise for a table cell's multiple paragraphs, or a
// footnote/comment's multiple paragraphs. Both would otherwise produce a
// TextUnit spanning more than one visual line, which breaks the
// orchestrator's -A/-B/-C context-line and per-match semantics: those
// operate per TextUnit, so a multi-line unit would make "one line of
// context" actually mean "the entire rest of the cell/footnote," and a
// single regex match spanning the embedded "\n" would report one
// location for what a user perceives as two separate lines. Every split
// unit shares its paragraph's/cell's/footnote's Location — Word has no
// addressable location finer than that anyway (see each Location's
// HyperlinkURI), so there's no location fidelity lost by splitting.
//
// Blank filtering: body paragraphs (and their break-delimited segments)
// are emitted even when empty, mirroring the plain-text plugin's
// behavior of emitting every line so paragraph numbering stays
// predictable and dense. Table-cell, header/footer, footnote, and
// comment segments are only emitted when they contain non-blank text,
// since those are "extra" units (not part of the primary sequential
// addressing) and emitting blank ones (e.g. empty alignment-only
// paragraphs common in headers/footers) would just be noise.
//
// All parsing is streamed via encoding/xml's token API — no part is ever
// unmarshalled into a full in-memory DOM.
package docx

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// Extractor implements ports.DocumentExtractor for WordprocessingML
// (.docx) files.
type Extractor struct{}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "docx" }

// Extensions is an optional fast-path hint consumed by the registry; it
// is not authoritative — Sniff is what actually decides.
func (Extractor) Extensions() []string {
	return []string{".docx"}
}

// Sniff implements ports.DocumentExtractor. It opens the file as a zip
// archive and checks for the presence of word/document.xml, the one
// part every WordprocessingML package must have. Any failure to open
// the file as a zip (including an encrypted OOXML file, which is
// actually an OLE/CFB container rather than a zip) or the absence of
// that part simply results in false, never a panic.
func (Extractor) Sniff(path string, ra io.ReaderAt, size int64) bool {
	if size <= 0 {
		return false
	}

	// Defensive: archive/zip is not expected to panic on malformed
	// input, but Sniff is called from the registry's dispatch loop
	// across arbitrary (possibly adversarial/corrupt) files, so we
	// guard against any surprises rather than relying purely on the
	// stdlib's own robustness.
	defer func() { recover() }()

	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return false
	}
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			return true
		}
	}
	return false
}

// Extract implements ports.DocumentExtractor, streaming text units from
// the document body, then any headers/footers, then footnotes and
// comments (each present part only).
func (Extractor) Extract(ctx context.Context, ra io.ReaderAt, size int64) (<-chan domain.TextUnit, <-chan error) {
	units := make(chan domain.TextUnit)
	errc := make(chan error, 1)

	go func() {
		defer close(units)
		defer close(errc)
		// A panic here happens in this goroutine, not the caller's
		// (e.g. the orchestrator's), so only a recover() here can
		// catch it. Per the ports.DocumentExtractor contract, convert
		// it into a single error on errc instead of letting it crash
		// the process.
		defer func() {
			if r := recover(); r != nil {
				select {
				case errc <- fmt.Errorf("panic during docx extraction: %v", r):
				default:
				}
			}
		}()

		zr, err := zip.NewReader(ra, size)
		if err != nil {
			reportErr(errc, fmt.Errorf("opening docx zip: %w", err))
			return
		}

		filesByName := make(map[string]*zip.File, len(zr.File))
		for _, f := range zr.File {
			filesByName[f.Name] = f
		}

		send := func(u domain.TextUnit) bool {
			select {
			case units <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}

		var firstErr error
		noteErr := func(err error) {
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}

		if docFile, ok := filesByName["word/document.xml"]; ok {
			noteErr(extractDocumentBody(docFile, send))
		}

		for _, hf := range headerFooterParts(filesByName, "header") {
			noteErr(extractHeaderFooterPart(hf.file, hf.label, send))
		}
		for _, hf := range headerFooterParts(filesByName, "footer") {
			noteErr(extractHeaderFooterPart(hf.file, hf.label, send))
		}

		if ff, ok := filesByName["word/footnotes.xml"]; ok {
			noteErr(extractFootnoteLike(ff, "footnote", func(label string) domain.Location { return footnoteLocation{Label: label} }, "Footnote", send))
		}
		if cf, ok := filesByName["word/comments.xml"]; ok {
			noteErr(extractFootnoteLike(cf, "comment", func(label string) domain.Location { return commentLocation{Label: label} }, "Comment", send))
		}

		if firstErr != nil {
			reportErr(errc, firstErr)
		}
	}()

	return units, errc
}

// reportErr sends a single error on errc without blocking, matching the
// "send at most one error" contract.
func reportErr(errc chan<- error, err error) {
	select {
	case errc <- err:
	default:
	}
}

// headerFooterPart pairs a header/footer zip part with its rendered
// Location.Human label.
type headerFooterPart struct {
	file  *zip.File
	label string
}

// headerFooterParts finds every word/<kind><N>.xml part (kind is
// "header" or "footer") and returns them sorted by their numeric suffix,
// each paired with a "Header N"/"Footer N" style label. Parts whose name
// doesn't carry a parseable trailing number still get a label, using
// their position among the (name-sorted) parts of that kind instead.
func headerFooterParts(filesByName map[string]*zip.File, kind string) []headerFooterPart {
	prefix := "word/" + kind
	var names []string
	for name := range filesByName {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".xml") {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	type numbered struct {
		name string
		num  int
		ok   bool
	}
	entries := make([]numbered, len(names))
	for i, name := range names {
		numStr := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".xml")
		n, err := strconv.Atoi(numStr)
		entries[i] = numbered{name: name, num: n, ok: err == nil}
	}
	// Sort numerically where the suffix parses (so header2 sorts before
	// header10), falling back to the original name-lexical order for
	// anything unparseable.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].ok && entries[j].ok {
			return entries[i].num < entries[j].num
		}
		return false
	})

	titled := strings.ToUpper(kind[:1]) + kind[1:]

	out := make([]headerFooterPart, 0, len(entries))
	for i, e := range entries {
		num := i + 1
		if e.ok {
			num = e.num
		}
		out = append(out, headerFooterPart{
			file:  filesByName[e.name],
			label: fmt.Sprintf("%s %d", titled, num),
		})
	}
	return out
}
