// Package xmldoc implements a ports.DocumentExtractor for generic XML
// documents.
//
// Extraction flattens the document tree into one domain.TextUnit per:
//   - element whose direct (non-descendant) text content is non-blank,
//     located at that element's absolute, synthesized XPath (e.g.
//     "/root/item[3]"); and
//   - each attribute on each element, located at "<element xpath>/@name".
//
// It streams via encoding/xml.Decoder.Token() rather than building any
// DOM (stdlib, not a third-party dependency): a stack of open elements
// tracks each level's synthesized XPath, a per-tag-name child counter for
// the "[N]" predicate, and that element's own accumulated direct text.
// Memory is bounded by nesting depth plus the size of whatever single
// element's direct text is currently being accumulated -- never the
// whole document -- so unlike a DOM-based approach there is no need to
// gate Sniff on file size.
//
// The "[N]" predicate is always rendered for a non-root element (e.g.
// "/root/item[1]" even when item has no siblings), rather than the
// conventional "omit [N] for a singleton" xmllint-style rendering: a
// single left-to-right pass can't know in advance whether more same-tag
// siblings will follow the one it's currently on, so it can't decide
// "singleton or not" without either buffering a whole subtree or making
// a second pass -- both of which reintroduce the memory cost streaming
// is meant to avoid. Always including the index is still fully valid,
// navigable XPath. The root element itself has no "[N]": XML's
// well-formedness rules guarantee there is exactly one, so an index on
// it would be meaningless.
//
// Line and column both come from encoding/xml.Decoder.InputPos(), which
// -- unlike the antchfx/xmlquery library this package used to depend on
// -- gives an exact column, not just a line. This is what makes a
// single-line (e.g. minified) XML file's matches locatable at all: a
// line-only location can't distinguish between two matches on the same
// line, which for a minified file is every match in the document.
//
// The package self-registers into registry.Default on package init, so
// importing this package for its side effect (see
// internal/adapters/extract/all) is enough to make it available.
package xmldoc

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"golang.org/x/net/html/charset"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// sniffPeekWindow bounds how many leading bytes Sniff inspects for a
// cheap "does this look like XML" check.
const sniffPeekWindow = 4096

// Extractor implements ports.DocumentExtractor for XML documents.
type Extractor struct{}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "xml" }

// Extensions is an optional fast-path hint consumed by the registry;
// Sniff (content inspection) is what actually decides.
func (Extractor) Extensions() []string { return []string{".xml"} }

// Sniff implements ports.DocumentExtractor. It does a cheap prefix check
// for an XML-like start, then confirms the very first token decodes
// without error -- deliberately not a full-document parse (streaming
// removed the reason YAML/other DOM-based formats need one): a full scan
// just to decide dispatch would cost as much time as Extract itself on a
// large file. This mirrors jsondoc's identical tradeoff: malformed XML
// whose problem is deeper than the first token is still claimed here and
// surfaces as an error from Extract, rather than falling back to text.
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
	switch {
	case len(trimmed) == 0:
		return false
	case bytes.HasPrefix(trimmed, []byte("<?xml")):
		// definitely worth a real token check
	case bytes.HasPrefix(trimmed, []byte("<!--")):
		// a comment before the root element is well-formed XML
	case bytes.HasPrefix(trimmed, []byte("<!DOCTYPE")):
		// a doctype declaration before the root element is well-formed XML
	case trimmed[0] == '<':
		if len(trimmed) < 2 || !isXMLNameStartByte(trimmed[1]) {
			return false
		}
	default:
		return false
	}

	dec := xml.NewDecoder(bytes.NewReader(trimmed))
	dec.CharsetReader = charset.NewReaderLabel
	_, err = dec.Token()
	return err == nil
}

// isXMLNameStartByte reports whether b could be the first byte of an XML
// element name -- good enough to reject obviously non-XML text like "<3
// hours ago" without a full XML NameStartChar implementation, while
// still letting a non-ASCII name (e.g. "<中文>", legal per the XML
// spec) through to dec.Token(), the real, authoritative check right
// after this. Any byte >= 0x80 is the lead or continuation byte of a
// multi-byte UTF-8 sequence, so it's accepted here on the same
// "let the decoder decide" principle -- this check only needs to reject
// bytes that can NEVER start an XML name, not validate ones that can.
func isXMLNameStartByte(b byte) bool {
	return b == '_' || b == ':' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
}

// isNamespaceDecl reports whether name is a namespace declaration
// (xmlns="..." or xmlns:prefix="...") rather than a real attribute.
// encoding/xml represents the default-namespace form as
// {Space: "", Local: "xmlns"} and the prefixed form as
// {Space: "xmlns", Local: prefix} -- without this check, a prefixed
// declaration like xmlns:a="urn:x" would render as "/root/@a", stripped
// of the "xmlns:" that identified it as a declaration and colliding with
// any genuine attribute actually named "a".
func isNamespaceDecl(name xml.Name) bool {
	return name.Space == "xmlns" || (name.Space == "" && name.Local == "xmlns")
}

// frame tracks one currently-open element while streaming: its own
// synthesized XPath, a per-child-tag-name counter for the next child's
// "[N]" index, and its own direct text accumulated so far (children's
// CharData tokens are never written here, since by the time a child is
// open it has pushed its own frame -- see Extract).
type frame struct {
	xpath  string
	counts map[string]int
	text   strings.Builder
	// posSet/line/col anchor this element's location to the end of its
	// *first non-blank* direct-text run, not wherever accumulation
	// happens to end (which for mixed content could be much further into
	// the document, after nested children) and not a whitespace-only run
	// that precedes the real text (e.g. pretty-printed indentation before
	// a child element) -- see Extract's CharData case.
	posSet    bool
	line, col int
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
				case errc <- fmt.Errorf("panic during xml extraction: %v", r):
				default:
				}
			}
		}()

		sr := io.NewSectionReader(ra, 0, size)
		dec := xml.NewDecoder(sr)
		dec.CharsetReader = charset.NewReaderLabel

		emit := func(xpath, text string, line, col int) error {
			select {
			case units <- domain.TextUnit{Location: xmlPathLocation{XPath: xpath, Line: line, Column: col}, Text: text}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		var stack []*frame
		// flush emits the top frame's accumulated direct text (if
		// non-blank) as one unit. Called on EndElement, once that
		// element's own direct text (all runs, before/between/after any
		// children) is fully collected.
		flush := func(f *frame) error {
			text := strings.TrimSpace(f.text.String())
			if text == "" {
				return nil
			}
			return emit(f.xpath, text, f.line, f.col)
		}

		for {
			if err := ctx.Err(); err != nil {
				return
			}
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				select {
				case errc <- err:
				default:
				}
				return
			}

			switch t := tok.(type) {
			case xml.StartElement:
				name := t.Name.Local
				var xpath string
				if len(stack) == 0 {
					xpath = "/" + name // root: no index, XML has exactly one
				} else {
					parent := stack[len(stack)-1]
					parent.counts[name]++
					xpath = fmt.Sprintf("%s/%s[%d]", parent.xpath, name, parent.counts[name])
				}
				stack = append(stack, &frame{xpath: xpath, counts: map[string]int{}})

				if len(t.Attr) > 0 {
					line, col := dec.InputPos()
					for _, attr := range t.Attr {
						if isNamespaceDecl(attr.Name) {
							continue // xmlns="..." or xmlns:prefix="..." -- not real content
						}
						if err := emit(xpath+"/@"+attr.Name.Local, attr.Value, line, col); err != nil {
							return
						}
					}
				}

			case xml.EndElement:
				top := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if err := flush(top); err != nil {
					return
				}

			case xml.CharData:
				if len(stack) == 0 {
					continue // text outside the root element: not addressable
				}
				top := stack[len(stack)-1]
				top.text.Write(t)
				if !top.posSet && len(bytes.TrimSpace(t)) > 0 {
					top.line, top.col = dec.InputPos()
					top.posSet = true
				}
			}
		}
	}()

	return units, errc
}
