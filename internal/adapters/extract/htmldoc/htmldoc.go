// Package htmldoc implements a ports.DocumentExtractor for HTML
// documents and fragments.
//
// Real-world HTML is frequently not well-formed XML: unclosed tags,
// implicit <html>/<head>/<body>, unescaped '&', mismatched close tags --
// all things a browser renders fine but a strict XML parser (as xmldoc
// uses, via encoding/xml) would reject outright. So this package parses
// with golang.org/x/net/html's Tokenizer, the HTML5-compliant tokenizer
// half of that module (not its tree-building Parse, and not
// encoding/xml): it never errors on malformed markup the way an XML
// decoder does, which is the whole point of using it here. It also
// never builds a full DOM -- memory is bounded by nesting depth (see the
// hard depth cap below) plus whichever single element's direct text is
// currently being accumulated, the same streaming shape xmldoc uses.
// That bound is per-element, not per-document: the underlying Tokenizer
// buffers a whole token before returning it, so one pathologically
// large single token (an enormous inline <script>, or the tail of a
// <plaintext> element, which has no end tag and so reads as one token
// to end of document) is itself only bounded by that token's own size,
// not by this package's depth cap.
//
// Extraction flattens the tag tree into one domain.TextUnit per
// meaningful chunk of an element's own direct (non-descendant) text,
// located at that element's synthesized, CSS-selector-like tag path
// (e.g. "html>body>div:nth-of-type(2)>p") plus the real source line that
// chunk's text begins on -- see location.go for both, and Extract's
// per-token position bookkeeping below for how the line is tracked
// using golang.org/x/net/html.Tokenizer's Raw(), which (per that
// method's own doc comment) hands back the token's exact, unmodified
// source bytes -- unlike TagName()/Text(), which return "cooked",
// entity-unescaped output. As with xmldoc's XPath, the "[N]"-equivalent
// ":nth-of-type(N)" predicate is always rendered, even for a singleton
// child, since a single left-to-right streaming pass can't know in
// advance whether more same-tag siblings will follow. Unlike XML, HTML
// has no well-formedness guarantee of exactly one top-level element (a
// bare fragment can have
// several), so top-level siblings are indexed the same way, using their
// own counter rather than xmldoc's "no index on the root" special case.
//
// rawTextElements' contents are never extracted: this is every element
// for which golang.org/x/net/html's tokenizer itself enters raw-text (or
// PLAINTEXT) mode -- script, style, iframe, noembed, noframes, noscript,
// plaintext, xmp -- plus, as a package-specific policy choice on top of
// that, the same script/style exclusion nobody searching a rendered
// page's content wants JS/CSS source matched. Getting this list right
// matters beyond script/style: the tokenizer treats ALL of these as one
// opaque raw-text token spanning start tag to end tag (or, for
// plaintext, to end of document -- it has no end tag at all), so any
// omission doesn't just search-pollute with a bit of markup-ish text, it
// swallows the *entire* raw span -- including any nested-looking tags
// inside it, verbatim -- as one lump. title and textarea are
// deliberately NOT in this set even though the tokenizer also treats
// their content as a single raw(-ish, RCDATA) token: unlike the others,
// their content is meaningful page text (a document's title, a form's
// prefilled value), so it's extracted like any other element's direct
// text.
//
// <br> is treated as a manual line break within whatever element's text
// is currently being accumulated (mirroring docx's treatment of w:br --
// see that package's doc comment), inserting a '\n' rather than being
// skipped or given its own unit. All other void elements (area, base,
// col, embed, hr, img, input, link, meta, param, source, track, wbr --
// the HTML5 list of elements that can never have an end tag or
// children) carry no extractable text and are otherwise ignored
// entirely: no unit, no tag-path frame, no effect on sibling counters.
// The self-closing flag on a *tag token* (a trailing "/>") is NOT the
// same thing as being a void element -- HTML5 ignores that flag on
// ordinary elements ("<div/>" means exactly "<div>", with a normal
// closing tag still expected later), so a self-closing token for a
// non-void tag is handled exactly like a normal start tag: it pushes a
// frame and counts as a sibling. It will simply never see a matching
// end tag, which the existing end-of-document flush (see Extract)
// already handles.
//
// A literal newline (or any other run of ASCII whitespace: space, tab,
// LF, CR, FF) inside an ordinary text node is collapsible source
// formatting, not a real line break -- a browser renders it as a single
// space, and a phrase wrapped across a source line must still be
// searchable as one phrase. So normal (non-<pre>/<textarea>) text is
// collapsed to single spaces before being accumulated, and the only
// place a real break is ever recognized within an element's accumulated
// text is a <br> (a start tag, handled directly in Extract) or -- inside
// <pre>/<textarea> -- a preserved literal newline byte. Inside <pre> and
// <textarea>, source whitespace is significant and is preserved verbatim
// instead.
//
// A text node can itself span multiple visual lines (a <br synthesized
// break, or -- inside <pre>/<textarea> -- a preserved literal newline),
// and a domain.TextUnit must never contain one (see domain.TextUnit's
// doc comment) -- so an element's fully-accumulated direct text is
// closed off into a separate segment (frame.finishLine) at each such
// real break, each becoming its own TextUnit sharing that element's tag
// path but -- since a real break, by definition, moves to a new source
// line -- carrying its own, individually-tracked source line, the same
// convention docx uses for a paragraph containing a manual break (docx
// has no source-line concept to individually track, HTML does).
//
// Malformed nesting (e.g. an unclosed <p>) is tolerated structurally,
// not by implementing the HTML5 tree-construction spec's full
// implied-end-tag rules: an end tag searches the currently-open stack
// from the top for the nearest element with the same tag name and
// closes everything above and including it, rather than requiring an
// exact match at the top (a stray end tag with no open match is simply
// ignored, as a browser would). This correctly recovers
// <div><p>Hello</div> (the </div> implicitly closes the still-open <p>
// first). On top of that, a small static table (impliedCloses) covers
// the common real-world cases where a *start* tag -- not just an end
// tag -- should trigger the same "close and flush" search: consecutive
// <li>, <td>/<th>, <tr>, <option>/<optgroup>, <dt>/<dd>, and
// <thead>/<tbody>/<tfoot> commonly appear unclosed in real markup and
// minifier output (e.g. "<tr><td>a<td>b<td>c"), and without this they'd
// nest instead of forming siblings -- both the wrong shape (td>td>td
// instead of three sibling td's) and, since path strings grow with
// nesting depth, a real memory/CPU liability on a large table. This
// still isn't full HTML5 fidelity (e.g. an unclosed <p> immediately
// followed by a sibling <p> is not in the table, and is instead nested
// one inside the other), and a hard cap (maxStackDepth) on how deep the
// open-element stack can grow is kept as a defensive backstop -- not the
// primary fix -- so that a case missing from the table, or genuinely
// adversarial input, still can't cause unbounded growth. Any element(s)
// still open at end-of-document are flushed in the same innermost-first
// order a matching close tag would have used.
//
// Position tracking: Extract keeps a running 1-based source line
// counter, advanced after every z.Next() by counting '\n' bytes in that
// token's z.Raw() -- which, per that method's doc comment
// ("return z.buf[z.raw.start:z.raw.end]"), hands back the token's exact
// source bytes, before any entity-unescaping, and does so for every
// token type with no gaps: each token's raw span starts exactly where
// the previous one ended. That makes counting newlines across
// accumulated Raw() calls a reliable running line count, not the
// "fragile" reconstruction an earlier version of this comment (wrongly)
// warned about. Raw() must be consumed (here, just scanned for '\n')
// immediately after Next() returns and before calling any "cooked"
// accessor (TagName(), Text()) on that same token: those decode the
// token in place in the tokenizer's own internal buffer, which is the
// same backing array Raw() sliced into, so a Raw() slice read *after*
// such a call is not reliable -- only reading it first is. The location
// reported for a given chunk of text is the line its own first real
// content begins on (skipping leading collapsible whitespace, for an
// ordinary element -- see frame's doc comment), not the line the
// element's start tag appeared on: for typical pretty-printed markup
// ("<p>\n  Hello\n</p>") those differ by a line, and the text's own line
// is what a user searching for "Hello" actually expects to land on.
//
// The package self-registers into registry.Default on package init, so
// importing this package for its side effect (see
// internal/adapters/extract/all) is enough to make it available.
package htmldoc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// sniffPeekWindow bounds how many leading bytes Sniff inspects for a
// cheap "does this look like HTML" check.
const sniffPeekWindow = 4096

// maxStackDepth is a defensive backstop on how deep the open-element
// stack (and therefore path strings, which grow with nesting) can ever
// grow, regardless of whether impliedCloses covers the input's actual
// shape. 1000 is comfortably deeper than any realistic hand-written or
// generated document's genuine nesting, while still bounding worst-case
// memory for adversarial or pathological input to a small, fixed
// multiple of that depth -- see the package doc comment.
const maxStackDepth = 1000

// voidElements is the fixed HTML5 list of elements that can never have
// an end tag or children (https://html.spec.whatwg.org/#void-elements).
// Encountering one of these as a start tag never pushes a tag-path
// frame: there's no direct text to ever accumulate for it, and (br
// aside, handled specially in Extract) no other effect on extraction.
// This is the only list consulted to decide "no content, no frame" --
// notably NOT the self-closing flag on a tag token, which HTML5 ignores
// for any element not in this list (see the package doc comment).
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

// rawTextElements marks elements whose content must never be extracted
// as searchable text (see the package doc comment). This mirrors
// exactly which elements golang.org/x/net/html's tokenizer itself
// treats as raw text (or PLAINTEXT) -- i.e. as one opaque token that is
// never scanned for nested tags -- with title and textarea deliberately
// excluded since their content is meaningful, searchable page text.
var rawTextElements = map[string]bool{
	"script": true, "style": true, "iframe": true, "noembed": true,
	"noframes": true, "noscript": true, "plaintext": true, "xmp": true,
}

// verbatimElements marks elements whose direct text must be accumulated
// exactly as written, without collapsing runs of source whitespace to a
// single space (see the package doc comment).
var verbatimElements = map[string]bool{"pre": true, "textarea": true}

// impliedCloses is the small static table of HTML5 "implied end tag"
// cases this package implements: encountering the map's key as a start
// tag implicitly closes the nearest still-open element (searching the
// stack from the top down) whose tag is in the corresponding value
// list, exactly as if a matching end tag had appeared first. Closing
// that frame also closes -- and flushes -- everything above it on the
// stack, which is what correctly handles e.g. a new <tr> implicitly
// closing a still-open <td> from the previous row along with that row's
// <tr> itself, without needing td/th listed under "tr" too. See the
// package doc comment for why this table, rather than full HTML5
// tree-construction, is the chosen fix.
var impliedCloses = map[string][]string{
	"li":       {"li"},
	"td":       {"td", "th"},
	"th":       {"td", "th"},
	"tr":       {"tr"},
	"option":   {"option"},
	"optgroup": {"optgroup"},
	"dt":       {"dt", "dd"},
	"dd":       {"dt", "dd"},
	"thead":    {"thead", "tbody", "tfoot"},
	"tbody":    {"thead", "tbody", "tfoot"},
	"tfoot":    {"thead", "tbody", "tfoot"},
}

// newlineBytes is a reusable single-byte "\n" slice for bytes.Count/
// bytes.Split calls in Extract's per-token position bookkeeping, avoiding
// a fresh allocation on every token.
var newlineBytes = []byte{'\n'}

func containsTag(list []string, tag string) bool {
	for _, t := range list {
		if t == tag {
			return true
		}
	}
	return false
}

// Extractor implements ports.DocumentExtractor for HTML documents.
type Extractor struct{}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "html" }

// Extensions is an optional fast-path hint consumed by the registry;
// Sniff (content inspection) is what actually decides.
func (Extractor) Extensions() []string { return []string{".html", ".htm", ".xhtml"} }

// firstRealTag returns the prefix of b starting at the first thing that
// looks like a real tag/doctype, after skipping leading ASCII
// whitespace and any leading HTML/XML comments ("<!-- ... -->"). It
// returns nil if b is exhausted, or ends in whitespace/an unterminated
// comment, before any such tag is found within the window -- an
// ambiguous case Sniff treats as "no signal" rather than guessing.
func firstRealTag(b []byte) []byte {
	for {
		b = bytes.TrimLeft(b, " \t\r\n\f")
		if len(b) == 0 {
			return nil
		}
		if bytes.HasPrefix(b, []byte("<!--")) {
			end := bytes.Index(b, []byte("-->"))
			if end == -1 {
				return nil
			}
			b = b[end+3:]
			continue
		}
		return b
	}
}

// Sniff implements ports.DocumentExtractor. It does a cheap prefix
// check for something that looks like a tag, comment, or doctype --
// deliberately not a full parse: golang.org/x/net/html's Tokenizer
// almost never reports an error for malformed markup (that leniency is
// the entire reason this package exists), so unlike xmldoc's "confirm
// the first token actually decodes" check, there is no cheap stronger
// signal available here to lean on. A file that doesn't start with '<'
// (allowing for a leading BOM/whitespace) is declined and falls through
// to the text extractor instead -- most plausible for a plain-text file
// that merely happens to be named .html/.htm/.xhtml.
//
// A bare "starts with '<' then a letter" match is deliberately treated
// as too weak a signal on its own: from this cheap, prefix-only vantage
// point it's indistinguishable from a truncated/malformed XML file
// (e.g. "<root" with the closing '>' never written) -- and unlike
// xmldoc, there's no stronger "does the first token actually decode"
// check available to tell them apart, since the whole point of the
// HTML tokenizer is that it doesn't error on things like that. So a
// bare tag-like prefix additionally requires either the file's own
// .html/.htm/.xhtml extension (the common case) or an unambiguous HTML
// signal in the peeked window.
//
// That content-based signal is deliberately narrow, to avoid claiming
// files that merely mention "<html" somewhere -- an XSLT stylesheet
// whose output happens to be HTML, an XML comment mentioning <html>, or
// an XML element named e.g. <htmlBody>, none of which are actually
// HTML. Content starting with "<?xml" is rejected outright (that's
// unambiguously not HTML). Otherwise, the doctype/<html> signal must be
// the first real tag in the content (skipping leading
// whitespace/BOM/comments -- see firstRealTag), and if it's "<html"
// specifically, that must be followed by a tag-terminating character
// (space, '>', '/', tab, newline, form feed), not just contained as a
// raw substring -- so "<htmlBody>" as the first tag correctly does not
// match. Without one of those two signals, the content is left for
// whichever other registered extractor (typically the XML plugin, or
// ultimately text) actually claims it.
func (Extractor) Sniff(path string, ra io.ReaderAt, size int64) (ok bool) {
	// Sniff must never panic to the caller.
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()

	if size <= 0 {
		return false
	}

	n := size
	if n > sniffPeekWindow {
		n = sniffPeekWindow
	}
	buf := make([]byte, n)
	nRead, err := ra.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return false
	}
	buf = buf[:nRead]

	trimmed := bytes.TrimPrefix(buf, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
	trimmed = bytes.TrimLeft(trimmed, " \t\r\n")
	if len(trimmed) < 2 || trimmed[0] != '<' {
		return false
	}
	b := trimmed[1]
	switch {
	case b == '!': // <!DOCTYPE ...> or <!-- comment -->
	case b == '/': // a stray end tag as the very first thing: still markup
	case (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z'):
	default:
		return false
	}

	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".html" || ext == ".htm" || ext == ".xhtml" {
		return true
	}

	if bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<?xml")) {
		return false
	}

	tag := firstRealTag(trimmed)
	if tag == nil {
		return false
	}
	lower := bytes.ToLower(tag)
	if bytes.HasPrefix(lower, []byte("<!doctype html")) {
		return true
	}
	const htmlPrefix = "<html"
	if bytes.HasPrefix(lower, []byte(htmlPrefix)) {
		if len(lower) == len(htmlPrefix) {
			return false // truncated within the window: ambiguous
		}
		switch lower[len(htmlPrefix)] {
		case ' ', '\t', '\n', '\r', '\f', '>', '/':
			return true
		}
	}
	return false
}

// flushLine is one already-completed (real-break-terminated) segment of
// a frame's accumulated direct text, paired with the source line its
// own first real content began on -- see frame.finishLine and the
// position-tracking discussion on frame's doc comment.
type flushLine struct {
	text string
	line int
}

// frame tracks one currently-open element while streaming: its own
// synthesized tag path, a per-child-tag-name counter for the next
// child's ":nth-of-type(N)" index (lazily allocated -- see incCount --
// since most frames are leaves with no children at all), and its own
// direct text accumulated so far (a child's text tokens are never
// written here, since by the time a child is open it has pushed its own
// frame -- see Extract). raw is true for an element whose text is never
// accumulated at all (see rawTextElements). verbatim is true for an
// element whose text is accumulated exactly as written, without
// whitespace collapsing (see verbatimElements).
//
// Position tracking: text is the still-open segment since the last real
// break (a <br>, or -- inside a verbatim element -- a preserved literal
// newline); lines holds every segment already closed by such a break.
// segStartLine is the 1-based source line on which text's own first real
// (non-whitespace, for a collapsible element; any byte, for a verbatim
// one) content began, or 0 if text has no real content yet -- see
// Extract's per-token position bookkeeping for how it's set. This is
// deliberately the line the *text* starts on, not the line the
// element's own start tag appeared on: see htmldoc.go's package doc
// comment and location.go for why that's the more useful, grep-parity
// choice once a position is actually available.
type frame struct {
	tag      string
	path     string
	counts   map[string]int
	text     strings.Builder
	lines    []flushLine
	raw      bool
	verbatim bool
	// endsWS is true when text's most recently written byte was a
	// collapsed whitespace space -- carried across separate
	// writeCollapsed calls on the same frame (e.g. a frame's direct
	// text arriving in more than one text token, interrupted by a
	// child element like <b>) so trailing whitespace from one call and
	// leading whitespace from the next collapse into a single space
	// instead of two adjacent ones.
	endsWS       bool
	segStartLine int
}

// finishLine closes f's currently-open segment (the real break itself --
// a <br>, or a verbatim element's own literal newline -- is never part
// of either resulting segment's text, mirroring the old split-on-'\n'
// behavior), appending it to f.lines and resetting f.text/f.endsWS/
// f.segStartLine ready for whatever comes next.
func (f *frame) finishLine() {
	f.lines = append(f.lines, flushLine{text: f.text.String(), line: f.segStartLine})
	f.text.Reset()
	f.endsWS = false
	f.segStartLine = 0
}

// incCount increments and returns *counts[tag], allocating the map on
// first use -- most frames are leaves and never need one at all.
func incCount(counts *map[string]int, tag string) int {
	if *counts == nil {
		*counts = make(map[string]int)
	}
	(*counts)[tag]++
	return (*counts)[tag]
}

// pathFor synthesizes a child element's tag path, incrementing counts
// (the owning scope's per-tag-name counter -- either a parent frame's,
// or the top-level rootCounts when there is no open parent) and always
// rendering the resulting ":nth-of-type(N)" predicate, even for a
// singleton child -- see the package doc comment for why a single
// streaming pass can't decide to omit it. Path segments are built
// manually into a byte slice (strconv.AppendInt plus direct
// concatenation) rather than via fmt.Sprintf, avoiding Sprintf's
// reflection/interface-boxing overhead on what is a hot path (this
// function runs once per element in the document).
func pathFor(parentPath string, counts *map[string]int, tag string) string {
	n := incCount(counts, tag)
	buf := make([]byte, 0, len(parentPath)+len(tag)+16)
	if parentPath != "" {
		buf = append(buf, parentPath...)
		buf = append(buf, '>')
	}
	buf = append(buf, tag...)
	buf = append(buf, ":nth-of-type("...)
	buf = strconv.AppendInt(buf, int64(n), 10)
	buf = append(buf, ')')
	return string(buf)
}

// isASCIIWhitespace reports whether c is HTML5 "ASCII whitespace"
// (https://infra.spec.whatwg.org/#ascii-whitespace): space, tab, LF,
// FF, or CR. This is deliberately narrower than unicode.IsSpace --
// scanning byte-by-byte for exactly these values is also safe on UTF-8
// text, since none of these byte values ever appears as part of a
// multi-byte UTF-8 sequence (continuation bytes are always >= 0x80).
func isASCIIWhitespace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\f', '\r':
		return true
	}
	return false
}

// indexFirstNonASCIIWhitespace returns the index of b's first byte that
// is not HTML5 ASCII whitespace, or -1 if b is empty or entirely
// whitespace. Used to find where a collapsible text token's real
// content -- as opposed to source indentation/line-wrap whitespace --
// actually begins, for position tracking (see frame's doc comment and
// Extract's per-token bookkeeping).
func indexFirstNonASCIIWhitespace(b []byte) int {
	for i, c := range b {
		if !isASCIIWhitespace(c) {
			return i
		}
	}
	return -1
}

// writeCollapsed appends text to b with every run of ASCII whitespace
// (including an embedded literal newline -- source line-wrapping, not a
// real line break in HTML) collapsed to a single space, mirroring how a
// browser renders such a run. Leading/trailing whitespace within text is
// preserved as a single space rather than trimmed, since a text node's
// own leading/trailing space can be significant relative to an adjacent
// inline element (see TestExtractTextSplitAcrossSiblingInlineTags).
//
// endsWS carries whether b's own most recently written byte was already
// a collapsed whitespace space, across separate calls writing into the
// same b (a frame's direct text can arrive in more than one text token
// if a child element interrupts it -- e.g. "Hello <b>World</b> Goodbye"
// calls this twice for the enclosing element, once for "Hello " and once
// for " Goodbye"). Without carrying that state, each call's own leading
// whitespace would independently collapse to its own space, producing
// two adjacent spaces at the boundary instead of one. writeCollapsed
// returns the updated state for the caller to store back.
//
// Because this always collapses any '\n' down to a plain space, the
// only literal '\n' bytes that would ever need to be recognized as a
// real break for a collapsible element come from <br>, handled directly
// in Extract by closing off a frame's current segment (frame.finishLine)
// rather than by writing a literal '\n' through this function at all.
func writeCollapsed(b *strings.Builder, text []byte, endsWS bool) bool {
	for _, c := range text {
		if isASCIIWhitespace(c) {
			if !endsWS {
				b.WriteByte(' ')
				endsWS = true
			}
			continue
		}
		endsWS = false
		b.WriteByte(c)
	}
	return endsWS
}

// Extract implements ports.DocumentExtractor.
func (Extractor) Extract(ctx context.Context, ra io.ReaderAt, size int64) (<-chan domain.TextUnit, <-chan error) {
	units := make(chan domain.TextUnit, domain.TextUnitChannelBuffer)
	errc := make(chan error, 1)

	go func() {
		defer close(units)
		defer close(errc)
		// This goroutine's own panics cannot be caught by a recover()
		// anywhere else (e.g. the orchestrator's per-file worker) --
		// only a recover deferred here, in the same goroutine, can. Per
		// the ports.DocumentExtractor contract, convert any panic into
		// a single error on errc instead of crashing the process. See
		// internal/adapters/extract/text/text.go for the pattern this
		// mirrors.
		defer func() {
			if r := recover(); r != nil {
				select {
				case errc <- fmt.Errorf("panic during html extraction: %v", r):
				default:
				}
			}
		}()

		sr := io.NewSectionReader(ra, 0, size)
		z := html.NewTokenizer(sr)

		emit := func(path, text string, line int) error {
			select {
			case units <- domain.TextUnit{Location: htmlPathLocation{Path: path, Line: line}, Text: text}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		// flush emits one unit per non-blank completed segment of f's
		// accumulated direct text (see frame's doc comment on segments
		// and finishLine, and the package doc comment on why each
		// segment -- not the whole element -- must never contain an
		// embedded newline). f's still-open trailing segment is closed
		// off via finishLine first, so this always sees every segment
		// including the last. A raw frame's text was never accumulated
		// in the first place, so this is a no-op for it.
		flush := func(f *frame) error {
			if f.raw {
				return nil
			}
			f.finishLine()
			for _, ln := range f.lines {
				text := strings.TrimSpace(ln.text)
				if text == "" {
					continue
				}
				if err := emit(f.path, text, ln.line); err != nil {
					return err
				}
			}
			return nil
		}

		var stack []*frame
		var rootCounts map[string]int
		// suppressedDepth counts start tags dropped by the maxStackDepth
		// backstop below (never pushed onto stack at all). Without this,
		// an end tag for one of those untracked elements would fall
		// through to the normal stack search and -- since it can't find
		// itself there -- might instead match and prematurely close an
		// unrelated, legitimately-open ancestor frame that happens to
		// share the same tag name. Consuming one suppressed count per
		// end tag while any remain (rather than searching the real
		// stack) avoids that misattribution; which specific suppressed
		// element a given end tag "belongs to" is unknowable once
		// depth tracking has been abandoned, so this is a best-effort
		// match, not a precise one -- acceptable since maxStackDepth is
		// already a last-resort backstop for pathological nesting, not
		// a case this package tries to render with full fidelity.
		var suppressedDepth int

		// lineNo is a running 1-based source line counter, advanced by
		// counting '\n' bytes in each token's z.Raw() -- see the package
		// doc comment's "Position tracking" paragraph for why that's a
		// reliable running count (Raw() returns the token's exact source
		// bytes, before any entity-unescaping, and consecutive Raw()
		// slices cover the source contiguously with no gaps).
		lineNo := 1

		for {
			if err := ctx.Err(); err != nil {
				return
			}
			tt := z.Next()
			if tt == html.ErrorToken {
				if err := z.Err(); err != nil && err != io.EOF {
					select {
					case errc <- err:
					default:
					}
				}
				break
			}

			// Position bookkeeping must happen here, immediately after
			// Next() and before any "cooked" accessor (TagName(),
			// Text()) is called below on this same token: those decode
			// the token in place in the tokenizer's own internal
			// buffer, the same backing array Raw() sliced into, so a
			// Raw() slice is only reliable if read (here, just scanned
			// for '\n') before such a call -- never after. tokenStart is
			// this token's own first byte's line; for a TextToken,
			// contentLine is the line the first real (non-whitespace)
			// byte of a collapsible token's raw source falls on (0 if
			// none -- an all-whitespace token), used below to find where
			// a chunk of text actually starts rather than where its
			// enclosing tag did (see the package doc comment).
			raw := z.Raw()
			tokenStart := lineNo
			var contentLine int
			if tt == html.TextToken {
				if idx := indexFirstNonASCIIWhitespace(raw); idx >= 0 {
					contentLine = tokenStart + bytes.Count(raw[:idx], newlineBytes)
				}
			}
			lineNo += bytes.Count(raw, newlineBytes)

			switch tt {
			case html.StartTagToken, html.SelfClosingTagToken:
				name, _ := z.TagName()
				tag := string(name)

				if tag == "br" {
					if len(stack) > 0 {
						top := stack[len(stack)-1]
						if !top.raw {
							top.finishLine()
						}
					}
					continue
				}
				if voidElements[tag] {
					// No direct text is ever possible for a void
					// element, so it gets no frame, no unit, and no
					// effect on sibling counters. Unlike a void
					// element, an ordinary element written with a
					// trailing '/' (SelfClosingTagToken) still falls
					// through below and is treated exactly like a
					// normal start tag -- HTML5 ignores that flag on
					// non-void elements (see the package doc comment).
					continue
				}

				// Implied end tags for common unclosed-shorthand
				// markup (see impliedCloses's doc comment).
				if targets := impliedCloses[tag]; targets != nil {
					idx := -1
					for i := len(stack) - 1; i >= 0; i-- {
						if containsTag(targets, stack[i].tag) {
							idx = i
							break
						}
					}
					if idx != -1 {
						for len(stack) > idx {
							top := stack[len(stack)-1]
							stack = stack[:len(stack)-1]
							if err := flush(top); err != nil {
								return
							}
						}
					}
				}

				// Defensive depth backstop (see maxStackDepth's doc
				// comment): once hit, further nesting is simply not
				// tracked at all -- see suppressedDepth's doc comment
				// for why its matching end tag must be handled
				// specially rather than falling through to the normal
				// end-tag stack search below.
				if len(stack) >= maxStackDepth {
					suppressedDepth++
					continue
				}

				var parentPath string
				var parentCounts *map[string]int
				if len(stack) == 0 {
					parentCounts = &rootCounts
				} else {
					parent := stack[len(stack)-1]
					parentPath = parent.path
					parentCounts = &parent.counts
				}
				path := pathFor(parentPath, parentCounts, tag)
				stack = append(stack, &frame{
					tag:      tag,
					path:     path,
					raw:      rawTextElements[tag],
					verbatim: verbatimElements[tag],
				})

			case html.EndTagToken:
				name, _ := z.TagName()
				tag := string(name)

				if suppressedDepth > 0 {
					// Assume this end tag closes one of the untracked,
					// suppressed elements rather than searching stack --
					// see suppressedDepth's doc comment for why a real
					// search here risks matching and prematurely
					// closing an unrelated ancestor with the same tag
					// name.
					suppressedDepth--
					continue
				}

				idx := -1
				for i := len(stack) - 1; i >= 0; i-- {
					if stack[i].tag == tag {
						idx = i
						break
					}
				}
				if idx == -1 {
					continue // stray end tag with no open match: ignore
				}
				for len(stack) > idx {
					top := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					if err := flush(top); err != nil {
						return
					}
				}

			case html.TextToken:
				if len(stack) == 0 {
					continue // text outside any element: not addressable
				}
				top := stack[len(stack)-1]
				if top.raw {
					continue
				}
				text := z.Text()
				if top.verbatim {
					// Unlike a collapsible element, every byte here
					// (including whitespace) is significant content, so
					// any real, preserved '\n' within this single token
					// closes off a segment right where it occurs, not
					// just at a <br>. Each split part starts exactly
					// tokenStart+i lines in, since each '\n' -- wherever
					// it originates -- advances exactly one line.
					parts := bytes.Split(text, newlineBytes)
					for i, part := range parts {
						if i > 0 {
							top.finishLine()
						}
						if top.segStartLine == 0 {
							top.segStartLine = tokenStart + i
						}
						top.text.Write(part)
					}
				} else {
					if top.segStartLine == 0 && contentLine > 0 {
						top.segStartLine = contentLine
					}
					top.endsWS = writeCollapsed(&top.text, text, top.endsWS)
				}

			case html.CommentToken, html.DoctypeToken:
				// Neither carries searchable page content.
			}
		}

		// Any elements still open at end-of-document (unclosed tags with
		// no later matching/implicit close) are flushed in the same
		// innermost-first order a real close tag would have used.
		for i := len(stack) - 1; i >= 0; i-- {
			if err := flush(stack[i]); err != nil {
				return
			}
		}
	}()

	return units, errc
}
