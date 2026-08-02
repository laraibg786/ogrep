// Package yamldoc implements a ports.DocumentExtractor for YAML
// documents.
//
// Extraction parses the whole file into a goccy/go-yaml AST (there is no
// cheap streaming YAML parser that also tracks line numbers, so unlike
// jsondoc this package cannot avoid building the full tree) and walks it
// recursively, flattening mapping keys and sequence indices into
// jq/yq-style paths (e.g. ".a.b[2]") exactly like the jsondoc sibling
// package. Each scalar leaf (or empty mapping/sequence) becomes one
// domain.TextUnit whose text is "<path> = <value literal>", where the
// value literal is the node's original source text (via Node.String())
// rather than a re-serialization, so quoting/formatting from the source
// is preserved.
//
// Multi-document files (YAML's "---"-separated documents) are fully
// walked, not just the first: when a file contains more than one
// document, each document's paths are prefixed with "document[N]." (0-
// indexed) so that identical paths in different documents don't
// collapse into indistinguishable lines. Single-document files keep
// bare paths (no prefix), matching jsondoc's style.
//
// Anchors (&anchor) are transparent: the value they annotate is walked
// as if the anchor weren't there, at the same path. Aliases (*alias)
// are NOT expanded — goccy's parser does not resolve them to their
// anchor's value by default (confirmed by inspection: an *ast.AliasNode
// is returned as-is, with GetValue() resolving lazily but the node
// itself remaining unexpanded in the tree), so an alias is walked as a
// leaf whose value text is its literal reference form (e.g. "*anchor").
// This is the simplest correct behavior and avoids having to detect and
// break reference cycles.
//
// The package self-registers into registry.Default on package init, so
// importing this package for its side effect (see
// internal/adapters/extract/all) is enough to make it available.
package yamldoc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// maxSniffSize caps the file size Sniff (and therefore Extract, since
// the registry only calls Extract on files Sniff has claimed) will
// attempt to parse. goccy/go-yaml has no cheap streaming parser that
// also tracks path/line info — it builds a full AST in memory — so
// gating oversized files here means the registry's fallback dispatch
// hands them to the permissive text plugin instead, degrading
// gracefully to plain-text grep rather than risking OOM on a huge file.
const maxSniffSize = 64 * 1024 * 1024 // 64 MiB

// Extractor implements ports.DocumentExtractor for YAML documents.
type Extractor struct{}

func init() {
	registry.Default.Register(Extractor{})
}

// Name implements ports.DocumentExtractor.
func (Extractor) Name() string { return "yaml" }

// Extensions is an optional fast-path hint consumed by the registry;
// Sniff (content inspection) is what actually decides.
func (Extractor) Extensions() []string { return []string{".yaml", ".yml"} }

// bareIdentRe matches a jq bare identifier: a leading letter or
// underscore followed by letters, digits, or underscores.
var bareIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// jqSegment renders a single mapping-key path segment in jq/yq syntax.
// Keys that are valid jq bare identifiers render as ".key"; everything
// else (dashes, spaces, leading digits, empty strings, unicode, etc.)
// renders as the bracketed, quoted form (e.g. `.["foo-bar"]`) so the
// resulting path can be pasted directly into jq/yq without being
// misparsed (e.g. as subtraction for a key like "foo-bar"). This
// duplicates jsondoc's identical helper rather than sharing code across
// packages, per the plan.
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

// appendKey joins path with a mapping key's jq/yq segment. jqSegment
// always returns a leading "."; the path root is also the literal "."
// (jq's own identity path, and the multi-document "document[N]" root
// below), so a naive path+jqSegment(key) concatenation at the root would
// double up into "..key". Array-index segments don't have this problem
// (they're appended with no separator, e.g. ".[0]"), only named-key
// segments do, since both the root and jqSegment supply a dot.
func appendKey(path, key string) string {
	seg := jqSegment(key)
	if path == "." {
		return seg
	}
	return path + seg
}

// mapKeyString renders a YAML mapping key node's scalar value as a
// string, for feeding into jqSegment. YAML permits non-string map keys
// (e.g. 123: x, true: x); yq renders their path segments using the
// key's string form, so this converts the key's scalar value to its
// string representation first, then jqSegment applies the same
// escaping rule as for genuine string keys.
func mapKeyString(key ast.MapKeyNode) string {
	switch k := key.(type) {
	case *ast.StringNode:
		return k.Value
	case *ast.NullNode:
		return "null"
	case ast.ScalarNode:
		return fmt.Sprintf("%v", k.GetValue())
	default:
		// No known scalar representation (e.g. a merge key "<<"); fall
		// back to the node's literal source text.
		return key.String()
	}
}

// Sniff implements ports.DocumentExtractor. It gates on size first (see
// maxSniffSize), then attempts a full parse: a file that parses cleanly
// as YAML is claimed; a parse error or oversized file is declined so
// the registry falls back to the text extractor.
func (Extractor) Sniff(path string, ra io.ReaderAt, size int64) (ok bool) {
	// Sniff must never panic-crash the caller.
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()

	if size > maxSniffSize {
		return false
	}
	if size <= 0 {
		return false
	}

	data, err := readAll(ra, size)
	if err != nil {
		return false
	}
	f, err := parser.ParseBytes(data, 0)
	if err != nil {
		return false
	}
	return hasStructure(f)
}

// hasStructure reports whether f contains at least one non-empty mapping
// or sequence in any of its documents. A "successful parse" alone is not
// a useful YAML signal: goccy/go-yaml (like most YAML parsers) happily
// parses arbitrary plain text -- including binary garbage -- as a single
// bare scalar document, since YAML's "plain scalar" production is
// extremely permissive. Without this check, Sniff would claim almost
// any file (confirmed: it previously misclaimed MS Office's transient
// "~$name.ext" lock files, whose content is a short plain-text string
// with no YAML syntax at all). Requiring real structure -- an actual
// "key: value" mapping or a "- item" sequence -- is what actually
// distinguishes YAML from incidental plain text.
func hasStructure(f *ast.File) bool {
	for _, doc := range f.Docs {
		if doc.Body != nil && nodeHasStructure(doc.Body) {
			return true
		}
	}
	return false
}

// nodeHasStructure reports whether node is, or contains, a non-empty
// mapping or sequence.
func nodeHasStructure(node ast.Node) bool {
	switch v := node.(type) {
	case *ast.MappingNode:
		return len(v.Values) > 0
	case *ast.MappingValueNode:
		return true
	case *ast.SequenceNode:
		return len(v.Values) > 0
	case *ast.AnchorNode:
		return nodeHasStructure(v.Value)
	case *ast.TagNode:
		return nodeHasStructure(v.Value)
	default:
		return false
	}
}

// readAll reads the full size bytes from ra via a bounded
// io.SectionReader, mirroring how the other extractors turn an
// io.ReaderAt into a byte slice.
func readAll(ra io.ReaderAt, size int64) ([]byte, error) {
	sr := io.NewSectionReader(ra, 0, size)
	return io.ReadAll(sr)
}

// Extract implements ports.DocumentExtractor, streaming the parsed
// document's flattened key/value pairs as TextUnits.
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
				case errc <- fmt.Errorf("panic during yaml extraction: %v", r):
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

		f, err := parser.ParseBytes(data, 0)
		if err != nil {
			select {
			case errc <- err:
			default:
			}
			return
		}

		emit := func(path string, node ast.Node) error {
			line, col := 0, 0
			if tk := node.GetToken(); tk != nil && tk.Position != nil {
				line, col = tk.Position.Line, tk.Position.Column
			}
			unit := domain.TextUnit{
				Location: yamlPathLocation{Path: path, Line: line, Column: col},
				Text:     fmt.Sprintf("%s = %s", path, node.String()),
			}
			select {
			case units <- unit:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		multiDoc := len(f.Docs) > 1
		for i, doc := range f.Docs {
			if err := ctx.Err(); err != nil {
				return
			}
			root := "."
			if multiDoc {
				root = fmt.Sprintf(".document[%d]", i)
			}
			if doc.Body == nil {
				continue
			}
			if err := walk(ctx, doc.Body, root, emit); err != nil {
				select {
				case errc <- err:
				default:
				}
				return
			}
		}
	}()

	return units, errc
}

// walk recursively visits node, invoking emit once per scalar leaf (or
// empty mapping/sequence), with path already carrying that value's
// jq/yq-style location. Anchors are transparent (walked at the same
// path as their wrapped value); aliases are treated as leaves and never
// expanded — see the package doc comment for why.
func walk(ctx context.Context, node ast.Node, path string, emit func(path string, node ast.Node) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	switch v := node.(type) {
	case *ast.MappingNode:
		if len(v.Values) == 0 {
			return emit(path, v) // empty mapping -> "<path> = {}"
		}
		for _, mv := range v.Values {
			if err := ctx.Err(); err != nil {
				return err
			}
			childPath := appendKey(path, mapKeyString(mv.Key))
			if err := walk(ctx, mv.Value, childPath, emit); err != nil {
				return err
			}
		}
		return nil

	case *ast.MappingValueNode:
		childPath := appendKey(path, mapKeyString(v.Key))
		return walk(ctx, v.Value, childPath, emit)

	case *ast.SequenceNode:
		if len(v.Values) == 0 {
			return emit(path, v) // empty sequence -> "<path> = []"
		}
		for i, val := range v.Values {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := walk(ctx, val, fmt.Sprintf("%s[%d]", path, i), emit); err != nil {
				return err
			}
		}
		return nil

	case *ast.AnchorNode:
		// Anchors are transparent: the value they annotate lives at the
		// same path, as if "&name" weren't there.
		return walk(ctx, v.Value, path, emit)

	case *ast.TagNode:
		// Tags (e.g. "!!str", custom tags) decorate a value without
		// introducing a new path level.
		return walk(ctx, v.Value, path, emit)

	default:
		// Scalar leaf: StringNode, IntegerNode, FloatNode, BoolNode,
		// NullNode, LiteralNode, AliasNode (deliberately not expanded),
		// InfinityNode, NanNode, MergeKeyNode, etc.
		return emit(path, node)
	}
}
