package walk

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// vcsDirs are directories skipped by default, regardless of ignore
// files.
var vcsDirs = map[string]bool{
	".git": true,
}

// ignoreFileNames are the ignore files consulted in each directory,
// in load order. .ogrepignore is project-local and additional to
// .gitignore, not a replacement for it.
var ignoreFileNames = []string{".gitignore", ".ogrepignore"}

// Walker implements ports.FileWalker over the local filesystem.
type Walker struct{}

// New returns a ready-to-use Walker.
func New() *Walker { return &Walker{} }

// ruleNode is one link in a directory's inherited ignore-rule chain: it
// pairs a directory with the ignore rules loaded from that directory's
// own ignore files, plus a pointer back to its parent's node.
//
// This is a persistent (immutable, singly-linked, parent-pointing) data
// structure: once built, a ruleNode is never mutated, so many worker
// goroutines can hold a reference to the same node (as the "parent" of
// however many sibling subdirectories it has) and read through it
// concurrently without copying, locking, or racing. Each subtree simply
// carries forward a pointer to the chain link for its own directory;
// building a child's node is O(1) (load that one directory's own ignore
// files, link to the existing parent) rather than O(depth), which the
// old single-goroutine walker's copy-on-push stack implicitly paid for
// every directory anyway.
type ruleNode struct {
	dir    string
	sets   []PatternSet
	parent *ruleNode
}

// dirJob is one unit of traversal work: a directory to list, plus the
// ignore-rule chain inherited from its ancestors (NOT including this
// directory's own ignore files yet -- those are loaded when the job is
// processed, matching the original walker's rule that a directory's own
// ignore file affects its children, not whether the directory itself is
// ignored).
type dirJob struct {
	dir    string
	parent *ruleNode
}

// resolveThreads applies the same "opts.Threads, falling back to
// runtime.NumCPU()" convention the orchestrator uses for its search
// worker pool, so the traversal phase and the search phase scale
// consistently with each other and with the configured thread count.
func resolveThreads(opts domain.SearchOptions) int {
	threads := opts.Threads
	if threads <= 0 {
		threads = runtime.NumCPU()
	}
	if threads < 1 {
		threads = 1
	}
	return threads
}

// Walk implements ports.FileWalker using a bounded concurrent
// work-queue: a pool of worker goroutines pulls directory jobs from a
// small, fixed-capacity channel, each job producing zero or more file
// paths (sent directly on the output channel) and zero or more new
// directory jobs (for subdirectories that pass ignore/glob/VCS-skip
// checks).
//
// Termination detection ("dynamic fan-out" problem: the total number of
// directories isn't known upfront) uses a sync.WaitGroup: the count is
// incremented once before a job is ever enqueued (whether handed off to
// the shared channel or processed inline -- see below) and decremented
// exactly once after that job has been fully processed (all of its
// entries examined and all of its own child jobs already Add()'ed). A
// dedicated goroutine calls wg.Wait() and then closes the jobs channel;
// because every Add() for a child job happens-before the Done() for its
// parent job (the parent doesn't return, and therefore doesn't get its
// Done() called, until every child it discovered has already been
// Add()'ed), the counter can only ever reach zero when there is
// genuinely no more work anywhere -- not when a single worker's local
// queue happens to be momentarily empty. That rules out both failure
// modes named in the design: closing early (which would drop files whose
// jobs were enqueued-but-not-yet-processed) and never closing (hanging
// forever), since the Add/Done pairing is exact and every code path that
// increments also unconditionally decrements (via defer/immediate call),
// including early-return paths taken because ctx was cancelled.
//
// Bounded-queue self-deadlock avoidance: workers are also the sole
// consumers of the shared jobs channel, so a worker must never block
// trying to *send* a newly discovered child job onto that same channel
// (if every worker were simultaneously blocked sending, nobody would be
// left to receive, and the whole pool would wedge). Enqueuing therefore
// always tries a non-blocking send first; if the channel is momentarily
// full, the worker just processes that child job itself, synchronously,
// right there (a "spill to inline DFS" fallback) rather than blocking.
// This keeps the shared channel's buffer -- and therefore the walker's
// steady-state memory use -- bounded by a small constant multiple of the
// worker count, never by the size of the tree, while still guaranteeing
// forward progress.
func (w *Walker) Walk(ctx context.Context, roots []string, opts domain.SearchOptions) (<-chan string, <-chan error) {
	paths := make(chan string)
	errc := make(chan error, 1)

	threads := resolveThreads(opts)

	go func() {
		defer close(paths)
		defer close(errc)

		// Capacity is a small constant multiple of the worker count: at
		// any instant there are at most a handful of directory jobs
		// in flight per worker, never one entry per file/directory in
		// the whole tree.
		const backlogFactor = 4
		jobs := make(chan dirJob, threads*backlogFactor)

		var wg sync.WaitGroup
		var errOnce sync.Once
		reportErr := func(err error) {
			if err == nil {
				return
			}
			errOnce.Do(func() {
				select {
				case errc <- err:
				default:
				}
			})
		}

		var workers sync.WaitGroup
		for i := 0; i < threads; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for j := range jobs {
					w.processDir(ctx, j, opts, paths, jobs, &wg)
					wg.Done()
				}
			}()
		}

		// The root-seeding loop below itself counts as outstanding work:
		// without this placeholder Add/Done pair, wg's count could
		// transiently hit zero *between* two roots (e.g. the first
		// root's directory job is fully processed, taking the count
		// back down to zero, before the loop advances and Add()s the
		// next root) — the closer goroutine below would then observe
		// wg.Wait() returning and close(jobs) while this loop still has
		// more roots left to enqueue, causing a send on a closed
		// channel. Holding one extra Add() for the whole seeding phase,
		// released only once every root has been considered, rules that
		// out. This Add() is deliberately placed before the closer
		// goroutine is even started: sync.WaitGroup requires that an
		// Add() which takes the counter away from zero happen-before
		// any Wait() call that could otherwise observe a zero counter
		// and return early (the package docs call out exactly this
		// ordering requirement), so starting the closer only after this
		// placeholder is in place avoids a racy/spurious early Wait()
		// return.
		wg.Add(1)

		// The only goroutine that closes jobs, and it does so exactly
		// once, exactly when wg's count has genuinely reached zero (see
		// the correctness argument in the Walk doc comment above).
		allDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(jobs)
			close(allDone)
		}()

	rootLoop:
		for _, root := range roots {
			info, err := os.Stat(root)
			if err != nil {
				reportErr(err)
				break rootLoop
			}

			if !info.IsDir() {
				// An explicitly named file is always searched,
				// regardless of ignore rules (matching rg's behavior
				// for explicit arguments).
				select {
				case paths <- root:
				case <-ctx.Done():
					break rootLoop
				}
				continue
			}

			wg.Add(1)
			select {
			case jobs <- dirJob{dir: root}:
			case <-ctx.Done():
				wg.Done()
				break rootLoop
			}
		}
		wg.Done() // release the root-seeding placeholder

		// Whether the root loop finished normally, hit a fatal Stat
		// error, or was cut short by ctx cancellation, every job that
		// *was* successfully enqueued is guaranteed to still be
		// processed to completion (each worker keeps draining jobs
		// until it's closed) or to fast-exit via its own ctx check --
		// either way wg's count still reaches zero, so waiting here
		// never hangs.
		<-allDone
		workers.Wait()
	}()

	return paths, errc
}

// processDir handles exactly one directory job: it lists the directory,
// loads its own ignore files, and for each entry either enqueues a new
// directory job (subdirectories that survive VCS/ignore/glob filtering)
// or sends a file path on paths (files that survive ignore/glob
// filtering). The walker deliberately has no notion of which files are
// "real documents" versus something like an MS Office lock file
// (~$report.docx) -- that's the registry/extractor layer's job (each
// format's Sniff naturally rejects such placeholder content), so a new
// format's quirks never require a change here. See
// internal/adapters/extract/all's regression test for the lock-file
// case specifically.
//
// Every subdirectory discovered here causes exactly one wg.Add(1), paired
// with exactly one wg.Done() once that subdirectory's own job finishes --
// whether it finishes by running to completion, by bailing out early
// because ctx was cancelled, or by being handed off to another worker via
// the shared jobs channel. That pairing is what makes the WaitGroup-based
// termination detection correct; see the Walk doc comment for the full
// argument.
func (w *Walker) processDir(ctx context.Context, j dirJob, opts domain.SearchOptions, paths chan<- string, jobs chan dirJob, wg *sync.WaitGroup) {
	if ctx.Err() != nil {
		return
	}

	entries, err := os.ReadDir(j.dir)
	if err != nil {
		// Unreadable directory (permissions, race with deletion,
		// etc): skip its subtree rather than aborting the whole walk,
		// matching the old walker's fs.SkipDir-on-readdir-error
		// behavior.
		return
	}

	own := &ruleNode{dir: j.dir, sets: loadDirIgnoreSets(j.dir), parent: j.parent}

	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}

		base := entry.Name()
		path := filepath.Join(j.dir, base)

		if entry.IsDir() {
			if vcsDirs[base] {
				continue
			}
			// own (this directory's chain, i.e. its ancestors plus
			// its own ignore files) decides whether the subdirectory
			// itself is ignored -- matching the old walker, where a
			// directory's own ignore file affects its children, not
			// whether the directory itself gets skipped.
			if !opts.NoIgnore && isIgnored(own, path, true) {
				continue
			}
			if !matchesGlobFilters(opts, path, base, true) {
				continue
			}

			w.enqueueOrProcessInline(ctx, dirJob{dir: path, parent: own}, opts, paths, jobs, wg)
			continue
		}

		// Regular file (or symlink etc.) candidate.
		if !opts.NoIgnore && isIgnored(own, path, false) {
			continue
		}
		if !matchesGlobFilters(opts, path, base, false) {
			continue
		}

		select {
		case paths <- path:
		case <-ctx.Done():
			return
		}
	}
}

// enqueueOrProcessInline hands child off to the shared jobs channel when
// there's room, or processes it right here (synchronously, recursing
// into processDir) when the channel is momentarily full. The
// non-blocking send is what prevents the classic bounded-work-queue
// self-deadlock: workers are the channel's only consumers, so a worker
// that unconditionally blocked trying to *produce* into a full channel
// could wedge the whole pool if every other worker were doing the same
// thing at the same instant. Falling back to inline processing instead
// means a worker only ever blocks waiting to *receive* new work (a
// perfectly normal idle wait), never trying to hand work off.
//
// wg.Add(1) happens unconditionally before either path, and the matching
// wg.Done() is supplied by whichever path actually runs: the worker loop
// in Walk (for a successful hand-off) or the explicit call here (for the
// inline fallback).
func (w *Walker) enqueueOrProcessInline(ctx context.Context, child dirJob, opts domain.SearchOptions, paths chan<- string, jobs chan dirJob, wg *sync.WaitGroup) {
	if ctx.Err() != nil {
		return
	}

	wg.Add(1)
	select {
	case jobs <- child:
		return
	default:
	}

	// Queue is momentarily full: do the work ourselves instead of
	// blocking. Recursion depth here is bounded by the tree's
	// directory depth (typically tens of levels at most), never by its
	// total size.
	w.processDir(ctx, child, opts, paths, jobs, wg)
	wg.Done()
}

// loadDirIgnoreSets reads any ignore files present directly inside dir.
func loadDirIgnoreSets(dir string) []PatternSet {
	var sets []PatternSet
	for _, name := range ignoreFileNames {
		lines, err := readLines(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		ps := ParsePatternLines(lines)
		if !ps.Empty() {
			sets = append(sets, ps)
		}
	}
	return sets
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(data), "\n"), nil
}

// isIgnored evaluates path against every ignore-rule scope in node's
// chain, from the root down to path's immediate parent, in order, so
// that a deeper (more specific) ignore file's rules are applied after
// (and can override, via negation) a shallower one's -- matching how git
// itself layers nested .gitignore files. It walks the persistent
// parent-pointer chain via recursion (root-most first, since we recurse
// into the parent before applying our own rules) rather than needing a
// materialized root-to-leaf slice.
func isIgnored(node *ruleNode, path string, isDir bool) bool {
	return evalChain(node, path, isDir)
}

func evalChain(node *ruleNode, path string, isDir bool) bool {
	if node == nil {
		return false
	}
	ignored := evalChain(node.parent, path, isDir)

	rel, err := filepath.Rel(node.dir, path)
	if err != nil || rel == "." {
		return ignored
	}
	rel = filepath.ToSlash(rel)
	for _, ps := range node.sets {
		for _, p := range ps.patterns {
			if p.match(rel, isDir) {
				ignored = !p.negate
			}
		}
	}
	return ignored
}

func matchesGlobFilters(opts domain.SearchOptions, path, base string, isDir bool) bool {
	if isDir {
		return true // never filter directories out of descent based on include/exclude
	}
	if len(opts.ExcludeGlobs) > 0 && matchAnyGlob(opts.ExcludeGlobs, path, base) {
		return false
	}
	if len(opts.IncludeGlobs) > 0 && !matchAnyGlob(opts.IncludeGlobs, path, base) {
		return false
	}
	return true
}

func matchAnyGlob(globs []string, path, base string) bool {
	for _, g := range globs {
		if ok, _ := filepath.Match(g, base); ok {
			return true
		}
		if ok, _ := filepath.Match(g, path); ok {
			return true
		}
	}
	return false
}
