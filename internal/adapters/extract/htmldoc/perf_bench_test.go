package htmldoc

// Benchmarks for htmldoc, additive to htmldoc_test.go.
//
// Two things are worth guarding here, both tied directly to fixes a
// review caught: (1) overall extraction throughput on a realistic
// web-page-shaped document not regressing (the fix replaced z.Token()
// with z.TagName()/z.Text() and removed a fmt.Sprintf from per-element
// path building -- both real, measured wins this benchmark exists to
// protect), and (2) the implied-end-tag fix actually keeping path depth
// (and therefore memory) bounded on ordinary, non-adversarial markup
// that omits closing tags (e.g. "<tr><td>a<tr><td>b..." -- ordinary
// generator/minifier output, not a crafted attack), which is the
// specific pattern that, before the fix, produced O(n^2) path-string
// growth on a plain, unclosed-cell table.

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildPageFixture renders a web-page-shaped HTML document: nested
// sections/paragraphs with inline markup, a style/script block, and a
// table, repeated nSections times.
func buildPageFixture(nSections int) []byte {
	var sb strings.Builder
	sb.Grow(nSections * 220)
	sb.WriteString("<!doctype html><html><head><title>Bench</title>")
	sb.WriteString("<style>body{color:red}</style><script>var x=1;</script></head><body>")
	for i := 0; i < nSections; i++ {
		fmt.Fprintf(&sb, "<div class=\"section\"><h2>Section %d</h2>", i)
		fmt.Fprintf(&sb, "<p>This is paragraph %d with <em>emphasis</em> and <a href=\"/x\">a link</a> inside it.</p>", i)
		sb.WriteString("<table><tr><td>a</td><td>b</td></tr></table>")
		sb.WriteString("</div>")
	}
	sb.WriteString("</body></html>")
	return []byte(sb.String())
}

// buildUnclosedTableFixture renders a table with nRows rows using the
// common no-explicit-closing-tag shorthand ("<tr><td>a<tr><td>b...")
// that real-world generators/minifiers emit, and that the
// implied-end-tag fix exists to handle without O(n^2) path growth.
func buildUnclosedTableFixture(nRows int) []byte {
	var sb strings.Builder
	sb.Grow(nRows * 24)
	sb.WriteString("<table>")
	for i := 0; i < nRows; i++ {
		fmt.Fprintf(&sb, "<tr><td>row %d", i)
	}
	sb.WriteString("</table>")
	return []byte(sb.String())
}

// drainExtractAll runs Extract to completion, discarding unit text, and
// returns the units emitted plus the deepest path string length seen
// (a proxy for path-depth/memory growth).
func drainExtractAll(tb testing.TB, data []byte) (n int, maxPathLen int) {
	tb.Helper()
	units, errc := (Extractor{}).Extract(context.Background(), bytes.NewReader(data), int64(len(data)))
	for u := range units {
		n++
		// Location.Human() is now just a line number (see location.go),
		// not the tag path, so the path-depth/memory proxy this helper
		// exists for must read the tag path (htmlPathLocation.Path)
		// directly instead.
		if l := len(u.Location.(htmlPathLocation).Path); l > maxPathLen {
			maxPathLen = l
		}
	}
	if err := <-errc; err != nil {
		tb.Fatalf("unexpected extraction error: %v", err)
	}
	return n, maxPathLen
}

// BenchmarkExtractPageShaped measures a full Extract pass over a
// ~realistic web-page-shaped document (nested sections, inline markup,
// a style/script block to skip, a small table), large enough that a
// reintroduced per-element allocation (e.g. z.Token() instead of
// TagName()/Text(), or fmt.Sprintf back in path building) shows up
// clearly in ns/op and B/op.
func BenchmarkExtractPageShaped(b *testing.B) {
	data := buildPageFixture(5_000)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n, _ := drainExtractAll(b, data); n == 0 {
			b.Fatal("got 0 units")
		}
	}
}

// TestImpliedCloseKeepsPathDepthBoundedAtScale is a regression test for
// the implied-end-tag fix: a 5,000-row table using the unclosed
// "<tr><td>" shorthand must NOT produce path strings that grow with row
// count. Before the fix, each unclosed row nested inside the previous
// one, so path length grew roughly linearly per row (quadratic total
// bytes across all rows); after the fix, implied closes keep rows as
// siblings, so the deepest path stays a small constant regardless of
// row count.
func TestImpliedCloseKeepsPathDepthBoundedAtScale(t *testing.T) {
	const smallRows = 50
	const largeRows = 5_000 // 100x smallRows

	_, smallMaxLen := drainExtractAll(t, buildUnclosedTableFixture(smallRows))
	_, largeMaxLen := drainExtractAll(t, buildUnclosedTableFixture(largeRows))

	t.Logf("max path length: %d rows -> %d bytes, %d rows -> %d bytes", smallRows, smallMaxLen, largeRows, largeMaxLen)

	// A sibling-shaped table's deepest path is a small constant
	// (table>tr:nth-of-type(N)>td:nth-of-type(1), roughly 40-60 bytes
	// regardless of N); a still-nesting implementation would instead
	// scale with row count (thousands of bytes at 5,000 rows). 4x the
	// small-run length, floored, cleanly separates the two.
	floor := smallMaxLen
	if floor < 64 {
		floor = 64
	}
	if largeMaxLen > 4*floor {
		t.Errorf("largest path length at %d rows = %d bytes, want <= %d (4x the %d-row baseline of %d); "+
			"this suggests unclosed table rows are nesting instead of being treated as siblings via implied closes",
			largeRows, largeMaxLen, 4*floor, smallRows, smallMaxLen)
	}
}

// samplePeakLiveHeapBytes runs fn while periodically forcing a GC and
// sampling live heap, returning the peak delta over baseline. Mirrors
// the identical helper in the text/docx packages' own
// perf_bench_test.go files (duplicated per-package by established
// convention, not shared, so each stays an independently buildable
// addition).
func samplePeakLiveHeapBytes(fn func()) int64 {
	liveHeapBytes := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}

	runtime.GC()
	runtime.GC()
	baseline := liveHeapBytes()
	peak := baseline

	stop := make(chan struct{})
	done := make(chan uint64)
	go func() {
		localPeak := baseline
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if v := liveHeapBytes(); v > localPeak {
					localPeak = v
				}
			case <-stop:
				done <- localPeak
				return
			}
		}
	}()

	fn()

	close(stop)
	if v := <-done; v > peak {
		peak = v
	}

	if peak < baseline {
		return 0
	}
	return int64(peak - baseline)
}

// TestExtractPeakMemoryBoundedNotLinear runs Extract on a ~small and a
// 1000x larger page-shaped document and asserts the large run's peak
// heap delta is nowhere near 1000x the small run's -- confirming the
// streaming, no-DOM design (see the package doc comment) really does
// keep memory bounded by nesting depth plus in-flight text, not the
// whole document. Mirrors the identical test in text/docx/pptx/xlsx.
func TestExtractPeakMemoryBoundedNotLinear(t *testing.T) {
	const smallSections = 5
	const largeSections = 5_000 // 1000x smallSections

	smallData := buildPageFixture(smallSections)
	largeData := buildPageFixture(largeSections)

	var smallUnits, largeUnits int
	smallDelta := samplePeakLiveHeapBytes(func() { smallUnits, _ = drainExtractAll(t, smallData) })
	largeDelta := samplePeakLiveHeapBytes(func() { largeUnits, _ = drainExtractAll(t, largeData) })

	t.Logf("htmldoc peak heap delta: small (%d units, %d bytes) = %d bytes, large (%d units, %d bytes) = %d bytes",
		smallUnits, len(smallData), smallDelta, largeUnits, len(largeData), largeDelta)

	const maxRatio = 50
	floor := smallDelta
	if floor < 64*1024 {
		floor = 64 * 1024
	}
	if largeDelta > int64(maxRatio)*floor {
		t.Errorf("large-doc peak heap delta (%d bytes) is more than %dx the small-doc delta (%d bytes, floored at %d); "+
			"this suggests memory use scales with document size rather than staying bounded",
			largeDelta, maxRatio, smallDelta, floor)
	}
}
