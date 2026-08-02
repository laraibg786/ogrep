package tomldoc

// Benchmarks for tomldoc, additive to tomldoc_test.go (which shares this
// package, not an external _test package, so these can exercise
// isBareIdent directly). Unlike the streaming extractors
// (text/docx/pptx/xlsx), tomldoc parses the whole file into memory via
// unstable.Parser (see the package doc comment for why), so a "peak
// memory stays bounded as file size grows" test would fail by design and
// isn't included here -- that non-streaming design is intentional, not a
// regression to guard against.
//
// What IS worth guarding here:
//
//   - isBareIdent (a hand-rolled replacement for a regexp.MatchString
//     call a review found was this package's single largest
//     self-inflicted CPU cost, run once per table key across the whole
//     document) actually being cheap at realistic key-density.
//   - Overall extraction throughput on a config-file-shaped document not
//     regressing silently -- in particular, BenchmarkExtractKeyDense is
//     what originally caught a real O(n²) regression during this
//     package's rewrite onto go-toml/v2/unstable: calling
//     unstable.Parser.Shape once per node looked like an O(1) position
//     lookup from its signature, but reading parser.go's position()
//     showed it rescans the document from byte 0 on every call, so
//     20,000 Shape calls actually cost O(n²). Confirmed here empirically
//     (2.6s/op before the fix, ~9ms/op after switching to this package's
//     own binary-search posAt over a precomputed line-start index --
//     both numbers from real `go test -bench` runs, not estimated), and
//     is now the reason posAt exists rather than a Shape call in the hot
//     per-node path (see tomldoc.go's posAt doc comment).
//   - maxSniffSize's 8 MiB ceiling still being an honest one for this
//     parser: BenchmarkExtractAtSizeCap measures a real document at that
//     exact size.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// buildKeyDenseTOML returns a TOML document with n top-level scalar
// keys plus a nested table and an array of tables, sized to exercise
// jqSegment/isBareIdent across a realistic mix of bare and
// non-bare-identifier keys (the latter forcing the bracketed-quote path
// through jsonQuote too).
func buildKeyDenseTOML(n int) []byte {
	var sb strings.Builder
	sb.Grow(n * 32)
	for i := 0; i < n; i++ {
		if i%5 == 0 {
			// Non-bare identifier: forces the bracketed jq path and a
			// jsonQuote call, not just isBareIdent's fast accept.
			fmt.Fprintf(&sb, "\"key-%d\" = %d\n", i, i)
		} else {
			fmt.Fprintf(&sb, "key_%d = %d\n", i, i)
		}
	}
	sb.WriteString("\n[nested.table]\n")
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&sb, "field_%d = \"value number %d\"\n", i, i)
	}
	sb.WriteString("\n[[servers]]\nname = \"alpha\"\nport = 8080\n")
	sb.WriteString("\n[[servers]]\nname = \"beta\"\nport = 8081\n")
	return []byte(sb.String())
}

// drainExtract runs Extract to completion, discarding unit text, and
// returns the number of units emitted.
func drainExtract(tb testing.TB, data []byte) int {
	tb.Helper()
	r := bytes.NewReader(data)
	units, errc := (Extractor{}).Extract(context.Background(), r, int64(len(data)))
	n := 0
	for range units {
		n++
	}
	if err := <-errc; err != nil {
		tb.Fatalf("unexpected extraction error: %v", err)
	}
	return n
}

// BenchmarkExtractKeyDense measures a full Extract pass over a
// document with 20,000 top-level keys (a fifth of them non-bare
// identifiers) plus a nested table and an array of tables -- large
// enough that a reintroduced O(n) or per-key regexp-call regression in
// the key-formatting path would show up clearly in ns/op and B/op.
func BenchmarkExtractKeyDense(b *testing.B) {
	data := buildKeyDenseTOML(20_000)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n := drainExtract(b, data); n < 20_000 {
			b.Fatalf("got %d units, want at least 20000", n)
		}
	}
}

// buildDocumentOfSize returns a synthetic, key-dense TOML document
// exactly targetSize bytes long, built the same way buildKeyDenseTOML is
// (a run of "key_N = N" lines, padded with a trailing comment line to
// land on the exact byte count), for BenchmarkExtractAtSizeCap to
// measure realistic full-document parse time at exactly maxSniffSize --
// not a few bytes over it, which would silently make the benchmark
// measure a document one byte too large to actually pass Sniff.
func buildDocumentOfSize(targetSize int) []byte {
	var sb strings.Builder
	sb.Grow(targetSize)
	for i := 0; ; i++ {
		line := fmt.Sprintf("key_%d = %d\n", i, i)
		if sb.Len()+len(line) > targetSize {
			break
		}
		sb.WriteString(line)
	}
	switch remaining := targetSize - sb.Len(); {
	case remaining == 1:
		sb.WriteByte('\n')
	case remaining >= 2:
		sb.WriteByte('#')
		sb.WriteString(strings.Repeat(" ", remaining-2))
		sb.WriteByte('\n')
	}
	return []byte(sb.String())
}

// BenchmarkExtractAtSizeCap measures a full Extract pass over a document
// at exactly maxSniffSize (8 MiB): the real-world question maxSniffSize's
// doc comment answers ("does this still cost ~10s the way toml.Decode
// did, or is go-toml/v2/unstable meaningfully faster"). Run with
// `go test -bench=AtSizeCap -benchtime=1x` for a single real
// measurement rather than the default's amortized loop.
func BenchmarkExtractAtSizeCap(b *testing.B) {
	data := buildDocumentOfSize(maxSniffSize)
	if len(data) != maxSniffSize {
		b.Fatalf("fixture size = %d, want exactly %d", len(data), maxSniffSize)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainExtract(b, data)
	}
}

// BenchmarkIsBareIdent isolates the hand-rolled identifier check itself
// (see its doc comment for the regexp.MatchString regression it
// replaces), across both accepting and rejecting inputs, so a future
// change to this function that reintroduces per-call allocation or a
// slower character-class check shows up here directly.
func BenchmarkIsBareIdent(b *testing.B) {
	cases := []string{"key_name", "a", "foo-bar with spaces and 日本語", "_leading_underscore_123"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, c := range cases {
			_ = isBareIdent(c)
		}
	}
}
