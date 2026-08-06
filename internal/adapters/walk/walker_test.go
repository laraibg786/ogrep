package walk

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func collectPaths(t *testing.T, w *Walker, root string, opts domain.SearchOptions) []string {
	t.Helper()
	paths, errc := w.Walk(context.Background(), []string{root}, opts)
	var got []string
	for p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, filepath.ToSlash(rel))
	}
	if err, ok := <-errc; ok && err != nil {
		t.Fatalf("walk error: %v", err)
	}
	sort.Strings(got)
	return got
}

func TestWalkerSkipsGitDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.txt"), "hello")
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main")
	// The walker itself has no notion of MS Office lock files -- it just
	// enumerates candidate paths -- so "~$report.docx" is expected here
	// like any other file; see internal/adapters/extract/all's
	// regression test for the actual "never recognized as a document"
	// guarantee, which lives at the registry/extractor layer instead.
	writeFile(t, filepath.Join(root, "~$report.docx"), "lock file placeholder")
	writeFile(t, filepath.Join(root, "report.docx"), "PK\x03\x04 fake docx")

	got := collectPaths(t, New(), root, domain.SearchOptions{})
	want := []string{"notes.txt", "report.docx", "~$report.docx"}
	assertPathsEqual(t, got, want)
}

func TestWalkerRespectsGitignore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitignore"), "*.log\nbuild/\n")
	writeFile(t, filepath.Join(root, "keep.txt"), "keep")
	writeFile(t, filepath.Join(root, "debug.log"), "ignored")
	writeFile(t, filepath.Join(root, "build", "output.txt"), "ignored dir contents")

	got := collectPaths(t, New(), root, domain.SearchOptions{})
	want := []string{".gitignore", "keep.txt"}
	assertPathsEqual(t, got, want)
}

func TestWalkerNoIgnoreOverride(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitignore"), "*.log\n")
	writeFile(t, filepath.Join(root, "keep.txt"), "keep")
	writeFile(t, filepath.Join(root, "debug.log"), "normally ignored")

	got := collectPaths(t, New(), root, domain.SearchOptions{NoIgnore: true})
	want := []string{".gitignore", "debug.log", "keep.txt"}
	assertPathsEqual(t, got, want)
}

func TestWalkerOgrepIgnoreIsAdditional(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitignore"), "*.log\n")
	writeFile(t, filepath.Join(root, ".ogrepignore"), "*.tmp\n")
	writeFile(t, filepath.Join(root, "keep.txt"), "keep")
	writeFile(t, filepath.Join(root, "debug.log"), "ignored via gitignore")
	writeFile(t, filepath.Join(root, "scratch.tmp"), "ignored via ogrepignore")

	got := collectPaths(t, New(), root, domain.SearchOptions{})
	want := []string{".gitignore", ".ogrepignore", "keep.txt"}
	assertPathsEqual(t, got, want)
}

func TestWalkerIncludeExcludeGlobs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "a")
	writeFile(t, filepath.Join(root, "b.md"), "b")
	writeFile(t, filepath.Join(root, "c.txt"), "c")

	got := collectPaths(t, New(), root, domain.SearchOptions{IncludeGlobs: []string{"*.txt"}})
	assertPathsEqual(t, got, []string{"a.txt", "c.txt"})

	got = collectPaths(t, New(), root, domain.SearchOptions{ExcludeGlobs: []string{"*.md"}})
	assertPathsEqual(t, got, []string{"a.txt", "c.txt"})
}

func TestWalkerExplicitFileArgumentAlwaysSearched(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitignore"), "ignored.txt\n")
	target := filepath.Join(root, "ignored.txt")
	writeFile(t, target, "content")

	w := New()
	paths, errc := w.Walk(context.Background(), []string{target}, domain.SearchOptions{})
	var got []string
	for p := range paths {
		got = append(got, p)
	}
	if err, ok := <-errc; ok && err != nil {
		t.Fatalf("walk error: %v", err)
	}
	if len(got) != 1 || got[0] != target {
		t.Fatalf("expected explicit file argument to always be searched, got %v", got)
	}
}

// TestWalkerEmptyRootsYieldsNothing regression-locks a behavior the
// orchestrator's stdin support now depends on: when every root was the
// "-" stdin pseudo-path, the orchestrator strips it out before calling
// Walk, potentially leaving an empty roots slice. Walk must degrade
// cleanly -- both channels close with nothing sent and no error --
// rather than hang or panic.
func TestWalkerEmptyRootsYieldsNothing(t *testing.T) {
	w := New()
	paths, errc := w.Walk(context.Background(), []string{}, domain.SearchOptions{})

	var got []string
	for p := range paths {
		got = append(got, p)
	}
	if len(got) != 0 {
		t.Errorf("got %d paths from empty roots, want 0", len(got))
	}
	if err, ok := <-errc; ok {
		t.Errorf("walk emitted error value %v, want a closed channel with no values", err)
	}
}

func TestWalkerNestedGitignore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "keep.txt"), "keep")
	writeFile(t, filepath.Join(root, "sub", ".gitignore"), "local.tmp\n")
	writeFile(t, filepath.Join(root, "sub", "local.tmp"), "ignored only within sub/")
	writeFile(t, filepath.Join(root, "sub", "keep2.txt"), "keep2")
	// A file with the same name outside sub/ is not affected by sub's
	// nested .gitignore.
	writeFile(t, filepath.Join(root, "local.tmp"), "not ignored at root")

	got := collectPaths(t, New(), root, domain.SearchOptions{})
	want := []string{"keep.txt", "local.tmp", "sub/.gitignore", "sub/keep2.txt"}
	assertPathsEqual(t, got, want)
}

// drainWithTimeout collects every path sent on paths and waits for errc to
// close, failing the test (rather than hanging the test binary forever)
// if that doesn't happen within timeout. This is the mechanism the task
// asks for explicitly: a bounded-timeout check via select against
// time.After, so a termination-detection bug (wrong Add/Done pairing
// closing the channel too early, or never closing it at all) shows up as
// a fast, clear test failure instead of a hung `go test` run.
func drainWithTimeout(t *testing.T, paths <-chan string, errc <-chan error, timeout time.Duration) ([]string, error) {
	t.Helper()
	deadline := time.After(timeout)
	var got []string
	var err error

	pathsOpen, errcOpen := true, true
	for pathsOpen || errcOpen {
		select {
		case p, ok := <-paths:
			if !ok {
				pathsOpen = false
				continue
			}
			got = append(got, p)
		case e, ok := <-errc:
			if !ok {
				errcOpen = false
				continue
			}
			err = e
		case <-deadline:
			t.Fatalf("Walk did not complete within %s (hung or leaked a goroutine); collected %d paths so far", timeout, len(got))
		}
	}
	return got, err
}

// TestWalkerConcurrentTerminationCollectsEveryFileAtEveryThreadCount is
// the explicit termination-detection test the task calls for: it walks a
// moderately large synthetic tree (many directories, several nesting
// levels, scattered nested ignore files -- reusing buildLargeTree from
// perf_bench_test.go) at several different worker-pool sizes, including
// deliberately tiny ones (1, 2) where the bounded job queue and the
// enqueue-or-process-inline fallback are most likely to be exercised, and
// asserts:
//
//   - Walk always finishes within a generous bounded timeout (no hang --
//     covers both "closed too early" and "never closed" failure modes,
//     since a too-early close would usually manifest as either a
//     send-on-closed-channel panic or simply fewer files than expected,
//     while never closing manifests as a hang here).
//   - Every thread count yields the exact same set of files (no dropped
//     files, no duplicates, regardless of how many workers raced to
//     process the tree).
func TestWalkerConcurrentTerminationCollectsEveryFileAtEveryThreadCount(t *testing.T) {
	const numFiles = 3000
	root := buildLargeTree(t, numFiles)

	var reference []string
	for _, threads := range []int{1, 2, 4, 8, runtime.NumCPU()} {
		w := New()
		paths, errc := w.Walk(context.Background(), []string{root}, domain.SearchOptions{Threads: threads})
		got, err := drainWithTimeout(t, paths, errc, 30*time.Second)
		if err != nil {
			t.Fatalf("threads=%d: Walk error = %v", threads, err)
		}

		sorted := append([]string(nil), got...)
		sort.Strings(sorted)

		// De-duplication check: a termination bug that double-processes
		// a directory (e.g. a job handed off AND processed inline) would
		// show up here as duplicate paths.
		for i := 1; i < len(sorted); i++ {
			if sorted[i] == sorted[i-1] {
				t.Fatalf("threads=%d: duplicate path emitted: %s", threads, sorted[i])
			}
		}

		if reference == nil {
			reference = sorted
			t.Logf("threads=%d: collected %d files (reference)", threads, len(sorted))
			continue
		}
		if len(sorted) != len(reference) {
			t.Fatalf("threads=%d: collected %d files, want %d (same as threads=1)", threads, len(sorted), len(reference))
		}
		for i := range sorted {
			if sorted[i] != reference[i] {
				t.Fatalf("threads=%d: file set differs from reference at index %d: got %q, want %q", threads, i, sorted[i], reference[i])
			}
		}
	}
}

// TestWalkerContextCancellationDoesNotHangOrLeakGoroutines covers the
// second explicit test the task calls for: starting a walk over a
// reasonably large synthetic tree, cancelling the context shortly after
// it starts (so the cancellation lands mid-traversal, with jobs still
// in flight), and then asserting both that draining the channels
// completes within a bounded timeout (no hang) and that the walker's
// goroutines actually exit (no leak), via runtime.NumGoroutine()
// settling back down close to its pre-walk baseline.
func TestWalkerContextCancellationDoesNotHangOrLeakGoroutines(t *testing.T) {
	const numFiles = 8000
	root := buildLargeTree(t, numFiles)

	runtime.GC()
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	w := New()
	paths, errc := w.Walk(ctx, []string{root}, domain.SearchOptions{Threads: runtime.NumCPU()})

	// Let the walk get underway (so jobs are genuinely in flight, not
	// cancelled before anything started) before cutting it off.
	select {
	case <-paths:
	case <-time.After(2 * time.Second):
		t.Fatal("walk did not produce any path within 2s; can't test mid-walk cancellation")
	}
	cancel()

	// Draining must complete promptly: every blocking point in the
	// walker (job-queue send/receive, output-channel send) selects on
	// ctx.Done(), so cancellation should unwind everything well within
	// this bound even for an 8000-file tree.
	got, _ := drainWithTimeout(t, paths, errc, 10*time.Second)
	t.Logf("collected %d paths before/after cancellation (tree has %d files)", len(got), numFiles)

	// Give already-exiting goroutines a moment to actually finish
	// unwinding and be reflected in runtime.NumGoroutine(), then check
	// the count has settled back down near its pre-walk baseline rather
	// than staying elevated by however many worker/closer goroutines the
	// walk spawned.
	var settled int
	for i := 0; i < 50; i++ {
		settled = runtime.NumGoroutine()
		if settled <= baseline+2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if settled > baseline+2 {
		t.Errorf("goroutine count did not settle after cancellation: baseline=%d, settled=%d (leaked walker goroutines)", baseline, settled)
	}
}

func assertPathsEqual(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
