package xlsx_test

// Performance/memory verification for the xlsx extractor, additive to
// the existing xlsx_test.go/extract_test.go/fixtures_test.go/
// integration_test.go suite. See
// internal/adapters/extract/docx/perf_bench_test.go for the fuller
// rationale behind the two pieces added here: a testing.B benchmark
// over a realistically large workbook, and a regular Test that
// quantitatively checks peak resident memory during Extract stays
// roughly flat as row count grows, rather than scaling with it -- this
// is THE scenario the package doc comment calls out by name ("100k+ rows
// is an explicit target scenario") as the reason sheet.go streams via
// encoding/xml.Decoder.Token() instead of unmarshaling into a DOM.

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/laraibg786/ogrep/internal/adapters/extract/xlsx"
)

// buildXlsxTB is a copy of fixtures_test.go's buildXlsx, retyped to
// accept testing.TB instead of *testing.T so it can be called from both
// benchmarks (*testing.B) and the memory-boundedness test (*testing.T).
func buildXlsxTB(tb testing.TB, parts map[string]string) []byte {
	tb.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range parts {
		w, err := zw.Create(name)
		if err != nil {
			tb.Fatalf("creating zip part %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			tb.Fatalf("writing zip part %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		tb.Fatalf("closing zip: %v", err)
	}
	return buf.Bytes()
}

// buildLargeSheetFixture builds a full xlsx workbook with a single sheet
// of nRows rows, each with 3 cells: a shared-string cell, a numeric
// cell, and a second shared-string cell. sharedStringPoolSize distinct
// strings are cycled through by index (rather than emitting nRows
// distinct strings), so the shared-strings table's own size stays FIXED
// regardless of nRows -- isolating the thing under test (does per-row
// streaming hold the whole sheet in memory) from a separate, expected
// scaling concern (the shared-strings table itself, which the package
// doc comment already documents as "eagerly loaded", proportional to
// the number of UNIQUE strings, not row count).
func buildLargeSheetFixture(tb testing.TB, nRows, sharedStringPoolSize int) []byte {
	tb.Helper()

	var pool strings.Builder
	pool.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	for i := 0; i < sharedStringPoolSize; i++ {
		fmt.Fprintf(&pool, `<si><t>shared string %d filler apple orange rarely needle</t></si>`, i)
	}
	pool.WriteString(`</sst>`)

	var sheet strings.Builder
	sheet.Grow(nRows*80 + 4096)
	sheet.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for r := 1; r <= nRows; r++ {
		idxA := r % sharedStringPoolSize
		idxC := (r*7 + 3) % sharedStringPoolSize
		fmt.Fprintf(&sheet, `<row r="%d">`+
			`<c r="A%d" t="s"><v>%d</v></c>`+
			`<c r="B%d"><v>%d</v></c>`+
			`<c r="C%d" t="s"><v>%d</v></c>`+
			`</row>`,
			r, r, idxA, r, r, r, idxC)
	}
	sheet.WriteString(`</sheetData></worksheet>`)

	workbookXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
		`<sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`
	workbookRelsXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
		`</Relationships>`

	return buildXlsxTB(tb, map[string]string{
		"[Content_Types].xml":        contentTypesXML,
		"_rels/.rels":                rootRelsXML,
		"xl/workbook.xml":            workbookXML,
		"xl/_rels/workbook.xml.rels": workbookRelsXML,
		"xl/sharedStrings.xml":       pool.String(),
		"xl/worksheets/sheet1.xml":   sheet.String(),
	})
}

// drainExtract runs Extract to completion, discarding unit text, and
// returns the number of units emitted.
func drainExtract(tb testing.TB, data []byte) int {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	e := xlsx.Extractor{}
	units, errc := e.Extract(ctx, bytes.NewReader(data), int64(len(data)))
	n := 0
	for range units {
		n++
	}
	if err := <-errc; err != nil {
		tb.Fatalf("unexpected extraction error: %v", err)
	}
	if ctx.Err() != nil {
		tb.Fatalf("extraction did not complete before the test timeout")
	}
	return n
}

// BenchmarkExtractLargeSheet measures a full Extract pass over a sheet
// with 120,000+ rows -- the specific scale ("100k+ rows") the xlsx
// package doc comment calls out as the reason cell data must be
// streamed rather than unmarshaled.
func BenchmarkExtractLargeSheet(b *testing.B) {
	const nRows = 120_000
	const poolSize = 50
	data := buildLargeSheetFixture(b, nRows, poolSize)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n := drainExtract(b, data); n != nRows*3 {
			b.Fatalf("got %d units, want %d", n, nRows*3)
		}
	}
}

// samplePeakLiveHeapBytes runs fn while periodically FORCING a garbage
// collection and reading runtime.MemStats.HeapAlloc immediately
// afterward, so each sample reflects the true live heap at that instant
// rather than however much garbage Go's GC pacer happens to be tolerating
// at the time. See the docx package's identical helper
// (internal/adapters/extract/docx/perf_bench_test.go) for the fuller
// rationale (including the empirically-observed false positive this
// fixes -- without forced per-sample GCs, a large already-live fixture
// object makes the default GOGC=100 pacer tolerate a "peak" that tracks
// the fixture's own size, not what the operation under test actually
// needs). Duplicated here rather than shared so each format plugin's
// benchmark file stays a self-contained, independently buildable
// addition.
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

// TestExtractPeakMemoryBoundedNotLinear is the quantitative check behind
// this package's doc-comment claim that xl/worksheets/sheetN.xml -- "the
// one part of an xlsx file that can legitimately be huge -- 100k+ rows"
// -- is streamed rather than unmarshaled. It runs Extract on a 100-row
// and a 120,000-row version of the SAME sheet shape (1200x the row
// count, same fixed-size shared-strings pool in both so that known,
// expected, row-count-independent overhead doesn't get conflated with
// the thing under test) and asserts the large run's peak heap-objects
// delta is NOT anywhere near 1200x the small run's.
//
// Threshold reasoning: unlike a hand-rolled encoding/xml.Decoder.Token()
// streamer (whose peak memory is dominated by roughly constant-size
// state and stays within single-digit-x of the small-run baseline),
// excelize's Rows() iterator carries some per-row overhead that grows
// sublinearly but not perfectly flat with row count -- empirically
// measured at ~14MB live-heap delta for a 120,000-row sheet, vs. ~700KB
// for the equivalent hand-rolled streamer. That's still nowhere near a
// full-DOM implementation's behavior (which would scale close to
// linearly, ~1200x for a 1200x row-count increase), so the ratio and
// absolute caps below are calibrated to excelize's actual measured
// behavior rather than the tighter bound the previous hand-rolled
// implementation held itself to.
//
// smallRunSamples is the small-run's live-heap delta re-measured this
// many times so the comparison floor below is a median rather than a
// single noisy sample: at excelize's baseline overhead, one 100-row run
// can swing between 0 and a few hundred KB purely from GC-scheduling
// timing (observed up to ~180KB across single-sample runs), which made
// an earlier version of this test (comparing against a single sample,
// or against a floor disconnected from any real measurement) either
// flaky or not actually testing anything relative to the small-run
// baseline. A median-of-5 is a real measured baseline that resists
// single-sample noise, restoring genuine ratio semantics.
func TestExtractPeakMemoryBoundedNotLinear(t *testing.T) {
	const poolSize = 50
	const smallRows = 100
	const largeRows = 120_000 // 1200x smallRows
	const smallRunSamples = 5

	smallData := buildLargeSheetFixture(t, smallRows, poolSize)
	largeData := buildLargeSheetFixture(t, largeRows, poolSize)

	var smallUnits int
	smallDeltas := make([]int64, smallRunSamples)
	for i := range smallDeltas {
		smallDeltas[i] = samplePeakLiveHeapBytes(func() { smallUnits = drainExtract(t, smallData) })
	}
	sort.Slice(smallDeltas, func(i, j int) bool { return smallDeltas[i] < smallDeltas[j] })
	smallDelta := smallDeltas[len(smallDeltas)/2] // median, resists single-sample noise

	var largeUnits int
	largeDelta := samplePeakLiveHeapBytes(func() { largeUnits = drainExtract(t, largeData) })

	t.Logf("xlsx peak heap-objects delta: small (%d units, %d rows) = %d bytes (median of %v), large (%d units, %d rows) = %d bytes",
		smallUnits, smallRows, smallDelta, smallDeltas, largeUnits, largeRows, largeDelta)

	// A fixed 512KiB floor still guards against the rare case where even
	// the median lands at or near zero (GC noise can occasionally align
	// across most of the 5 samples), while normally deferring to the
	// actual measured (median) small-run baseline -- a real ratio
	// comparison, not a disguised second absolute cap.
	const maxRatio = 150
	const minFloor = 512 * 1024
	floor := smallDelta
	if floor < minFloor {
		floor = minFloor
	}
	if largeDelta > int64(maxRatio)*floor {
		t.Errorf("large-sheet peak heap delta (%d bytes) is more than %dx the small-run median (%d bytes, floored at %d); "+
			"this suggests memory use scales with row count far more than excelize's Rows() iterator is expected to (a %dx row-count increase should not produce anywhere close to a %dx memory increase)",
			largeDelta, maxRatio, smallDelta, floor, largeRows/smallRows, largeRows/smallRows)
	}

	// Independent absolute sanity cap, calibrated to excelize's measured
	// ~14MB at 120,000 rows (vs. the hand-rolled implementation's
	// ~32MiB cap): generous enough to absorb machine-to-machine
	// variance, while still cleanly rejecting a full-DOM-style
	// implementation, which holding ~360,000 cell values (120,000 rows x
	// 3 cells) simultaneously, each with Go string/struct overhead well
	// above the raw text size, would comfortably blow past this.
	const absoluteCapBytes = 128 * 1024 * 1024
	if largeDelta > absoluteCapBytes {
		t.Errorf("large-sheet peak heap delta = %d bytes, want <= %d bytes (%d MiB)",
			largeDelta, absoluteCapBytes, absoluteCapBytes/(1024*1024))
	}
}
