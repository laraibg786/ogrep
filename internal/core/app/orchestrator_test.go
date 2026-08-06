package app_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laraibg786/ogrep/internal/adapters/extract/text"
	"github.com/laraibg786/ogrep/internal/adapters/match"
	"github.com/laraibg786/ogrep/internal/adapters/walk"
	"github.com/laraibg786/ogrep/internal/core/app"
	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/core/ports"
	"github.com/laraibg786/ogrep/internal/registry"
)

// fakeSink collects matches in memory so tests can assert on them
// without depending on the terminal/json adapters.
type fakeSink struct {
	mu      sync.Mutex
	matches []domain.Match
	summary map[string]int

	// onWriteMatch, if set, is invoked synchronously (while s.mu is
	// held) on every WriteMatch call, letting a test observe/react to
	// writes as they happen rather than only after Run returns -- used
	// to verify matches are streamed to the sink as they're found, not
	// only after a whole file has been fully processed.
	onWriteMatch func(domain.Match)
}

func newFakeSink() *fakeSink {
	return &fakeSink{summary: make(map[string]int)}
}

func (s *fakeSink) WriteMatch(m domain.Match) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.matches = append(s.matches, m)
	if s.onWriteMatch != nil {
		s.onWriteMatch(m)
	}
	return nil
}

func (s *fakeSink) WriteFileSummary(path string, count int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summary[path] = count
	return nil
}

func (s *fakeSink) Flush() error { return nil }

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTestRegistry() *registry.Registry {
	r := registry.New()
	r.Register(text.Extractor{})
	return r
}

func TestOrchestratorEndToEndSearch(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "a.txt"), "hello world\nfoo bar\nHELLO again\n")
	writeFixture(t, filepath.Join(dir, "b.txt"), "nothing to see here\nfoo hello foo\n")
	writeFixture(t, filepath.Join(dir, "sub", "c.txt"), "hello from a subdirectory\n")

	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "hello", []string{dir}, domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if stats.TotalMatches != 3 {
		t.Errorf("TotalMatches = %d, want 3 (case-sensitive: a.txt line1, b.txt line2, sub/c.txt line1)", stats.TotalMatches)
	}
	if stats.FilesMatched != 3 {
		t.Errorf("FilesMatched = %d, want 3", stats.FilesMatched)
	}

	var gotPaths []string
	for _, m := range sink.matches {
		gotPaths = append(gotPaths, filepath.Base(m.Path))
		if m.Format != "text" {
			t.Errorf("match format = %q, want %q", m.Format, "text")
		}
		if m.Path == "" {
			t.Error("expected orchestrator to fill in Match.Path")
		}
	}
	sort.Strings(gotPaths)
	want := []string{"a.txt", "b.txt", "c.txt"}
	if len(gotPaths) != len(want) {
		t.Fatalf("got paths %v, want %v", gotPaths, want)
	}
	for i := range want {
		if gotPaths[i] != want[i] {
			t.Fatalf("got paths %v, want %v", gotPaths, want)
		}
	}
}

func TestOrchestratorIgnoreCase(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "a.txt"), "hello\nHELLO\nHeLLo\nnope\n")

	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "hello", []string{dir}, domain.SearchOptions{IgnoreCase: true})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.TotalMatches != 3 {
		t.Errorf("TotalMatches = %d, want 3", stats.TotalMatches)
	}
}

func TestOrchestratorNoMatches(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "a.txt"), "nothing relevant here\n")

	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "zzz-not-found", []string{dir}, domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.TotalMatches != 0 || stats.FilesMatched != 0 {
		t.Errorf("expected no matches, got %+v", stats)
	}
	if stats.FilesSearched != 1 {
		t.Errorf("FilesSearched = %d, want 1", stats.FilesSearched)
	}
}

func TestOrchestratorInvertMatch(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "a.txt"), "hello\nworld\nhello again\n")

	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "hello", []string{dir}, domain.SearchOptions{InvertMatch: true})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.TotalMatches != 1 {
		t.Errorf("TotalMatches = %d, want 1 (only the 'world' line doesn't contain hello)", stats.TotalMatches)
	}
}

func TestOrchestratorRegexPattern(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "a.txt"), "cat\ncot\ncut\ndog\n")

	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "c[aou]t", []string{dir}, domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.TotalMatches != 3 {
		t.Errorf("TotalMatches = %d, want 3", stats.TotalMatches)
	}
}

func TestOrchestratorConcurrentFilesNotInterleaved(t *testing.T) {
	dir := t.TempDir()
	// Many files, each with several matching lines, searched with a
	// worker pool wider than 1, to exercise the "one file's matches
	// are written as one atomic batch" guarantee.
	for i := 0; i < 20; i++ {
		content := "target line 1\nnoise\ntarget line 2\ntarget line 3\n"
		writeFixture(t, filepath.Join(dir, "file"+string(rune('a'+i))+".txt"), content)
	}

	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "target", []string{dir}, domain.SearchOptions{Threads: 8})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.TotalMatches != 60 {
		t.Errorf("TotalMatches = %d, want 60", stats.TotalMatches)
	}

	// Verify matches for the same file are contiguous in the recorded
	// order (i.e. never interleaved with another file's matches).
	seen := make(map[string]bool)
	var lastPath string
	for _, m := range sink.matches {
		if m.Path != lastPath {
			if seen[m.Path] {
				t.Fatalf("file %s's matches were interleaved with another file's", m.Path)
			}
			seen[m.Path] = true
			lastPath = m.Path
		}
	}
}

// TestOrchestratorTypeFilter verifies --type filtering (opts.Types):
// files recognized by an extractor not in the allowed list are skipped
// entirely, and not counted as FilesSearched.
func TestOrchestratorTypeFilter(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "a.txt"), "hello world\n")
	writeFixture(t, filepath.Join(dir, "b.txt"), "hello again\n")

	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "hello", []string{dir}, domain.SearchOptions{Types: []string{"docx"}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.FilesSearched != 0 {
		t.Errorf("FilesSearched = %d, want 0 (both files are \"text\", filtered out by --type docx)", stats.FilesSearched)
	}
	if stats.TotalMatches != 0 {
		t.Errorf("TotalMatches = %d, want 0", stats.TotalMatches)
	}

	stats, err = orch.Run(context.Background(), "hello", []string{dir}, domain.SearchOptions{Types: []string{"text"}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.FilesSearched != 2 {
		t.Errorf("FilesSearched = %d, want 2", stats.FilesSearched)
	}
	if stats.TotalMatches != 2 {
		t.Errorf("TotalMatches = %d, want 2", stats.TotalMatches)
	}
}

// TestOrchestratorSearchesStdin confirms the domain.StdinPath ("-")
// pseudo-root is read from orch.Stdin via orch.StdinExtractor, entirely
// bypassing the walker/registry, and that resulting matches report "-"
// as their path -- not a temp file or any other real path -- matching
// grep/rg's own convention for piped input with no backing file.
func TestOrchestratorSearchesStdin(t *testing.T) {
	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)
	orch.Stdin = strings.NewReader("hello world\nnothing here\nhello again\n")
	orch.StdinExtractor = text.Extractor{}

	stats, err := orch.Run(context.Background(), "hello", []string{"-"}, domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.TotalMatches != 2 {
		t.Errorf("TotalMatches = %d, want 2", stats.TotalMatches)
	}
	if len(sink.matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(sink.matches))
	}
	for _, m := range sink.matches {
		if m.Path != domain.StdinPath {
			t.Errorf("match Path = %q, want %q", m.Path, domain.StdinPath)
		}
		if m.Format != "text" {
			t.Errorf("match Format = %q, want %q", m.Format, "text")
		}
	}
}

// TestOrchestratorSearchesStdinAlongsideRealRoots confirms "-" can be
// mixed with real filesystem roots in the same Run: the walker gets
// only the real root (stripped of "-"), while stdin is searched
// concurrently as one more producer into the same writer queue.
func TestOrchestratorSearchesStdinAlongsideRealRoots(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "a.txt"), "hello from disk\n")

	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)
	orch.Stdin = strings.NewReader("hello from stdin\n")
	orch.StdinExtractor = text.Extractor{}

	stats, err := orch.Run(context.Background(), "hello", []string{"-", dir}, domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.TotalMatches != 2 {
		t.Errorf("TotalMatches = %d, want 2", stats.TotalMatches)
	}

	var gotPaths []string
	for _, m := range sink.matches {
		gotPaths = append(gotPaths, m.Path)
	}
	sort.Strings(gotPaths)
	want := []string{domain.StdinPath, filepath.Join(dir, "a.txt")}
	sort.Strings(want)
	if !reflect.DeepEqual(gotPaths, want) {
		t.Errorf("match paths = %v, want %v", gotPaths, want)
	}
}

// TestOrchestratorStdinHonorsTypeFilter confirms --type filtering
// applies to stdin exactly like a real file: stdin is always searched
// as "text" (see StreamExtractor's doc comment), so a --type that isn't
// "text" must skip it, without counting it as searched.
func TestOrchestratorStdinHonorsTypeFilter(t *testing.T) {
	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)
	orch.Stdin = strings.NewReader("hello world\n")
	orch.StdinExtractor = text.Extractor{}

	stats, err := orch.Run(context.Background(), "hello", []string{"-"}, domain.SearchOptions{Types: []string{"docx"}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.FilesSearched != 0 {
		t.Errorf("FilesSearched = %d, want 0 (stdin is \"text\", filtered out by --type docx)", stats.FilesSearched)
	}
	if stats.TotalMatches != 0 {
		t.Errorf("TotalMatches = %d, want 0", stats.TotalMatches)
	}
}

// TestOrchestratorStreamsStdinMatchesAsFound is stdin's counterpart to
// TestOrchestratorStreamsMatchesAsFound: a match found on stdin must
// reach the sink as soon as it's read, not only once stdin reaches EOF
// -- confirming searchStdin's "genuine streaming, no full-input
// buffering" design actually holds, not just that ExtractReader looks
// like it would in isolation. An io.Pipe is used as orch.Stdin so the
// test controls exactly when each line becomes available to read,
// mirroring how gatedExtractor gates a file's units above.
func TestOrchestratorStreamsStdinMatchesAsFound(t *testing.T) {
	pr, pw := io.Pipe()

	firstWrite := make(chan domain.Match, 1)
	sink := newFakeSink()
	sink.onWriteMatch = func(m domain.Match) {
		select {
		case firstWrite <- m:
		default:
		}
	}

	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)
	orch.Stdin = pr
	orch.StdinExtractor = text.Extractor{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := orch.Run(context.Background(), "target", []string{"-"}, domain.SearchOptions{}); err != nil {
			t.Errorf("Run() error = %v", err)
		}
	}()

	if _, err := pw.Write([]byte("target one\n")); err != nil {
		t.Fatalf("writing to pipe: %v", err)
	}

	select {
	case m := <-firstWrite:
		if m.Text != "target one" {
			t.Errorf("first streamed match Text = %q, want %q", m.Text, "target one")
		}
	case <-done:
		t.Fatal("Run finished before the first match was written -- streaming isn't happening")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first match to stream to the sink")
	}

	pw.Write([]byte("target two\n"))
	pw.Close()
	<-done
}

// TestOrchestratorContextLines exercises -A/-B/-C context-line
// accumulation over a small multi-line text fixture, including the
// tricky case of two matches close enough together that their context
// windows overlap (line 4, below): it must appear exactly once in the
// output, not duplicated as both the first match's trailing context and
// the second match's leading context.
func TestOrchestratorContextLines(t *testing.T) {
	dir := t.TempDir()
	// Line numbers (1-indexed, matching the text plugin's Location.Line):
	//   1: alpha
	//   2: beta
	//   3: TARGET one   (match)
	//   4: gamma        (trailing context for line 3 AND leading context for line 5)
	//   5: TARGET two   (match)
	//   6: delta
	//   7: epsilon
	writeFixture(t, filepath.Join(dir, "a.txt"), "alpha\nbeta\nTARGET one\ngamma\nTARGET two\ndelta\nepsilon\n")

	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)

	opts := domain.SearchOptions{ContextBefore: 2, ContextAfter: 2}
	stats, err := orch.Run(context.Background(), "TARGET", []string{dir}, opts)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if stats.TotalMatches != 2 {
		t.Errorf("TotalMatches = %d, want 2 (context lines must not inflate this)", stats.TotalMatches)
	}
	if stats.FilesMatched != 1 {
		t.Errorf("FilesMatched = %d, want 1", stats.FilesMatched)
	}

	var gotLines []int
	var gotHasSpans []bool
	for _, m := range sink.matches {
		gotLines = append(gotLines, m.Location.Fields(m.Spans)["line"].(int))
		gotHasSpans = append(gotHasSpans, len(m.Spans) > 0)
	}

	wantLines := []int{1, 2, 3, 4, 5, 6, 7}
	if len(gotLines) != len(wantLines) {
		t.Fatalf("got lines %v, want %v (line 4 must appear exactly once, not duplicated)", gotLines, wantLines)
	}
	for i := range wantLines {
		if gotLines[i] != wantLines[i] {
			t.Fatalf("got lines %v, want %v in that exact order", gotLines, wantLines)
		}
	}

	// Only the real matches (line 3 and line 5) should carry non-empty
	// Spans; every context line uses the empty-Spans convention.
	wantHasSpans := []bool{false, false, true, false, true, false, false}
	for i := range wantHasSpans {
		if gotHasSpans[i] != wantHasSpans[i] {
			t.Errorf("line %d: hasSpans = %v, want %v", gotLines[i], gotHasSpans[i], wantHasSpans[i])
		}
	}
}

// TestOrchestratorContextLinesAfterOnly checks -A alone (no -B): only
// trailing context should appear, and it should stop at the requested
// count even when more non-matching lines follow.
func TestOrchestratorContextLinesAfterOnly(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "a.txt"), "TARGET\nctx1\nctx2\nctx3\nnope\n")

	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "TARGET", []string{dir}, domain.SearchOptions{ContextAfter: 2})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.TotalMatches != 1 {
		t.Errorf("TotalMatches = %d, want 1", stats.TotalMatches)
	}

	var gotLines []int
	for _, m := range sink.matches {
		gotLines = append(gotLines, m.Location.Fields(m.Spans)["line"].(int))
	}
	want := []int{1, 2, 3}
	if len(gotLines) != len(want) {
		t.Fatalf("got lines %v, want %v", gotLines, want)
	}
	for i := range want {
		if gotLines[i] != want[i] {
			t.Fatalf("got lines %v, want %v", gotLines, want)
		}
	}
}

// TestOrchestratorInvertMatchHonorsMaxCount is a regression test for a
// gap in the pre-existing implementation: -m/--max-count was declared
// as applying to InvertMatch too, but the invert-match branch
// `continue`d before ever reaching the MaxCount check, so it was
// silently ignored whenever -v was combined with -m. Fixed as part of
// extending searchFile for context lines.
func TestOrchestratorInvertMatchHonorsMaxCount(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "a.txt"), "nope1\nnope2\nnope3\nnope4\n")

	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "zzz-never-matches", []string{dir}, domain.SearchOptions{InvertMatch: true, MaxCount: 2})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.TotalMatches != 2 {
		t.Errorf("TotalMatches = %d, want 2 (MaxCount should cap invert-match results too)", stats.TotalMatches)
	}
}

// TestOrchestratorFilesWithMatchesAndCountOnly verifies that
// FilesWithMatches/CountOnly suppress per-match WriteMatch calls while
// still reporting the correct match count via WriteFileSummary, and
// that stats are unaffected.
func TestOrchestratorFilesWithMatchesAndCountOnly(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "a.txt"), "hello\nhello again\nnope\n")

	sink := newFakeSink()
	orch := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "hello", []string{dir}, domain.SearchOptions{FilesWithMatches: true})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(sink.matches) != 0 {
		t.Errorf("expected no WriteMatch calls in FilesWithMatches mode, got %d", len(sink.matches))
	}
	if stats.TotalMatches != 2 {
		t.Errorf("TotalMatches = %d, want 2", stats.TotalMatches)
	}
	path := filepath.Join(dir, "a.txt")
	if got := sink.summary[path]; got != 2 {
		t.Errorf("summary[%s] = %d, want 2", path, got)
	}

	sink2 := newFakeSink()
	orch2 := app.New(newTestRegistry(), walk.New(), match.NewFactory(), sink2)
	stats2, err := orch2.Run(context.Background(), "hello", []string{dir}, domain.SearchOptions{CountOnly: true})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(sink2.matches) != 0 {
		t.Errorf("expected no WriteMatch calls in CountOnly mode, got %d", len(sink2.matches))
	}
	if stats2.TotalMatches != 2 {
		t.Errorf("TotalMatches = %d, want 2", stats2.TotalMatches)
	}
}

// panicExtractor is a ports.DocumentExtractor whose Extract spawns a
// goroutine that panics, following the contract documented on
// ports.DocumentExtractor: it recovers from that panic INSIDE its own
// goroutine and reports it as a single error on the error channel,
// rather than letting the panic propagate (which it cannot do across
// goroutines anyway — recover() only unwinds the goroutine it's
// deferred in).
type panicExtractor struct{}

func (panicExtractor) Name() string { return "panic" }

func (panicExtractor) Sniff(path string, ra io.ReaderAt, size int64) bool { return true }

func (panicExtractor) Extract(ctx context.Context, ra io.ReaderAt, size int64) (<-chan domain.TextUnit, <-chan error) {
	units := make(chan domain.TextUnit)
	errc := make(chan error, 1)

	go func() {
		defer close(units)
		defer close(errc)
		defer func() {
			if r := recover(); r != nil {
				select {
				case errc <- fmt.Errorf("panic during extraction: %v", r):
				default:
				}
			}
		}()

		// Simulate the failure mode malformed XML/zip content is
		// expected to trigger in the docx/pptx/xlsx plugins: an
		// index-out-of-range or nil-dereference partway through
		// streaming decode.
		var bad []int
		_ = bad[5] // panics: index out of range
	}()

	return units, errc
}

// fakeLookup is a minimal ExtractorLookup that always resolves to a
// given extractor, regardless of path/contents.
type fakeLookup struct{ extractor ports.DocumentExtractor }

func (f fakeLookup) For(path string, ra io.ReaderAt, size int64) (ports.DocumentExtractor, bool) {
	return f.extractor, true
}

// TestOrchestratorSurvivesExtractorGoroutinePanic is a regression test
// for a bug where a panic inside a DocumentExtractor's own Extract
// goroutine could crash the whole process: a recover() deferred in a
// DIFFERENT goroutine (e.g. the orchestrator's per-file worker) cannot
// catch a panic raised in this one. The fix requires every extractor to
// recover inside its own goroutine and report the panic via the error
// channel instead (see ports.DocumentExtractor's doc comment and
// internal/adapters/extract/text/text.go for the reference
// implementation). This test exercises that contract end-to-end through
// the real SearchOrchestrator.Run and asserts the run completes
// normally, with the panic surfaced as a logged warning, instead of
// crashing the test binary.
func TestOrchestratorSurvivesExtractorGoroutinePanic(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "corrupt.docx"), "this content is irrelevant; panicExtractor always claims it")

	var stderr bytes.Buffer
	sink := newFakeSink()
	orch := app.New(fakeLookup{extractor: panicExtractor{}}, walk.New(), match.NewFactory(), sink)
	orch.Stderr = &stderr

	stats, err := orch.Run(context.Background(), "anything", []string{dir}, domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (per-file failures must not fail the whole run)", err)
	}
	if stats.TotalMatches != 0 {
		t.Errorf("TotalMatches = %d, want 0 (the panicking file produced no units)", stats.TotalMatches)
	}
	if !strings.Contains(stderr.String(), "panic") {
		t.Errorf("expected the panic to be logged as a warning to Stderr, got %q", stderr.String())
	}
}

// gatedExtractor is a ports.DocumentExtractor that sends one matching
// unit per string in lines, blocking after each send until release is
// signaled -- letting a test control exactly when the "rest of a slow
// file" becomes available, to observe whether the orchestrator writes
// each match as it's found or only after the whole file finishes.
type gatedExtractor struct {
	lines   []string
	release <-chan struct{}
}

func (gatedExtractor) Name() string                                       { return "gated" }
func (gatedExtractor) Sniff(path string, ra io.ReaderAt, size int64) bool { return true }

func (g gatedExtractor) Extract(ctx context.Context, ra io.ReaderAt, size int64) (<-chan domain.TextUnit, <-chan error) {
	units := make(chan domain.TextUnit)
	errc := make(chan error, 1)
	go func() {
		defer close(units)
		defer close(errc)
		for i, line := range g.lines {
			select {
			case units <- domain.TextUnit{Location: fakeLineLocation{line: i + 1}, Text: line}:
			case <-ctx.Done():
				return
			}
			if i < len(g.lines)-1 {
				select {
				case <-g.release:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return units, errc
}

// TestOrchestratorStreamsMatchesAsFound is a regression test for
// per-file streaming: a match must reach the sink as soon as it's
// found, not only after the whole file has been fully extracted. Using
// gatedExtractor, the second unit is withheld until the test explicitly
// releases it; if the orchestrator batched matches in memory and wrote
// them out only once the file finished (the earlier behavior), the
// first match would never appear in the sink while this test is
// blocked waiting on it below -- the test would time out instead of
// observing it.
func TestOrchestratorStreamsMatchesAsFound(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "slow.txt"), "irrelevant; gatedExtractor supplies its own units")

	release := make(chan struct{})
	extractor := gatedExtractor{lines: []string{"target one", "target two"}, release: release}

	firstWrite := make(chan domain.Match, 1)
	sink := newFakeSink()
	sink.onWriteMatch = func(m domain.Match) {
		select {
		case firstWrite <- m:
		default:
		}
	}

	orch := app.New(fakeLookup{extractor: extractor}, walk.New(), match.NewFactory(), sink)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := orch.Run(context.Background(), "target", []string{dir}, domain.SearchOptions{}); err != nil {
			t.Errorf("Run() error = %v", err)
		}
	}()

	select {
	case m := <-firstWrite:
		if m.Text != "target one" {
			t.Errorf("first streamed match Text = %q, want %q", m.Text, "target one")
		}
	case <-done:
		t.Fatal("Run finished before the first match was written -- streaming isn't happening")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first match to stream to the sink")
	}

	close(release)
	<-done
}
