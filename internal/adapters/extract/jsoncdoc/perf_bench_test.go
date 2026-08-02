package jsoncdoc_test

// Benchmark guarding against the O(n) rescan-per-comment regression a
// review caught in lineCol: before the fix, every comment's position
// lookup rescanned the whole document from its start (bytes.Count +
// bytes.LastIndexByte), so a comment-dense file was O(n*m) in document
// size n and comment count m. The fix precomputes newline offsets once
// per document and binary-searches per comment instead. This benchmark
// exists so a future change reintroducing the rescan pattern shows up as
// a clear regression here rather than silently, since none of the
// correctness tests in jsoncdoc_test.go exercise more than a handful of
// comments per fixture.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/laraibg786/ogrep/internal/adapters/extract/jsoncdoc"
)

// buildCommentDenseJSONC returns a JSONC document with n top-level
// members, each followed by a trailing line comment -- worst case for
// lineCol's old per-comment rescan, since later comments' offsets grow
// linearly with document position.
func buildCommentDenseJSONC(n int) []byte {
	var sb strings.Builder
	sb.WriteString("{\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "  \"k%d\": %d, // comment %d\n", i, i, i)
	}
	sb.WriteString("  \"last\": true\n}\n")
	return []byte(sb.String())
}

func BenchmarkExtractCommentDense(b *testing.B) {
	data := buildCommentDenseJSONC(20000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := bytes.NewReader(data)
		units, errc := (jsoncdoc.Extractor{}).Extract(context.Background(), r, int64(len(data)))
		for range units {
		}
		if err := <-errc; err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}
