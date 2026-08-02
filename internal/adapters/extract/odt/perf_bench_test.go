package odt

// Benchmarks and memory-shape verification for odt, additive to
// odt_test.go. Mirrors the identical pattern already established in
// docx/pptx/xlsx/text's own perf_bench_test.go files: a throughput
// benchmark over a realistically large document, and a test confirming
// peak memory stays roughly flat as document size grows rather than
// scaling with it -- since this package's whole design premise (see
// the package doc comment) is streaming via encoding/xml's token API,
// never building a full in-memory DOM.

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildOdtFixture returns a minimal but valid .odt zip containing
// nParagraphs paragraphs, a heading every 25 paragraphs (exercising the
// nearest-heading tracking on every emitted unit) and a small table
// every 200 paragraphs (exercising cell/row counting).
func buildOdtFixture(tb testing.TB, nParagraphs int) []byte {
	tb.Helper()

	var body strings.Builder
	for i := 0; i < nParagraphs; i++ {
		if i%25 == 0 {
			fmt.Fprintf(&body, "<text:h text:outline-level=\"1\">Section %d</text:h>", i/25)
		}
		fmt.Fprintf(&body, "<text:p>Paragraph %d with some filler words, mentioning apple and orange but rarely the needle.</text:p>", i)
		if i%200 == 0 {
			body.WriteString("<table:table><table:table-row>" +
				"<table:table-cell><text:p>cell a</text:p></table:table-cell>" +
				"<table:table-cell><text:p>cell b</text:p></table:table-cell>" +
				"</table:table-row></table:table>")
		}
	}

	content := `<?xml version="1.0" encoding="UTF-8"?><office:document-content ` + odtNS + `>` +
		`<office:body><office:text>` + body.String() + `</office:text></office:body></office:document-content>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	parts := map[string]string{
		"mimetype":              "application/vnd.oasis.opendocument.text",
		"META-INF/manifest.xml": `<?xml version="1.0" encoding="UTF-8"?><manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0"></manifest:manifest>`,
		"content.xml":           content,
	}
	for name, data := range parts {
		w, err := zw.Create(name)
		if err != nil {
			tb.Fatalf("creating zip part %s: %v", name, err)
		}
		if _, err := w.Write([]byte(data)); err != nil {
			tb.Fatalf("writing zip part %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		tb.Fatalf("closing zip writer: %v", err)
	}
	return buf.Bytes()
}

func drainExtractBench(tb testing.TB, data []byte) int {
	tb.Helper()
	units, errc := (Extractor{}).Extract(context.Background(), bytes.NewReader(data), int64(len(data)))
	n := 0
	for range units {
		n++
	}
	if err := <-errc; err != nil {
		tb.Fatalf("unexpected extraction error: %v", err)
	}
	return n
}

// BenchmarkExtractLargeDocument measures a full Extract pass over a
// ~10,000-paragraph .odt document with headings and tables interspersed.
func BenchmarkExtractLargeDocument(b *testing.B) {
	data := buildOdtFixture(b, 10_000)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n := drainExtractBench(b, data); n == 0 {
			b.Fatal("got 0 units")
		}
	}
}

// samplePeakLiveHeapBytes mirrors the identical helper duplicated
// across docx/pptx/xlsx/text/htmldoc's own perf_bench_test.go files
// (see any of those for the fuller rationale); duplicated here rather
// than shared so this file stays a self-contained, independently
// buildable addition.
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

// TestExtractPeakMemoryBoundedNotLinear runs Extract on a small and a
// 1000x larger document and asserts the large run's peak heap delta is
// nowhere near 1000x the small run's -- confirming the streaming,
// no-DOM design really does keep memory bounded, matching the identical
// test in docx/pptx/xlsx/text/htmldoc.
func TestExtractPeakMemoryBoundedNotLinear(t *testing.T) {
	const smallParagraphs = 10
	const largeParagraphs = 10_000 // 1000x smallParagraphs

	smallData := buildOdtFixture(t, smallParagraphs)
	largeData := buildOdtFixture(t, largeParagraphs)

	var smallUnits, largeUnits int
	smallDelta := samplePeakLiveHeapBytes(func() { smallUnits = drainExtractBench(t, smallData) })
	largeDelta := samplePeakLiveHeapBytes(func() { largeUnits = drainExtractBench(t, largeData) })

	t.Logf("odt peak heap delta: small (%d units, %d bytes) = %d bytes, large (%d units, %d bytes) = %d bytes",
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
