package ods

// Benchmarks and a repeated-cell/row scaling regression test for ods,
// additive to ods_test.go. Mirrors docx/pptx/xlsx/text's own
// perf_bench_test.go convention (throughput benchmark plus a bounded-
// memory-shape test), plus one test specific to this package: a review
// found that ods's repeated-cell/row handling
// (table:number-columns-repeated/table:number-rows-repeated, ODF's
// encoding for "this cell/row repeats N times", commonly a
// LibreOffice-written trailing pad reaching the spreadsheet's max
// column/row count) is genuinely O(1) per repeat group rather than
// O(repeat-count) -- this test locks that in so a future change can't
// silently reintroduce materializing the repeated range.

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

// buildOdsFixture returns a minimal but valid .ods zip with nRows rows
// on one sheet, each row holding a handful of real cells followed by a
// wide trailing table:number-columns-repeated pad -- the realistic
// LibreOffice shape a review's own profiling corpus used.
func buildOdsFixture(tb testing.TB, nRows int) []byte {
	tb.Helper()

	var body strings.Builder
	body.WriteString(`<table:table table:name="Sheet1">`)
	for i := 0; i < nRows; i++ {
		fmt.Fprintf(&body, "<table:table-row><table:table-cell><text:p>row %d</text:p></table:table-cell>", i)
		body.WriteString(`<table:table-cell table:number-columns-repeated="16372"/></table:table-row>`)
	}
	body.WriteString(`</table:table>`)

	content := `<?xml version="1.0" encoding="UTF-8"?><office:document-content ` + odsNS + `>` +
		`<office:body><office:spreadsheet>` + body.String() + `</office:spreadsheet></office:body></office:document-content>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	parts := map[string]string{
		"mimetype":              "application/vnd.oasis.opendocument.spreadsheet",
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

// BenchmarkExtractRowsWithColumnPad measures a full Extract pass over
// 2,000 rows, each with one real cell followed by a 16,372-wide
// repeated-blank-cell pad (LibreOffice commonly pads to its max column
// count) -- large enough that a regression turning the O(1) repeat
// handling into an O(repeat-count) one would make this benchmark's
// ns/op and B/op explode, not just drift.
func BenchmarkExtractRowsWithColumnPad(b *testing.B) {
	data := buildOdsFixture(b, 2_000)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n := drainExtractBench(b, data); n != 2_000 {
			b.Fatalf("got %d units, want 2000", n)
		}
	}
}

// TestRepeatedCellsAreConstantTimeNotProportionalToRepeatCount is a
// regression test locking in that blank repeated cells/rows are handled
// in O(1) per repeat group, not O(repeat-count): a spreadsheet built
// entirely from maximum-size repeat groups (LibreOffice's own column/row
// ceiling) addresses an enormous number of logical cells from a tiny
// document. An O(repeat-count) implementation would need on the order
// of columnCap*rowCap*sheets individual steps here and would time out
// or take orders of magnitude longer; the real implementation should
// finish near-instantly.
func TestRepeatedCellsAreConstantTimeNotProportionalToRepeatCount(t *testing.T) {
	const columnCap = 16384
	const rowCap = 1048576
	const sheets = 20

	var body strings.Builder
	for s := 0; s < sheets; s++ {
		fmt.Fprintf(&body, `<table:table table:name="Sheet%d">`, s)
		body.WriteString(`<table:table-row table:number-rows-repeated="` + fmt.Sprint(rowCap) + `">`)
		body.WriteString(`<table:table-cell table:number-columns-repeated="` + fmt.Sprint(columnCap) + `"/>`)
		body.WriteString(`</table:table-row></table:table>`)
	}

	content := `<?xml version="1.0" encoding="UTF-8"?><office:document-content ` + odsNS + `>` +
		`<office:body><office:spreadsheet>` + body.String() + `</office:spreadsheet></office:body></office:document-content>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	parts := map[string]string{
		"mimetype":              "application/vnd.oasis.opendocument.spreadsheet",
		"META-INF/manifest.xml": `<?xml version="1.0" encoding="UTF-8"?><manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0"></manifest:manifest>`,
		"content.xml":           content,
	}
	for name, data := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("creating zip part %s: %v", name, err)
		}
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatalf("writing zip part %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip writer: %v", err)
	}
	data := buf.Bytes()

	logicalCells := int64(columnCap) * int64(rowCap) * int64(sheets)

	done := make(chan int)
	go func() { done <- drainExtractBench(t, data) }()

	select {
	case n := <-done:
		// All cells in every repeat group are blank, so nothing is
		// actually emitted -- the point is that reaching "0 units" for
		// ~340 trillion logically-addressed cells happens fast.
		if n != 0 {
			t.Errorf("got %d units, want 0 (all cells in this fixture are blank)", n)
		}
		t.Logf("addressed %d logical cells (%d sheets x %d rows x %d cols) from a %d-byte file with 0 units emitted",
			logicalCells, sheets, rowCap, columnCap, len(data))
	case <-time.After(5 * time.Second):
		t.Fatalf("extraction did not finish within 5s -- this suggests repeated cells/rows are being "+
			"materialized proportional to their repeat count (%d logical cells addressed) rather than "+
			"handled in O(1) per repeat group", logicalCells)
	}
}

// samplePeakLiveHeapBytes mirrors the identical helper duplicated
// across docx/pptx/xlsx/text/htmldoc/odt's own perf_bench_test.go
// files; duplicated here rather than shared so this file stays a
// self-contained, independently buildable addition.
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
// nowhere near 1000x the small run's, matching the identical test in
// docx/pptx/xlsx/text/htmldoc/odt.
func TestExtractPeakMemoryBoundedNotLinear(t *testing.T) {
	const smallRows = 10
	const largeRows = 10_000 // 1000x smallRows

	smallData := buildOdsFixture(t, smallRows)
	largeData := buildOdsFixture(t, largeRows)

	var smallUnits, largeUnits int
	smallDelta := samplePeakLiveHeapBytes(func() { smallUnits = drainExtractBench(t, smallData) })
	largeDelta := samplePeakLiveHeapBytes(func() { largeUnits = drainExtractBench(t, largeData) })

	t.Logf("ods peak heap delta: small (%d units, %d bytes) = %d bytes, large (%d units, %d bytes) = %d bytes",
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
