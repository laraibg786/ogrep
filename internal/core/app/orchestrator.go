// Package app implements ogrep's core use case: a parallel
// worker-pool search that ties together the FileWalker, the format
// Registry, extractor plugins, a compiled Matcher, and an OutputSink.
package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/core/ports"
)

// ExtractorLookup resolves the DocumentExtractor for a given file. In
// production this is registry.Registry.For; tests can supply a fake.
type ExtractorLookup interface {
	For(path string, ra io.ReaderAt, size int64) (ports.DocumentExtractor, bool)
}

// SearchOrchestrator implements the "search" use case: walk files in
// parallel, dispatch each to its extractor, match its extracted text,
// and write results to a single OutputSink without interleaving
// different files' output.
type SearchOrchestrator struct {
	Registry ExtractorLookup
	Walker   ports.FileWalker
	Matchers ports.MatcherFactory
	Sink     ports.OutputSink

	// Stderr receives warnings about per-file failures (unreadable
	// files, panics recovered from a misbehaving extractor, etc). If
	// nil, os.Stderr is used.
	Stderr io.Writer

	// writeMu serializes the whole per-file write-out (every WriteMatch
	// call for one file, plus its WriteFileSummary) so that two files'
	// results are never interleaved, even though many files are
	// processed concurrently. The OutputSink implementations are also
	// individually safe for concurrent use; this mutex additionally
	// guarantees a whole file's batch of matches is written as one
	// contiguous unit, matching the grouped-by-file output convention
	// grep-family tools use even under parallelism.
	//
	// searchFile acquires this lazily, on its first actual write (see
	// lockOnce there), and holds it continuously through the rest of
	// that file's matches and its final summary line -- not just for a
	// single quick flush at the end. That's what lets one file's matches
	// reach the sink as they're found rather than only after the whole
	// file has been fully read (valuable for a large, slow-to-extract
	// file), at a real cost: once a file has started writing, every
	// other file's own writes block until this one finishes, for
	// whatever's left of its processing after that first match. Because
	// the lock is only acquired once a file actually has something to
	// write, a file with no matches never blocks anyone, and files
	// finishing before another's first match are unaffected.
	writeMu sync.Mutex
}

// New builds a SearchOrchestrator from its four collaborators.
func New(reg ExtractorLookup, walker ports.FileWalker, matchers ports.MatcherFactory, sink ports.OutputSink) *SearchOrchestrator {
	return &SearchOrchestrator{Registry: reg, Walker: walker, Matchers: matchers, Sink: sink}
}

// Stats summarizes one Run.
type Stats struct {
	FilesWalked   int64
	FilesSearched int64 // files recognized by an extractor and actually searched
	FilesMatched  int64
	TotalMatches  int64
}

// Run walks roots, searches every recognized file for pattern under
// opts, and writes matches to o.Sink. It returns aggregate Stats plus
// the first fatal error encountered (a walker error, or a Compile
// error); per-file problems are logged as warnings to o.Stderr and do
// not fail the whole run.
func (o *SearchOrchestrator) Run(ctx context.Context, pattern string, roots []string, opts domain.SearchOptions) (Stats, error) {
	var stats Stats

	matcher, err := o.Matchers.Compile(pattern, opts)
	if err != nil {
		return stats, fmt.Errorf("compiling pattern: %w", err)
	}

	stderr := o.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	threads := opts.Threads
	if threads <= 0 {
		threads = runtime.NumCPU()
	}
	if threads < 1 {
		threads = 1
	}

	// runCtx is cancelled either by the caller's ctx or once MaxCount
	// total matches have been found, giving early-exit behavior for a
	// future -m/max-total-matches flag without the walker or extractors
	// needing to know about it.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	paths, walkErrc := o.Walker.Walk(runCtx, roots, opts)

	var wg sync.WaitGroup
	var firstWalkErr error
	var walkErrOnce sync.Once

	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range paths {
				atomic.AddInt64(&stats.FilesWalked, 1)
				o.searchFile(runCtx, path, matcher, opts, &stats, stderr)
			}
		}()
	}

	wg.Wait()

	if err, ok := <-walkErrc; ok && err != nil {
		walkErrOnce.Do(func() { firstWalkErr = err })
	}

	if err := o.Sink.Flush(); err != nil {
		return stats, fmt.Errorf("flushing output: %w", err)
	}

	return stats, firstWalkErr
}

// searchFile handles exactly one file: extractor lookup, streaming
// extraction, matching, and writing this file's results to o.Sink as
// they're found (see writeMu's doc comment for why writes, not just
// extraction, are streamed).
//
// The deferred recover() below only catches panics that happen
// synchronously in THIS goroutine — e.g. inside Registry.For/Sniff, or
// inside Matcher.FindAll while we range over units. It does NOT, and
// cannot, catch a panic raised inside an extractor's own Extract
// goroutine (see internal/adapters/extract/text/text.go), since Go's
// recover() only unwinds the goroutine it's deferred in. That case —
// the one most likely to be triggered by malformed XML/zip content in
// the docx/pptx/xlsx plugins — is the extractor's own responsibility to
// guard against, per the contract documented on
// ports.DocumentExtractor: implementations must recover inside their
// Extract goroutine and report the panic as an error on the error
// channel instead of letting it crash the process.
func (o *SearchOrchestrator) searchFile(ctx context.Context, path string, matcher ports.Matcher, opts domain.SearchOptions, stats *Stats, stderr io.Writer) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "ogrep: warning: panic while searching %s: %v\n", path, r)
		}
	}()

	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "ogrep: warning: %s: %v\n", path, err)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		fmt.Fprintf(stderr, "ogrep: warning: %s: %v\n", path, err)
		return
	}
	size := info.Size()

	extractor, ok := o.Registry.For(path, f, size)
	if !ok {
		return // not a recognized format; silently skip, like rg skipping binaries
	}
	if len(opts.Types) > 0 && !typeAllowed(opts.Types, extractor.Name()) {
		// Filtered out by --type: treat exactly like an unrecognized
		// format (not counted as searched), rather than as a file that
		// was searched and simply produced no matches.
		return
	}
	atomic.AddInt64(&stats.FilesSearched, 1)
	format := extractor.Name()

	// A per-file context lets us unblock (and let the extractor's
	// goroutine exit and close its channels) if we stop consuming units
	// early, e.g. because MaxCount was reached or the run is being
	// cancelled — without this, the extractor could block forever
	// trying to send its next unit to a reader that's gone away.
	fileCtx, fileCancel := context.WithCancel(ctx)
	defer fileCancel()

	units, extractErrc := extractor.Extract(fileCtx, f, size)

	// realMatchCount tracks only genuine matches -- used for
	// Stats.TotalMatches, the -m/--max-count cap, and the count reported
	// to -c/--count -- so context lines (-A/-B/-C) never inflate it.
	realMatchCount := 0

	// writeLocked is true once this file has acquired o.writeMu; it's
	// acquired lazily, right before the first actual write this file
	// makes (see writeMu's doc comment), and released via the deferred
	// unlockIfLocked below once searchFile returns -- whether that's
	// after a normal finish or an early return, e.g. a zero-match file
	// skipping straight past every write.
	writeLocked := false
	lockOnce := func() {
		if !writeLocked {
			o.writeMu.Lock()
			writeLocked = true
		}
	}
	defer func() {
		if writeLocked {
			o.writeMu.Unlock()
		}
	}()

	// write sends m to o.Sink.WriteMatch, unless -l/-c was requested (in
	// which case only the per-file count that write's callers already
	// track matters, not the matches themselves).
	write := func(m domain.Match) {
		if opts.FilesWithMatches || opts.CountOnly {
			return
		}
		lockOnce()
		if werr := o.Sink.WriteMatch(m); werr != nil {
			fmt.Fprintf(stderr, "ogrep: warning: writing match for %s: %v\n", path, werr)
		}
	}

	// Context-line bookkeeping for -A/-B/-C, kept bounded (at most
	// opts.ContextBefore units buffered at any time — never the whole
	// file):
	//
	//   - before is a fixed-capacity ring of the most recently seen
	//     units, flushed (as context) whenever a match is found, then
	//     reset.
	//   - afterRemaining counts down how many more non-matching units
	//     to still emit as trailing context after the most recent
	//     match.
	//   - lastEmittedIdx is the sequential index of the last unit
	//     passed to emit; emit uses it to avoid re-emitting a unit
	//     that's already been written (e.g. a unit that was trailing
	//     context for one match and would otherwise also be flushed as
	//     leading context for the next), which is exactly the "two
	//     matches whose context windows overlap" case that must not
	//     produce duplicate entries.
	before := newContextRing(opts.ContextBefore)
	afterRemaining := 0
	lastEmittedIdx := -1
	maxCountReached := false

	emit := func(idx int, u domain.TextUnit, spans []domain.Span) {
		if idx <= lastEmittedIdx {
			return
		}
		write(domain.Match{Path: path, Format: format, Location: u.Location, Text: u.Text, Spans: spans})
		lastEmittedIdx = idx
	}

	unitIdx := -1
	for unit := range units {
		unitIdx++
		if ctx.Err() != nil {
			break
		}
		spans := matcher.FindAll(unit.Text)
		matched := len(spans) > 0

		if opts.InvertMatch {
			// Context lines (-A/-B/-C) are not combined with
			// -v/--invert-match in v1: invert-match's whole result set
			// is already "every non-matching unit", so a context
			// window around each one adds little. MaxCount is honored
			// here too (a preexisting gap: the original implementation
			// never applied -m to the invert-match path at all, since
			// it `continue`d before reaching the cap check below).
			if matched {
				continue
			}
			write(domain.Match{Path: path, Format: format, Location: unit.Location, Text: unit.Text})
			realMatchCount++
			if opts.MaxCount > 0 && realMatchCount >= opts.MaxCount {
				break
			}
			continue
		}

		if matched {
			for _, bu := range before.items() {
				emit(bu.idx, bu.unit, nil)
			}
			before.reset()

			emit(unitIdx, unit, spans)
			realMatchCount++
			afterRemaining = opts.ContextAfter

			if opts.MaxCount > 0 && realMatchCount >= opts.MaxCount {
				// Don't stop immediately: still emit any trailing
				// context owed to this (final, per the cap) match.
				maxCountReached = true
			}
		} else {
			if afterRemaining > 0 {
				emit(unitIdx, unit, nil)
				afterRemaining--
			}
			before.push(ctxUnit{idx: unitIdx, unit: unit})
		}

		if maxCountReached && afterRemaining <= 0 {
			break
		}
	}

	// Cancel fileCtx before draining extractErrc: if we stopped
	// consuming units early (MaxCount/context cap reached above, or the
	// run-level ctx being cancelled), the extractor's own goroutine may
	// currently be blocked trying to send its next unit to a reader
	// that's gone away. Without cancelling here first, the blocking
	// receive on extractErrc below would deadlock forever waiting for a
	// goroutine that itself is waiting on us. This is a no-op if the
	// extractor already finished and closed its channels on its own
	// (the common case: we drained every unit it sent).
	fileCancel()

	if err, ok := <-extractErrc; ok && err != nil {
		fmt.Fprintf(stderr, "ogrep: warning: %s: %v\n", path, err)
	}

	if realMatchCount == 0 {
		return
	}

	atomic.AddInt64(&stats.FilesMatched, 1)
	atomic.AddInt64(&stats.TotalMatches, int64(realMatchCount))

	lockOnce()
	if werr := o.Sink.WriteFileSummary(path, realMatchCount); werr != nil {
		fmt.Fprintf(stderr, "ogrep: warning: writing summary for %s: %v\n", path, werr)
	}
}

// typeAllowed reports whether name (an extractor's Name(), e.g. "docx")
// is one of the values passed via --type.
func typeAllowed(types []string, name string) bool {
	for _, t := range types {
		if t == name {
			return true
		}
	}
	return false
}

// ctxUnit pairs a TextUnit with its sequential position within the
// current file's unit stream, so context-window bookkeeping (detecting
// overlap with already-emitted units) doesn't need to retain every unit
// seen so far — only the small bounded set in contextRing.
type ctxUnit struct {
	idx  int
	unit domain.TextUnit
}

// contextRing is a small fixed-capacity ring buffer holding the most
// recently seen TextUnits, used to implement -B/--before-context
// without buffering a whole file's units in memory: at most
// opts.ContextBefore units are ever retained at once, regardless of
// file size.
type contextRing struct {
	buf   []ctxUnit
	start int
	n     int
}

// newContextRing returns a ring with room for capacity units (capacity
// 0 or less means the ring holds nothing and push is a no-op, i.e.
// -B/-C wasn't requested).
func newContextRing(capacity int) contextRing {
	if capacity < 0 {
		capacity = 0
	}
	return contextRing{buf: make([]ctxUnit, capacity)}
}

// push records u as the most recently seen unit, evicting the oldest
// entry once the ring is full.
func (r *contextRing) push(u ctxUnit) {
	if len(r.buf) == 0 {
		return
	}
	i := (r.start + r.n) % len(r.buf)
	r.buf[i] = u
	if r.n < len(r.buf) {
		r.n++
	} else {
		r.start = (r.start + 1) % len(r.buf)
	}
}

// items returns the ring's current contents in oldest-to-newest order.
func (r *contextRing) items() []ctxUnit {
	out := make([]ctxUnit, r.n)
	for i := 0; i < r.n; i++ {
		out[i] = r.buf[(r.start+i)%len(r.buf)]
	}
	return out
}

// reset empties the ring, keeping its allocated capacity.
func (r *contextRing) reset() { r.start, r.n = 0, 0 }
