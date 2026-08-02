package odp

// Benchmarks and memory-shape verification for odp, additive to
// odp_test.go. Mirrors the identical pattern already established in
// docx/pptx/xlsx/text/htmldoc/odt/ods's own perf_bench_test.go files.

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

// buildOdpFixture returns a minimal but valid .odp zip with nSlides
// draw:page elements, each holding a titled shape and a body shape with
// a few paragraphs, plus speaker notes on every fifth slide.
func buildOdpFixture(tb testing.TB, nSlides int) []byte {
	tb.Helper()

	var body strings.Builder
	for i := 0; i < nSlides; i++ {
		fmt.Fprintf(&body, `<draw:page>`)
		fmt.Fprintf(&body, `<draw:frame draw:name="Title %d"><draw:text-box><text:p>Slide %d Title</text:p></draw:text-box></draw:frame>`, i, i)
		body.WriteString(`<draw:frame draw:name="Body"><draw:text-box>`)
		for p := 0; p < 5; p++ {
			fmt.Fprintf(&body, "<text:p>Bullet point %d with some filler content mentioning apple and orange.</text:p>", p)
		}
		body.WriteString(`</draw:text-box></draw:frame>`)
		if i%5 == 0 {
			fmt.Fprintf(&body, `<presentation:notes><draw:frame><draw:text-box><text:p>Speaker note for slide %d</text:p></draw:text-box></draw:frame></presentation:notes>`, i)
		}
		body.WriteString(`</draw:page>`)
	}

	content := `<?xml version="1.0" encoding="UTF-8"?><office:document-content ` + odpNS + `>` +
		`<office:body><office:presentation>` + body.String() + `</office:presentation></office:body></office:document-content>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	parts := map[string]string{
		"mimetype":              "application/vnd.oasis.opendocument.presentation",
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

// BenchmarkExtractManySlides measures a full Extract pass over a
// 2,000-slide deck with a title, five body bullets, and periodic
// speaker notes per slide.
func BenchmarkExtractManySlides(b *testing.B) {
	data := buildOdpFixture(b, 2_000)
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
// across this repo's other perf_bench_test.go files; duplicated here
// rather than shared so this file stays a self-contained,
// independently buildable addition.
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
// 1000x larger deck and asserts the large run's peak heap delta is
// nowhere near 1000x the small run's, matching the identical test in
// this repo's other streaming extractors.
func TestExtractPeakMemoryBoundedNotLinear(t *testing.T) {
	const smallSlides = 5
	const largeSlides = 5_000 // 1000x smallSlides

	smallData := buildOdpFixture(t, smallSlides)
	largeData := buildOdpFixture(t, largeSlides)

	var smallUnits, largeUnits int
	smallDelta := samplePeakLiveHeapBytes(func() { smallUnits = drainExtractBench(t, smallData) })
	largeDelta := samplePeakLiveHeapBytes(func() { largeUnits = drainExtractBench(t, largeData) })

	t.Logf("odp peak heap delta: small (%d units, %d bytes) = %d bytes, large (%d units, %d bytes) = %d bytes",
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
