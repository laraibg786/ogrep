// Package tomldoc implements a ports.DocumentExtractor for TOML
// documents.
//
// This package parses via github.com/pelletier/go-toml/v2/unstable's
// Parser, an iterative AST parser exposed specifically for tools like
// this one. This replaced an earlier implementation built on
// github.com/BurntSushi/toml, which decoded straight into a
// map[string]any: that decoder discards three things this package
// needs and BurntSushi/toml's exported surface simply has no way to
// recover -- confirmed by reading its source, not assumed:
//
//  1. Real line/column for a decoded value. BurntSushi/toml's MetaData
//     only exposes position information for a parse *error*, never for
//     a successfully decoded value.
//  2. The value's original source text. toml.Decode re-encodes into Go
//     values, so a hex/octal/binary integer literal, a legacy escape
//     sequence, or one of TOML's four datetime forms all lose their
//     exact source spelling.
//  3. Comments. toml.Decode drops them before extraction ever sees the
//     tree.
//
// unstable.Parser fixes all three, confirmed empirically (via small
// probe programs against the real library, and by reading
// unstable/parser.go and unstable/ast.go in the module cache) rather
// than assumed from documentation:
//
//   - Every Node carries a Raw Range (byte Offset/Length) directly into
//     the original input; Parser.Shape(n.Raw) converts that into a real
//     Position{Offset, Line, Column} pair (1-based). This is not
//     reconstructed from surrounding tokens -- it's the exact span the
//     parser matched for that node, so it's used directly for both
//     position (Line/Column) and verbatim source text (via lineText,
//     see below) for every scalar kind: String, Bool, Float, Integer,
//     LocalDate, LocalTime, LocalDateTime, DateTime. A hex integer's
//     Raw is literally "0xdeadBEEF"; a legacy escape's Raw is literally
//     "a\"b", not a re-decoded/re-escaped rendering of either.
//   - Parser.KeepComments, when set to true, makes NextExpression()
//     surface "#..." comments as first-class nodes: either as their own
//     top-level expression (a standalone comment line) or attached via
//     Node.Next() to the expression it trails (an inline "key = value #
//     comment" line) -- confirmed with a probe program, not assumed.
//     Comments nested inside a multi-line array (one element per
//     physical line, some followed by "# ...") show up interleaved
//     among the array's children with Kind == unstable.Comment, also
//     confirmed empirically. There is no case where go-toml/v2 discards
//     a comment with no way to recover it, so this package never falls
//     back to a hand-rolled line scanner for comments.
//
// Extraction still flattens table keys and array indices into jq/yq-
// style paths (e.g. ".a.b[2]"), same as jsondoc/yamldoc/the old
// implementation -- but unlike the old implementation, table/array-of-
// tables order comes for free from the parser's flat, in-order stream of
// top-level expressions: NextExpression() yields Table/ArrayTable/
// KeyValue/Comment nodes in exact document order, so there is no map
// with randomized iteration order to sort around anymore (see
// resolveTablePath/resolveArrayTablePath). Sibling order is simply
// document order.
//
// TextUnit granularity and Human()'s format: a TOML key=value pair is
// almost always exactly one physical source line, but a value can
// legitimately span more than one -- a multi-line basic/literal string
// (three double quotes or three single quotes), or a multi-line
// array/inline-table-bearing
// expression with one element per line. domain.TextUnit.Text must never
// contain an embedded newline (see domain.TextUnit's doc comment), so
// this package does not use a KeyValue's full multi-line Raw span as
// Text. Instead, every emitted TextUnit's Text is the single physical
// source line containing that value's own start position (see lineText)
// -- for the overwhelmingly common one-value-per-line case this is
// exactly the "key = value" text a reader would expect; for a value
// spread across several lines, each physical line the walk visits
// becomes its own TextUnit at its own real Line/Column, rather than one
// unit whose Text would have to mash multiple lines together. Distinct
// leaf values that happen to share one physical line (e.g. a compact
// "nums = [1, 2, 3]") legitimately produce multiple TextUnits with
// identical Text but different Location (jq path and column) -- an
// accepted, documented tradeoff: it mirrors how jsondoc/yamldoc report
// one TextUnit per leaf even when several sibling values share a source
// line, just without folding the jq path into Text/Human() (see
// tomlPathLocation.Human's doc comment for why not).
//
// Whether a table declared but never populated (e.g. "[empty]\n" with
// nothing under it) stays empty can only be determined once the whole
// document has been scanned -- there is no way to know a table stays
// empty until EOF (or wherever else it might have been populated) has
// been reached. So Extract runs in two passes over its own small,
// already-decoded record list (not over the parser itself, which cannot
// be rewound safely once NextExpression has moved past an expression --
// see its own doc comment on Node lifetime): first collecting every
// leaf, comment, and "possibly-empty table" placeholder in document
// order, tracking which table paths receive at least one key along the
// way (see populated); second, resolving each placeholder into a real
// leaf (using the header line's own verbatim text -- e.g. "[empty]" --
// as Text, not a reconstructed "= {}") only if it turned out to truly be
// empty, then streaming the final, ordered list out over the TextUnit
// channel. Since the whole file is already read into memory for the
// parser (see maxSniffSize), this second pass adds no new memory-scaling
// behavior.
//
// The package self-registers into registry.Default on package init, so
// importing this package for its side effect (see
// internal/adapters/extract/all) is enough to make it available.
package tomldoc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2/unstable"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// maxSniffSize caps the file size Sniff (and therefore Extract, since
// the registry only calls Extract on files Sniff has claimed) will
// attempt to parse. unstable.Parser still buffers the entire document in
// memory -- there is no streaming TOML parser -- so gating oversized
// files here means the registry's fallback dispatch hands them to the
// permissive text plugin instead, degrading gracefully rather than
// risking an unbounded-memory parse.
//
// This stays at the same 8 MiB ceiling the BurntSushi/toml-based
// implementation used, even though go-toml/v2's unstable.Parser is
// substantially faster: BenchmarkExtractAtSizeCap (perf_bench_test.go),
// re-run against this new implementation on an 8 MiB synthetic document,
// measured ~164ms/op -- versus the roughly 10 seconds profiling
// previously found for toml.Decode at that size (a ~60x improvement,
// both real `go test -bench` numbers, not estimated). Real-world TOML is
// still config-file-sized -- Cargo.toml, pyproject.toml, application
// configs -- not tens of megabytes, so 8 MiB remains an honest ceiling
// for this format specifically; the bottleneck this constant originally
// guarded against is gone, but there's no real-world TOML file this cap
// would wrongly exclude, so raising it further would only be a change
// for its own sake.
const maxSniffSize = 8 * 1024 * 1024 // 8 MiB

// Extractor implements ports.DocumentExtractor for TOML documents.
type Extractor struct{}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "toml" }

// Extensions is an optional fast-path hint consumed by the registry;
// Sniff (content inspection) is what actually decides.
func (Extractor) Extensions() []string { return []string{".toml"} }

// isBareIdent reports whether key is a jq bare identifier: a leading
// letter or underscore followed by letters, digits, or underscores.
// This is a hand-rolled equivalent of the regexp
// `^[A-Za-z_][A-Za-z0-9_]*$` -- profiling showed the regexp.MatchString
// call was this package's single largest self-inflicted CPU cost (over
// the sort and the JSON encoding combined), since it runs once per
// table key across the whole document. A manual byte loop accepts and
// rejects exactly the same set of strings without the regexp engine's
// per-call overhead.
func isBareIdent(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
			// Valid in any position.
		case c >= '0' && c <= '9':
			// Valid, but not as the leading character.
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// jqSegment renders a single table-key path segment in jq/yq syntax.
// Keys that are valid jq bare identifiers render as ".key"; everything
// else (dashes, spaces, leading digits, empty strings, unicode, etc.)
// renders as the bracketed, quoted form (e.g. `.["foo-bar"]`) so the
// resulting path can be pasted directly into jq/yq without being
// misparsed (e.g. as subtraction for a key like "foo-bar"). This
// duplicates jsondoc's/yamldoc's identical helper rather than sharing
// code across packages, per the established convention.
func jqSegment(key string) string {
	if isBareIdent(key) {
		return "." + key
	}
	return ".[" + jsonQuote(key) + "]"
}

// jsonQuote renders s as a JSON string literal (double-quoted, with the
// usual backslash escapes) without HTML-escaping '<', '>', or '&' --
// unlike json.Marshal, which escapes those three characters by default
// (a mitigation aimed at strings embedded directly into HTML, not
// relevant here). Left un-disabled, that escaping silently corrupts any
// TOML key containing them, making the path text wrong to display.
// encoding/json's Encoder.SetEscapeHTML(false) is the standard way to
// opt out.
func jsonQuote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// json.Marshal-equivalent encoding of a string cannot fail;
		// this is unreachable in practice, but fall back to a safe
		// representation rather than panicking.
		return strconv.Quote(s)
	}
	// Encode appends a trailing newline; strip it.
	return strings.TrimSuffix(buf.String(), "\n")
}

// appendKey joins path with a table key's jq/yq segment. jqSegment
// always returns a leading "."; the path root is also the literal "."
// (jq's own identity path), so a naive path+jqSegment(key) concatenation
// at the root would double up into "..key". Array-index segments don't
// have this problem (they're appended with no separator, e.g. ".[0]"),
// only named-key segments do, since both the root and jqSegment supply a
// dot.
func appendKey(path, key string) string {
	seg := jqSegment(key)
	if path == "." {
		return seg
	}
	return path + seg
}

// readAll reads the full size bytes from ra via a bounded
// io.SectionReader, mirroring how the other size-gated extractors turn
// an io.ReaderAt into a byte slice.
func readAll(ra io.ReaderAt, size int64) ([]byte, error) {
	sr := io.NewSectionReader(ra, 0, size)
	return io.ReadAll(sr)
}

// Sniff implements ports.DocumentExtractor. It gates on size first (see
// maxSniffSize), then attempts a full parse (without KeepComments --
// Sniff only needs to know whether the document is structurally real
// TOML, not its comments), requiring at least one real top-level
// expression: a bare successful parse alone is not a useful signal,
// since an empty file, a whitespace-only file, or a comment-only file
// all parse "successfully" as an empty document -- with KeepComments
// false, a comment-only document yields zero expressions from
// NextExpression, exactly like an empty one, so this one counter
// correctly rejects both. Malformed TOML is declined so the registry
// falls back to the text extractor.
func (Extractor) Sniff(path string, ra io.ReaderAt, size int64) (ok bool) {
	// Sniff must never panic-crash the caller: go-toml/v2 is third-party
	// code operating on attacker-controllable input.
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()

	if size <= 0 || size > maxSniffSize {
		return false
	}

	data, err := readAll(ra, size)
	if err != nil {
		return false
	}

	p := &unstable.Parser{}
	p.Reset(data)
	saw := false
	for p.NextExpression() {
		saw = true
	}
	if err := p.Error(); err != nil {
		return false
	}
	return saw
}

// lineStarts returns the byte offset each line of data begins at (0-
// indexed by line number - 1), for lineText to look up a physical line
// by its 1-based line number without rescanning data from the start
// each time. Computed once per Extract call.
func lineStarts(data []byte) []int {
	starts := make([]int, 1, 64)
	starts[0] = 0
	for i, b := range data {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// lineText returns the physical source line at line (1-based),
// excluding its terminating "\n" (and a trailing "\r", for CRLF input),
// via the precomputed starts (see lineStarts). This is what makes
// tomldoc's TextUnit.Text verbatim: the real bytes of that line, not a
// reconstruction of any kind.
func lineText(data []byte, starts []int, line int) string {
	if line < 1 || line > len(starts) {
		return ""
	}
	start := starts[line-1]
	end := len(data)
	if line < len(starts) {
		end = starts[line] - 1 // exclude the '\n' itself
	}
	if end < start {
		end = start
	}
	return strings.TrimSuffix(string(data[start:end]), "\r")
}

// record is one already-decoded entry collected during Extract's first
// pass over the parser (see the package doc comment for why a two-pass
// design is needed at all): either a real leaf value, a comment, or a
// placeholder for a table/array-table header whose emptiness can only be
// known once the whole document has been scanned. Plain Go values only
// (no unstable.Node/Parser pointers survive past NextExpression's next
// call), so records can be freely buffered and reordered.
type record struct {
	isComment bool
	isHeader  bool // placeholder; resolved in pass two
	path      string
	line, col int
}

// walker carries the state Extract's first pass threads through the
// parser's flat stream of top-level expressions: the AST parser itself
// (for Raw byte ranges), the source bytes and their line index (for
// lineText and posAt), and the bookkeeping resolveTablePath/
// resolveArrayTablePath and the empty-table check need.
type walker struct {
	ctx        context.Context
	p          *unstable.Parser
	data       []byte
	starts     []int
	arrayCount map[string]int
	populated  map[string]bool
	records    []record
}

// posAt converts an absolute byte offset into a 1-based (line, column)
// via binary search over w.starts (see lineStarts).
//
// This deliberately does NOT use unstable.Parser.Shape/Parser.position:
// profiling BenchmarkExtractKeyDense first surfaced -- and reading
// parser.go's position() confirmed -- that Shape rescans p.data from
// byte 0 every single call (it counts '\n' bytes from the start of the
// document up to the target offset each time), making a per-node Shape
// call O(document size) rather than O(1). Calling it once per node
// across a document with thousands of nodes therefore made the whole
// walk effectively O(n²), which is exactly what turned up as multi-
// second runtime on a 20,000-key benchmark document. Node.Raw.Offset is
// a public field, so this package computes the same (line, column)
// itself from its own precomputed line-start index instead, the same
// O(log n)-per-lookup technique jsoncdoc's lineCol already uses for the
// identical reason.
func (w *walker) posAt(offset int) (line, col int) {
	if offset < 0 {
		offset = 0
	}
	// i = index of the first line-start strictly greater than offset;
	// i-1 is therefore the line offset belongs to (starts[0] == 0, so
	// i is always >= 1).
	i := sort.Search(len(w.starts), func(i int) bool { return w.starts[i] > offset })
	return i, offset - w.starts[i-1] + 1
}

// emitLeaf records a real key/value leaf at path, positioned at n's
// start.
func (w *walker) emitLeaf(path string, n *unstable.Node) {
	line, col := w.posAt(int(n.Raw.Offset))
	w.records = append(w.records, record{path: path, line: line, col: col})
}

// emitComment records a comment node.
func (w *walker) emitComment(n *unstable.Node) {
	line, col := w.posAt(int(n.Raw.Offset))
	w.records = append(w.records, record{isComment: true, line: line, col: col})
}

// emitHeaderPlaceholder records a Table/ArrayTable header at path,
// positioned at its first key part (headerKey) -- resolved into a real
// leaf in pass two only if path never receives a key (see populated).
func (w *walker) emitHeaderPlaceholder(path string, headerKey *unstable.Node) {
	line, col := w.posAt(int(headerKey.Raw.Offset))
	w.records = append(w.records, record{isHeader: true, path: path, line: line, col: col})
}

// keyParts collects a Table/ArrayTable/KeyValue node's (possibly
// dotted) key as its individual, decoded string parts, in order.
func keyParts(root *unstable.Node) []string {
	var parts []string
	it := root.Key()
	for it.Next() {
		parts = append(parts, string(it.Node().Data))
	}
	return parts
}

// resolveTablePath resolves a "[a.b.c]" table header's dotted key parts
// into a full jq/yq path from the document root, marking each ancestor
// path as populated along the way (declaring a subtable under an
// ancestor makes that ancestor non-empty, exactly like a real key
// would). If an intermediate segment matches an already-declared
// array-of-tables path, this descends into that array's most recently
// declared element (TOML's own table-nesting rule for dotted headers
// following an array-of-tables), via w.arrayCount.
func (w *walker) resolveTablePath(parts []string) string {
	path := "."
	for _, part := range parts {
		w.populated[path] = true
		path = appendKey(path, part)
		if n, ok := w.arrayCount[path]; ok {
			path = fmt.Sprintf("%s[%d]", path, n-1)
		}
	}
	return path
}

// resolveArrayTablePath resolves a "[[a.b.c]]" array-of-tables header's
// dotted key parts the same way resolveTablePath does for all but the
// final segment; the final segment is the array itself, so this
// increments its element count and appends the new element's index.
func (w *walker) resolveArrayTablePath(parts []string) string {
	path := "."
	for i, part := range parts {
		w.populated[path] = true
		path = appendKey(path, part)
		if i == len(parts)-1 {
			w.arrayCount[path]++
			path = fmt.Sprintf("%s[%d]", path, w.arrayCount[path]-1)
		} else if n, ok := w.arrayCount[path]; ok {
			path = fmt.Sprintf("%s[%d]", path, n-1)
		}
	}
	return path
}

// walkKeyValue processes a top-level (or inline-table-nested) KeyValue
// node, resolving its (possibly dotted) key against base and then
// walking its value.
func (w *walker) walkKeyValue(kv *unstable.Node, base string) {
	path := base
	it := kv.Key()
	for it.Next() {
		w.populated[path] = true
		path = appendKey(path, string(it.Node().Data))
	}
	w.walkValue(kv.Value(), path)
}

// walkValue processes a value node -- a scalar leaf, or a container
// (Array/InlineTable) whose own children are walked recursively -- at
// path. Comment nodes interleaved among a container's children (a
// multi-line array with per-element trailing comments) are recorded as
// their own comment records, not counted as array elements.
func (w *walker) walkValue(n *unstable.Node, path string) {
	switch n.Kind {
	case unstable.Array:
		child := n.Child()
		if child == nil {
			w.emitLeaf(path, n) // empty array literal: "path = []"
			return
		}
		idx := 0
		it := n.Children()
		for it.Next() {
			c := it.Node()
			if c.Kind == unstable.Comment {
				w.emitComment(c)
				continue
			}
			w.walkValue(c, fmt.Sprintf("%s[%d]", path, idx))
			idx++
		}

	case unstable.InlineTable:
		child := n.Child()
		if child == nil {
			w.emitLeaf(path, n) // empty inline table: "path = {}"
			return
		}
		it := n.Children()
		for it.Next() {
			c := it.Node()
			if c.Kind == unstable.Comment {
				w.emitComment(c)
				continue
			}
			w.walkKeyValue(c, path)
		}

	default:
		// Scalar leaf: String, Bool, Float, Integer, LocalDate,
		// LocalTime, LocalDateTime, DateTime.
		w.emitLeaf(path, n)
	}
}

// run drives the parser's flat top-level expression stream, populating
// w.records in document order. Returns a non-nil error only for a real
// parse failure or context cancellation (ctx.Err() distinguishes the two
// for the caller, matching every other extractor's cancellation
// contract in this repo).
func (w *walker) run() error {
	currentPath := "."
	for w.p.NextExpression() {
		if err := w.ctx.Err(); err != nil {
			return err
		}

		root := w.p.Expression()
		switch root.Kind {
		case unstable.Comment:
			w.emitComment(root)

		case unstable.Table:
			parts := keyParts(root)
			currentPath = w.resolveTablePath(parts)
			w.emitHeaderPlaceholder(currentPath, root.Child())

		case unstable.ArrayTable:
			parts := keyParts(root)
			currentPath = w.resolveArrayTablePath(parts)
			w.emitHeaderPlaceholder(currentPath, root.Child())

		case unstable.KeyValue:
			w.walkKeyValue(root, currentPath)
		}

		// A trailing "key = value # comment" or "[table] # comment"
		// attaches its comment as root's own Next() sibling, not as a
		// child (confirmed empirically -- see the package doc comment).
		if n := root.Next(); n != nil && n.Kind == unstable.Comment {
			w.emitComment(n)
		}
	}
	return w.p.Error()
}

// Extract implements ports.DocumentExtractor, walking the parsed
// document's flattened key/value pairs (and comments) as TextUnits.
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
				case errc <- fmt.Errorf("panic during toml extraction: %v", r):
				default:
				}
			}
		}()

		data, err := readAll(ra, size)
		if err != nil {
			select {
			case errc <- err:
			default:
			}
			return
		}

		p := &unstable.Parser{KeepComments: true}
		p.Reset(data)

		w := &walker{
			ctx:        ctx,
			p:          p,
			data:       data,
			starts:     lineStarts(data),
			arrayCount: map[string]int{},
			populated:  map[string]bool{},
		}

		if err := w.run(); err != nil {
			// A cancelled/expired context is not a real extraction
			// failure -- it means a consumer stopped early (e.g. -m 1
			// on a huge file). Reporting that as a warning would be
			// spurious noise on ordinary early exit; mirror jsoncdoc's
			// handling of this exact case rather than yamldoc's, which
			// incorrectly reports it.
			if ctx.Err() == nil {
				select {
				case errc <- err:
				default:
				}
			}
			return
		}

		// Pass two: resolve header placeholders (see the package doc
		// comment) and stream the final, ordered records out.
		for _, rec := range w.records {
			if err := ctx.Err(); err != nil {
				return
			}
			if rec.isHeader && w.populated[rec.path] {
				continue // turned out non-empty; no unit for the header itself
			}

			text := lineText(data, w.starts, rec.line)
			var loc domain.Location
			if rec.isComment {
				loc = tomlCommentLocation{Line: rec.line, Column: rec.col}
			} else {
				loc = tomlPathLocation{Path: rec.path, Line: rec.line, Column: rec.col}
			}

			select {
			case units <- domain.TextUnit{Location: loc, Text: text}:
			case <-ctx.Done():
				return
			}
		}
	}()

	return units, errc
}
