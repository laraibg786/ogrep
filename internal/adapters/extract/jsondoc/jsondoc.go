// Package jsondoc implements a ports.DocumentExtractor for JSON
// documents.
//
// Extraction streams the document via encoding/json.Decoder.Token(),
// recursively flattening objects and arrays into jq-style paths (e.g.
// ".a.b[2]") without ever buffering the full decoded document. Each
// scalar leaf (or empty object/array) becomes one domain.TextUnit whose
// text is "<path> = <value literal>", where the value literal is the
// exact JSON encoding of the token (numbers are preserved verbatim via
// json.Number rather than round-tripping through float64).
//
// Each leaf's real source line/column is also tracked (see posTracker),
// so its domain.Location can link straight to the value's actual
// position in the file rather than just carrying its jq path.
//
// The package self-registers into registry.Default on package init, so
// importing this package for its side effect (see
// internal/adapters/extract/all) is enough to make it available.
package jsondoc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// Extractor implements ports.DocumentExtractor for JSON documents.
type Extractor struct{}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "json" }

// Extensions is an optional fast-path hint consumed by the registry;
// Sniff (content inspection) is what actually decides.
func (Extractor) Extensions() []string { return []string{".json"} }

// utf8BOM is the UTF-8 byte order mark some editors (notably on
// Windows) prepend to text files, including JSON. encoding/json doesn't
// skip it, so both Sniff and Extract strip it explicitly -- otherwise a
// BOM-prefixed JSON file would be silently declined by Sniff (falling
// back to a plain-text grep of what is actually valid, structured JSON)
// or fail to decode at all if it somehow reached Extract anyway.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// sniffWindow bounds how many leading bytes are inspected when deciding
// whether a file looks like JSON. JSON is streamed by Extract, so there
// is no size gate here (unlike YAML/XML) — this window only exists to
// keep the cheap trial decode in Sniff bounded.
const sniffWindow = 4096

// bareIdentRe matches a jq bare identifier: a leading letter or
// underscore followed by letters, digits, or underscores.
var bareIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// jqSegment renders a single object-key path segment in jq syntax. Keys
// that are valid jq bare identifiers render as ".key"; everything else
// (dashes, spaces, leading digits, empty strings, unicode, etc.) renders
// as the bracketed, quoted form (e.g. `.["foo-bar"]`) so the resulting
// path can be pasted directly into jq without being misparsed (e.g. as
// subtraction for a key like "foo-bar").
func jqSegment(key string) string {
	if bareIdentRe.MatchString(key) {
		return "." + key
	}
	b, err := json.Marshal(key)
	if err != nil {
		// json.Marshal on a string cannot fail; this is unreachable in
		// practice, but fall back to a safe representation rather than
		// panicking.
		b = []byte(fmt.Sprintf("%q", key))
	}
	return ".[" + string(b) + "]"
}

// appendKey joins path with a mapping key's jq segment. jqSegment always
// returns a leading "."; the path root is also the literal "." (jq's own
// identity path), so a naive path+jqSegment(key) concatenation at the
// root would double up into "..key". Array-index segments don't have
// this problem (they're appended with no separator, e.g. ".[0]"), only
// named-key segments do, since both the root and jqSegment supply a dot.
func appendKey(path, key string) string {
	seg := jqSegment(key)
	if path == "." {
		return seg
	}
	return path + seg
}

// Sniff implements ports.DocumentExtractor. It peeks a bounded prefix of
// the file, skips leading whitespace, checks that the first non-
// whitespace byte plausibly starts a JSON value, and confirms the first
// token decodes without error. Malformed JSON is deliberately NOT
// claimed here so the registry falls back to the text extractor.
func (Extractor) Sniff(path string, ra io.ReaderAt, size int64) (ok bool) {
	// Sniff must never panic-crash the caller.
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()

	if size <= 0 {
		return false
	}

	n := size
	if n > sniffWindow {
		n = sniffWindow
	}
	buf := make([]byte, n)
	nRead, err := ra.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return false
	}
	buf = buf[:nRead]
	buf = bytes.TrimPrefix(buf, utf8BOM)

	trimmed := bytes.TrimLeft(buf, " \t\r\n")
	if len(trimmed) == 0 {
		return false
	}
	c := trimmed[0]
	switch {
	case c == '{' || c == '[' || c == '"' || c == '-' || (c >= '0' && c <= '9'):
	case bytes.HasPrefix(trimmed, []byte("true")):
	case bytes.HasPrefix(trimmed, []byte("false")):
	case bytes.HasPrefix(trimmed, []byte("null")):
	default:
		return false
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if _, err := dec.Token(); err != nil {
		return false
	}
	return true
}

// Extract implements ports.DocumentExtractor, streaming the document's
// flattened key/value pairs as TextUnits.
func (Extractor) Extract(ctx context.Context, ra io.ReaderAt, size int64) (<-chan domain.TextUnit, <-chan error) {
	units := make(chan domain.TextUnit)
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
				case errc <- fmt.Errorf("panic during json extraction: %v", r):
				default:
				}
			}
		}()

		start := int64(0)
		if size >= int64(len(utf8BOM)) {
			peek := make([]byte, len(utf8BOM))
			if _, err := ra.ReadAt(peek, 0); err == nil && bytes.Equal(peek, utf8BOM) {
				start = int64(len(utf8BOM))
			}
		}
		sr := io.NewSectionReader(ra, start, size-start)
		pt := &posTracker{r: sr}
		dec := json.NewDecoder(pt)
		dec.UseNumber()

		emit := func(path string, tok json.Token, offset int64) error {
			var literal string
			if d, isDelim := tok.(json.Delim); isDelim {
				literal = string(d) + string(closingFor(d))
			} else {
				b, err := json.Marshal(tok)
				if err != nil {
					return err
				}
				literal = string(b)
			}
			line, col := pt.lineCol(offset)
			unit := domain.TextUnit{
				Location: jsonPathLocation{Path: path, Line: line, Column: col},
				Text:     fmt.Sprintf("%s = %s", path, literal),
			}
			select {
			case units <- unit:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if err := walk(ctx, dec, ".", emit); err != nil {
			select {
			case errc <- err:
			default:
			}
		}
	}()

	return units, errc
}

// posTracker wraps an io.Reader, recording the absolute byte offset of
// every newline it sees as bytes flow through Read. Recording absolute
// offsets (not a running count) makes this exact regardless of how far
// ahead of the decoder's own logical parse position json.Decoder's
// internal buffering reads: a newline at file offset 743 is recorded as
// 743 no matter when it's observed, so later asking "how many newlines
// occurred at or before decoder offset X" (via lineCol) is correct even
// though more bytes may already have been buffered by the time we ask.
// This is what makes deriving a real line/column from
// json.Decoder.InputOffset() exact rather than merely approximate.
type posTracker struct {
	r        io.Reader
	off      int64
	newlines []int64 // absolute byte offsets of '\n', strictly increasing
}

func (t *posTracker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	for i := 0; i < n; i++ {
		if p[i] == '\n' {
			t.newlines = append(t.newlines, t.off+int64(i))
		}
	}
	t.off += int64(n)
	return n, err
}

// lineCol converts a decoder byte offset (as returned by
// json.Decoder.InputOffset()) into a 1-based (line, column) via binary
// search over the newlines seen so far. offset never exceeds the number
// of bytes already handed to Read, since the decoder can't report having
// consumed bytes it was never given, so the newlines slice always has
// enough information to answer correctly.
func (t *posTracker) lineCol(offset int64) (line, col int) {
	i := sort.Search(len(t.newlines), func(i int) bool { return t.newlines[i] >= offset })
	lineStart := int64(-1)
	if i > 0 {
		lineStart = t.newlines[i-1]
	}
	return i + 1, int(offset - lineStart)
}

// closingFor returns the matching closing delimiter for an empty
// object/array leaf's literal rendering ("{}" or "[]").
func closingFor(d json.Delim) byte {
	if d == '{' {
		return '}'
	}
	return ']'
}

// walk recursively decodes the token stream rooted at the delimiter (or
// scalar) about to be read from dec, invoking emit once per scalar leaf
// or empty object/array, with path already carrying that value's
// jq-style location and offset the decoder's byte position (via
// dec.InputOffset()) at the end of that leaf's token -- always on the
// correct source line, since no JSON scalar token can itself contain an
// unescaped literal newline; see posTracker.lineCol for how emit turns
// this into a line/column. It correctly consumes matching closing
// delimiters and detects empty containers via whether the loop body ran
// at all.
func walk(ctx context.Context, dec *json.Decoder, path string, emit func(path string, tok json.Token, offset int64) error) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			any := false
			for dec.More() {
				any = true
				if err := ctx.Err(); err != nil {
					return err
				}
				key, err := dec.Token()
				if err != nil {
					return err
				}
				keyStr, _ := key.(string)
				if err := walk(ctx, dec, appendKey(path, keyStr), emit); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return err
			}
			if !any {
				return emit(path, t, dec.InputOffset()) // empty object -> "<path> = {}"
			}
		} else if t == '[' {
			i := 0
			for dec.More() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := walk(ctx, dec, fmt.Sprintf("%s[%d]", path, i), emit); err != nil {
					return err
				}
				i++
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return err
			}
			if i == 0 {
				return emit(path, t, dec.InputOffset()) // empty array -> "<path> = []"
			}
		}
	default:
		return emit(path, tok, dec.InputOffset()) // scalar leaf: string, json.Number, bool, nil
	}
	return nil
}
