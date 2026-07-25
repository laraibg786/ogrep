package app_test

// End-to-end performance verification for SearchOrchestrator.Run,
// additive to the existing orchestrator_test.go suite. It reuses that
// file's fakeSink, fakeLookup, and writeFixture helpers (same package,
// app_test) rather than duplicating them.
//
// Extractor choice: this benchmark deliberately uses a SYNTHETIC fake
// extractor (workExtractor below), not the real docx/pptx/xlsx/text
// plugins from internal/adapters/extract/*. Importing all four adapter
// packages here would compile fine -- internal/core/app itself has no
// import of them, so there's no literal import cycle -- but it would
// still be a layering violation of this project's hexagonal
// architecture: the core use-case package (internal/core/app) has no
// business depending on adapters at all, even from a _test.go file, when
// a fake implementing the ports.DocumentExtractor contract does the job.
// (orchestrator_test.go itself already imports the real text/match/walk
// adapters for its own integration-style tests, which is an established
// precedent for external test packages; this file only goes one step
// further by preferring a synthetic fake specifically for the
// throughput-scaling question below, where controlling the exact amount
// of per-unit work matters more than exercising a real format parser.)
//
// The benchmark's actual question: does wall-clock meaningfully improve
// as Threads increases, or does something (e.g. the writeMu-guarded
// write-out) serialize work unexpectedly? To make that a fair test, the
// fake extractor does a deliberately-sized amount of artificial CPU work
// per unit (busyWork), so file-processing time dominates over the
// output-writing critical section -- otherwise, with near-instant fake
// extraction, the benchmark would mostly measure scheduling/mutex
// overhead rather than the realistic "CPU-bound extraction, occasional
// write-out" shape production traffic actually has.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/laraibg786/ogrep/internal/adapters/match"
	"github.com/laraibg786/ogrep/internal/adapters/walk"
	"github.com/laraibg786/ogrep/internal/core/app"
	"github.com/laraibg786/ogrep/internal/core/domain"
)

// busyWork spends roughly n iterations of trivial integer arithmetic,
// standing in for the CPU cost a real extractor would spend parsing XML
// tokens for one unit of text.
func busyWork(n int) int {
	x := 0
	for i := 0; i < n; i++ {
		x += i * (i % 7)
	}
	return x
}

// fakeLineLocation is a minimal domain.Location for workExtractor's
// synthetic units.
type fakeLineLocation struct {
	line int
}

func (l fakeLineLocation) Human() string          { return fmt.Sprintf("line %d", l.line) }
func (l fakeLineLocation) Fields() map[string]any { return map[string]any{"line": l.line} }
func (l fakeLineLocation) HyperlinkURI(path string) string {
	return fmt.Sprintf("%s:%d:1", domain.FileURI(path, ""), l.line)
}

// workExtractor is a ports.DocumentExtractor whose Extract emits
// unitsPerFile TextUnits, each preceded by a small busyWork call so the
// benchmark's total CPU cost is controllable independent of real
// document parsing. Every 5th unit's text contains "needle" so a
// realistic minority of units actually match and flow through the
// orchestrator's write-out path.
type workExtractor struct {
	unitsPerFile int
	workPerUnit  int
}

func (workExtractor) Name() string { return "fake" }

func (workExtractor) Sniff(path string, ra io.ReaderAt, size int64) bool { return true }

func (e workExtractor) Extract(ctx context.Context, ra io.ReaderAt, size int64) (<-chan domain.TextUnit, <-chan error) {
	units := make(chan domain.TextUnit)
	errc := make(chan error, 1)

	go func() {
		defer close(units)
		defer close(errc)
		// Mirrors the panic-safety contract every real extractor's own
		// Extract goroutine must follow (see ports.DocumentExtractor);
		// not expected to trigger here, but keeping the fake honest to
		// the contract avoids this benchmark accidentally relying on
		// behavior real plugins don't get to rely on.
		defer func() {
			if r := recover(); r != nil {
				select {
				case errc <- fmt.Errorf("panic during fake extraction: %v", r):
				default:
				}
			}
		}()

		for i := 0; i < e.unitsPerFile; i++ {
			busyWork(e.workPerUnit)

			text := fmt.Sprintf("unit %d filler words apple orange", i)
			if i%5 == 0 {
				text += " needle"
			}
			u := domain.TextUnit{
				Location: fakeLineLocation{line: i},
				Text:     text,
			}
			select {
			case units <- u:
			case <-ctx.Done():
				return
			}
		}
	}()

	return units, errc
}

// buildOrchestratorCorpus writes nFiles small placeholder files to disk
// (their content is irrelevant, since fakeLookup always resolves to
// workExtractor regardless of path/contents) for the real walk.New()
// walker to enumerate. Unlike orchestrator_test.go's writeFixture (which
// is typed against the concrete *testing.T), this accepts testing.TB so
// it can also be called from a *testing.B.
func buildOrchestratorCorpus(tb testing.TB, nFiles int) string {
	tb.Helper()
	dir := tb.TempDir()
	for i := 0; i < nFiles; i++ {
		path := filepath.Join(dir, fmt.Sprintf("doc%d.fake", i))
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			tb.Fatalf("writing fixture %s: %v", path, err)
		}
	}
	return dir
}

// BenchmarkOrchestratorThreadScaling runs a full SearchOrchestrator.Run
// over a synthetic corpus at Threads = 1, 2, and runtime.NumCPU(),
// reporting each configuration's ns/op so the scaling behavior can be
// read directly off the benchmark output: if per-op time doesn't drop
// substantially from threads=1 to threads=runtime.NumCPU(), that's a
// signal something (e.g. writeMu, or per-file setup cost) is serializing
// work that should otherwise parallelize across the worker pool.
func BenchmarkOrchestratorThreadScaling(b *testing.B) {
	const nFiles = 200
	const unitsPerFile = 10
	const workPerUnit = 150_000 // tuned so file-processing dominates write-out cost

	dir := buildOrchestratorCorpus(b, nFiles)
	extractor := workExtractor{unitsPerFile: unitsPerFile, workPerUnit: workPerUnit}

	threadCounts := []int{1, 2, runtime.NumCPU()}
	for _, threads := range threadCounts {
		threads := threads
		b.Run(fmt.Sprintf("threads=%d", threads), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sink := newFakeSink()
				orch := app.New(fakeLookup{extractor: extractor}, walk.New(), match.NewFactory(), sink)

				stats, err := orch.Run(context.Background(), "needle", []string{dir}, domain.SearchOptions{Threads: threads})
				if err != nil {
					b.Fatalf("Run() error = %v", err)
				}
				if stats.FilesSearched != nFiles {
					b.Fatalf("FilesSearched = %d, want %d", stats.FilesSearched, nFiles)
				}
			}
		})
	}
}
