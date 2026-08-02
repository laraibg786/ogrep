package jsoncdoc

import (
	"sort"

	"github.com/tailscale/hujson"
)

// comment is one "//..." or "/*...*/" comment found in a JSONC document,
// with its exact text and 1-based source position.
type comment struct {
	text      string
	line, col int
}

// collectComments walks every node hujson.Value.All() visits and pulls
// comments out of three places, chosen to cover the document exactly
// once each (verified empirically -- see the package tests):
//
//  1. Every node's own BeforeExtra: covers ordinary comments before a
//     key, value, or array element. This is where the vast majority of
//     comments live, since hujson attaches extra text to whatever comes
//     *after* it, not what came before.
//  2. The root node's own AfterExtra (checked once, not per node):
//     covers a trailing comment after the very last token in the
//     document, which by definition has no following node to claim it
//     as BeforeExtra.
//  3. Any Object or Array's own AfterExtra field (distinct from the
//     wrapping Value's AfterExtra in (2)): covers a comment inside an
//     otherwise-empty container, or a trailing comment after the last
//     member/element and before the closing brace/bracket -- neither
//     has a following sibling node to claim it as BeforeExtra either.
//
// newlineOffsets(src) is computed once here and threaded through to
// every lineCol lookup, rather than each comment re-scanning src from
// its start: on a large, comment-dense file this turned an O(n) rescan
// per comment into an O(n log n) one-time cost plus an O(log n) lookup
// each, matching the same precompute-once approach jsondoc's posTracker
// already uses for the same reason.
func collectComments(src []byte, root *hujson.Value) []comment {
	newlines := newlineOffsets(src)

	var out []comment
	isRoot := true
	for n := range root.All() {
		out = append(out, scanExtra(newlines, n.BeforeExtra, n.StartOffset-len(n.BeforeExtra))...)

		if isRoot {
			out = append(out, scanExtra(newlines, n.AfterExtra, n.EndOffset)...)
			isRoot = false
		}

		var containerExtra hujson.Extra
		switch vv := n.Value.(type) {
		case *hujson.Object:
			containerExtra = vv.AfterExtra
		case *hujson.Array:
			containerExtra = vv.AfterExtra
		}
		if len(containerExtra) > 0 {
			// This extra sits immediately before the closing '}'/']',
			// which is the byte at n.EndOffset-1.
			closeAt := n.EndOffset - 1
			out = append(out, scanExtra(newlines, containerExtra, closeAt-len(containerExtra))...)
		}
	}
	return out
}

// newlineOffsets returns the byte offset of every '\n' in src, in
// ascending order.
func newlineOffsets(src []byte) []int {
	var offs []int
	for i, b := range src {
		if b == '\n' {
			offs = append(offs, i)
		}
	}
	return offs
}

// scanExtra scans one Extra blob for "//" and "/* */" comments, returning
// each one's exact text and absolute source position. This is safe with
// a dumb byte scanner -- no JSON-string-escaping awareness needed --
// because an Extra blob is guaranteed by hujson's own grammar to contain
// only whitespace and comments, never string content; the parser has
// already resolved the string-vs-comment ambiguity before populating it.
// absStart is extra's own starting byte offset within the original
// source; newlines is that source's precomputed newline-offset list (see
// collectComments).
func scanExtra(newlines []int, extra hujson.Extra, absStart int) []comment {
	var out []comment
	b := []byte(extra)
	i := 0
	for i < len(b) {
		switch {
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '/':
			j := i
			for j < len(b) && b[j] != '\n' {
				j++
			}
			out = append(out, newComment(newlines, absStart+i, string(b[i:j])))
			i = j
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '*':
			j := i + 2
			for j+1 < len(b) && !(b[j] == '*' && b[j+1] == '/') {
				j++
			}
			end := j + 2
			if end > len(b) {
				end = len(b)
			}
			out = append(out, newComment(newlines, absStart+i, string(b[i:end])))
			i = end
		default:
			i++
		}
	}
	return out
}

func newComment(newlines []int, absOffset int, text string) comment {
	line, col := lineCol(newlines, absOffset)
	return comment{text: text, line: line, col: col}
}

// lineCol converts an absolute byte offset into a 1-based (line, column)
// via binary search over newlines, the precomputed, ascending list of
// every '\n' byte offset in the source document (see collectComments).
// Unlike jsondoc's streaming posTracker, this doesn't need to worry about
// a decoder reading ahead of its logical position -- jsoncdoc already
// holds the whole file in memory for hujson.Parse, so newlines covers
// the entire document up front.
func lineCol(newlines []int, offset int) (line, col int) {
	if offset < 0 {
		offset = 0
	}
	// i = count of newlines strictly before offset.
	i := sort.Search(len(newlines), func(i int) bool { return newlines[i] >= offset })
	lastNL := -1
	if i > 0 {
		lastNL = newlines[i-1]
	}
	return i + 1, offset - lastNL
}
