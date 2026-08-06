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
//
// Concurrency model: each of Run's worker goroutines does the CPU-bound
// extraction+matching for its files completely independently -- nothing
// about that work is ever serialized across files. The only serialized
// part is the actual write to Sink, and that's handled by a single
// dedicated writer goroutine (spawned in Run) draining a FIFO queue of
// per-file fileSessions, one at a time, fully draining one file's
// matches (in the order they were found) before moving to the next
// file in queue order. Because the writer is the sole caller of Sink's
// methods, no mutex is needed to keep two files' output from
// interleaving. This also means a slow-to-extract file's matches still
// reach the sink as they're found -- the writer is draining its
// session's matches channel concurrently with the rest of that file
// being extracted -- without that file's unrelated CPU work ever
// blocking any other file's writes the way a single global write lock
// held across a whole file's processing would (profiling a
// high-match-density corpus showed exactly that: wall-clock time
// completely flat regardless of thread count under that earlier
// design).
type SearchOrchestrator struct {
	Registry ExtractorLookup
	Walker   ports.FileWalker
	Matchers ports.MatcherFactory
	Sink     ports.OutputSink

	// StdinExtractor, if set, is used to search piped stdin (the
	// domain.StdinPath "-" pseudo-root, matching grep/rg convention)
	// directly off an io.Reader -- no temp file, no io.ReaderAt/size,
	// genuine streaming. If nil, "-" is silently skipped, the same way
	// an unrecognized file format is.
	StdinExtractor ports.StreamExtractor

	// Stdin is read from when "-" is requested. If nil, os.Stdin is
	// used. Overridable so tests don't need a real stdin.
	Stdin io.Reader

	// Stderr receives warnings about per-file failures (unreadable
	// files, panics recovered from a misbehaving extractor, etc). If
	// nil, os.Stderr is used.
	Stderr io.Writer
}

// fileSession is one file's slot in the single writer goroutine's FIFO
// queue (see Run): matches carries this file's Matches as they're
// found, closed once the file's search ends (however it ends).
// summaryCh carries the file's final match count for WriteFileSummary,
// sent exactly once in the normal case -- or closed without a value if
// the file's search aborted before producing one (e.g. a recovered
// panic), telling the writer to skip that file's summary rather than
// block forever waiting for a count that will never arrive.
type fileSession struct {
	path      string
	matches   chan domain.Match
	summaryCh chan int
}

// matchQueueBuffer bounds how many of one file's matches the writer
// goroutine may lag behind by before that file's own searchFile call
// starts blocking on a send. Large enough that a fast worker rarely
// blocks waiting for the (usually much cheaper) write side to catch up;
// small enough that a pathologically match-dense file can't grow an
// unbounded in-memory backlog the way batching a whole file's matches
// before writing any of them would.
const matchQueueBuffer = 256

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
// opts, and writes matches to o.Sink. A literal domain.StdinPath ("-")
// entry in roots is handled separately from the rest (see
// splitStdinRoot): it's searched via o.StdinExtractor against o.Stdin
// directly, never through the walker/registry. It returns aggregate
// Stats plus the first fatal error encountered (a walker error, or a Compile
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

	// "-" is grep/rg's convention for reading standard input in place of
	// a real path. The walker only ever deals in real filesystem paths
	// (and must stay that way), so it's stripped out of roots here and
	// searched separately below, as one more producer into the same
	// sessions queue every file-search worker already uses.
	walkRoots, stdinRequested := splitStdinRoot(roots)

	paths, walkErrc := o.Walker.Walk(runCtx, walkRoots, opts)

	// sessions is the writer goroutine's FIFO queue of pending file
	// write-outs, one entry per file that actually has something to
	// write (see fileSession, and searchFile's ensureSession). Buffered
	// by threads so a worker enqueueing a new session doesn't have to
	// wait for the writer to finish the previous one unless more than
	// `threads` files are simultaneously ready to write at once.
	sessions := make(chan *fileSession, threads)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for session := range sessions {
			for m := range session.matches {
				// A panic here (e.g. from a misbehaving Sink) must not
				// take down the writer goroutine: unlike searchFile's own
				// per-file recover(), this goroutine is shared by every
				// file, so an unrecovered panic would stop draining every
				// other file's session too, deadlocking their producers.
				// Scoped per-write (not once around the whole session) so
				// draining continues -- and this file's remaining matches
				// and its summary are still consumed -- after a single
				// bad write.
				func() {
					defer func() {
						if r := recover(); r != nil {
							fmt.Fprintf(stderr, "ogrep: warning: panic writing match for %s: %v\n", session.path, r)
						}
					}()
					if werr := o.Sink.WriteMatch(m); werr != nil {
						fmt.Fprintf(stderr, "ogrep: warning: writing match for %s: %v\n", session.path, werr)
					}
				}()
			}
			if count, ok := <-session.summaryCh; ok {
				func() {
					defer func() {
						if r := recover(); r != nil {
							fmt.Fprintf(stderr, "ogrep: warning: panic writing summary for %s: %v\n", session.path, r)
						}
					}()
					if werr := o.Sink.WriteFileSummary(session.path, count); werr != nil {
						fmt.Fprintf(stderr, "ogrep: warning: writing summary for %s: %v\n", session.path, werr)
					}
				}()
			}
		}
	}()

	var wg sync.WaitGroup
	var firstWalkErr error
	var walkErrOnce sync.Once

	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range paths {
				atomic.AddInt64(&stats.FilesWalked, 1)
				o.searchFile(runCtx, path, matcher, opts, &stats, stderr, sessions)
			}
		}()
	}

	if stdinRequested {
		stdin := o.Stdin
		if stdin == nil {
			stdin = os.Stdin
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			atomic.AddInt64(&stats.FilesWalked, 1)
			o.searchStdin(runCtx, stdin, matcher, opts, &stats, stderr, sessions)
		}()
	}

	wg.Wait()
	close(sessions)
	<-writerDone

	if err, ok := <-walkErrc; ok && err != nil {
		walkErrOnce.Do(func() { firstWalkErr = err })
	}

	if err := o.Sink.Flush(); err != nil {
		return stats, fmt.Errorf("flushing output: %w", err)
	}

	return stats, firstWalkErr
}

// searchFile handles exactly one file: extractor lookup, streaming
// extraction, then delegating to runOnUnits for matching and writing
// results (see SearchOrchestrator's doc comment for the overall
// concurrency model, and runOnUnits's doc comment for what the deferred
// recover() below can and can't catch).
func (o *SearchOrchestrator) searchFile(ctx context.Context, path string, matcher ports.Matcher, opts domain.SearchOptions, stats *Stats, stderr io.Writer, sessions chan<- *fileSession) {
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

	// A per-file context lets us unblock (and let the extractor's
	// goroutine exit and close its channels) if we stop consuming units
	// early, e.g. because MaxCount was reached or the run is being
	// cancelled — without this, the extractor could block forever
	// trying to send its next unit to a reader that's gone away.
	fileCtx, fileCancel := context.WithCancel(ctx)
	defer fileCancel()

	units, extractErrc := extractor.Extract(fileCtx, f, size)

	o.runOnUnits(ctx, fileCancel, path, extractor.Name(), units, extractErrc, matcher, opts, stats, stderr, sessions)
}

// searchStdin is searchFile's stdin-shaped sibling: same matching,
// context-line, and session-write behavior (via runOnUnits), fed from
// o.StdinExtractor's direct io.Reader stream instead of a
// Registry-resolved, file-backed extractor. Every match it produces
// reports domain.StdinPath ("-") as its path, matching grep/rg's own
// convention for a piped input with no real backing file.
//
// opts.IncludeGlobs/ExcludeGlobs/NoIgnore never apply here (there's no
// path for a glob to match against, or an ignore file to consult) --
// only opts.Types is checked below, same as rg's own stdin handling.
//
// Because sessions is drained strictly FIFO (see fileSession's doc
// comment), an indefinitely-long, slow-trickling stdin (e.g. `tail -f
// |ogrep pat - somedir`) enqueued ahead of a real root's files would
// starve them: their own sessions queue up behind stdin's still-open
// one and never get drained. A finite stdin (the overwhelmingly common
// case -- piping a command's output, or "-" alone) is unaffected.
func (o *SearchOrchestrator) searchStdin(ctx context.Context, r io.Reader, matcher ports.Matcher, opts domain.SearchOptions, stats *Stats, stderr io.Writer, sessions chan<- *fileSession) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(stderr, "ogrep: warning: panic while searching %s: %v\n", domain.StdinPath, rec)
		}
	}()

	if o.StdinExtractor == nil {
		return
	}
	if len(opts.Types) > 0 && !typeAllowed(opts.Types, o.StdinExtractor.Name()) {
		return
	}
	atomic.AddInt64(&stats.FilesSearched, 1)

	fileCtx, fileCancel := context.WithCancel(ctx)
	defer fileCancel()

	units, extractErrc := o.StdinExtractor.ExtractReader(fileCtx, r)

	o.runOnUnits(ctx, fileCancel, domain.StdinPath, o.StdinExtractor.Name(), units, extractErrc, matcher, opts, stats, stderr, sessions)
}

// runOnUnits is the shared tail of searchFile and searchStdin: matching,
// -A/-B/-C context-line bookkeeping, -m/--max-count, -v/--invert-match,
// and enqueuing path's results for the writer goroutine as
// they're found (see SearchOrchestrator's doc comment for the overall
// concurrency model). It has no notion of how units/extractErrc were
// produced -- a Registry-resolved file extractor and a stdin
// StreamExtractor look identical from here.
//
// fileCancel is the cancel func for the context units/extractErrc were
// produced under; it's called explicitly below (not just deferred by
// the caller) to unblock a still-producing extractor goroutine as soon
// as this function stops consuming units early (MaxCount/context cap
// reached, or ctx cancelled), before the blocking receive on
// extractErrc that follows.
//
// The deferred recover() in searchFile/searchStdin only catches panics
// that happen synchronously in THIS goroutine — e.g. inside
// Registry.For/Sniff, or inside Matcher.FindAll while we range over
// units. It does NOT, and cannot, catch a panic raised inside an
// extractor's own Extract/ExtractReader goroutine (see
// internal/adapters/extract/text/text.go), since Go's recover() only
// unwinds the goroutine it's deferred in. That case — the one most
// likely to be triggered by malformed XML/zip content in the
// docx/pptx/xlsx plugins — is the extractor's own responsibility to
// guard against, per the contract documented on ports.DocumentExtractor:
// implementations must recover inside their Extract goroutine and
// report the panic as an error on the error channel instead of letting
// it crash the process.
func (o *SearchOrchestrator) runOnUnits(ctx context.Context, fileCancel context.CancelFunc, path, format string, units <-chan domain.TextUnit, extractErrc <-chan error, matcher ports.Matcher, opts domain.SearchOptions, stats *Stats, stderr io.Writer, sessions chan<- *fileSession) {
	// realMatchCount tracks only genuine matches -- used for
	// Stats.TotalMatches, the -m/--max-count cap, and the count reported
	// to -c/--count -- so context lines (-A/-B/-C) never inflate it.
	realMatchCount := 0

	// session is this file's slot in the writer's FIFO queue, created
	// lazily by ensureSession on the first actual write this file makes
	// (mirroring the old lockOnce pattern): a file with no matches never
	// enqueues anything, and -l/-c files only enqueue once, for their
	// summary, never for individual matches.
	var session *fileSession
	summarySent := false
	ensureSession := func() *fileSession {
		if session == nil {
			session = &fileSession{
				path:      path,
				matches:   make(chan domain.Match, matchQueueBuffer),
				summaryCh: make(chan int, 1),
			}
			select {
			case sessions <- session:
			case <-ctx.Done():
				// Run is winding down; the writer may never see this
				// session. Sends below select on ctx.Done() too, so
				// searchFile still won't block forever.
			}
		}
		return session
	}
	defer func() {
		if session != nil {
			close(session.matches)
			if !summarySent {
				// searchFile is returning without ever reaching the
				// normal summary send below (e.g. a recovered panic
				// mid-loop). Close, don't leave the writer blocked
				// forever waiting on a count that's never coming.
				close(session.summaryCh)
			}
		}
	}()

	// write sends m to this file's session, unless -l/-c was requested
	// (in which case only the per-file count that write's callers
	// already track matters, not the matches themselves).
	write := func(m domain.Match) {
		if opts.FilesWithMatches || opts.CountOnly {
			return
		}
		s := ensureSession()
		select {
		case s.matches <- m:
		case <-ctx.Done():
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

	s := ensureSession()
	s.summaryCh <- realMatchCount
	summarySent = true
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

// splitStdinRoot pulls every literal domain.StdinPath ("-") entry out
// of roots -- the walker only ever deals in real filesystem paths and
// must stay that way -- returning the rest as walkRoots and whether any
// were found at all. Repeated "-" entries collapse to a single bool:
// stdin is read once regardless of how many times "-" was given, since
// (unlike a real path) re-reading os.Stdin a second time wouldn't
// reproduce the same bytes.
func splitStdinRoot(roots []string) (walkRoots []string, stdinRequested bool) {
	walkRoots = make([]string, 0, len(roots))
	for _, r := range roots {
		if r == domain.StdinPath {
			stdinRequested = true
			continue
		}
		walkRoots = append(walkRoots, r)
	}
	return walkRoots, stdinRequested
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
