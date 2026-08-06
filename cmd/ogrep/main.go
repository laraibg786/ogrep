// Command ogrep is an rg-style CLI search tool specialized for
// searching inside MS Office files (docx/pptx/xlsx) as well as plain
// text.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	_ "go.uber.org/automaxprocs" // sets GOMAXPROCS from the cgroup CPU quota when running in a container/pod, so runtime.NumCPU()-based defaults (walker/orchestrator thread counts) don't oversubscribe; a documented no-op outside a container.

	_ "github.com/laraibg786/ogrep/internal/adapters/extract/all"
	"github.com/laraibg786/ogrep/internal/adapters/extract/text"
	"github.com/laraibg786/ogrep/internal/adapters/match"
	"github.com/laraibg786/ogrep/internal/adapters/output"
	"github.com/laraibg786/ogrep/internal/adapters/walk"
	"github.com/laraibg786/ogrep/internal/adapters/xdg"
	"github.com/laraibg786/ogrep/internal/core/app"
	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/core/ports"
	"github.com/laraibg786/ogrep/internal/registry"
)

// version is the ogrep release version. It defaults to "dev" for
// local/`go build`/`go run` invocations that don't set it explicitly.
// goreleaser overrides it at build time via -ldflags
// "-X main.version=vX.Y.Z" (see .goreleaser.yaml), pinning each release
// artifact to the git tag it was built from.
var version = "dev"

// buildLongHelp renders the CLI's long help text, interpolating the
// currently-registered extractor names (formats) so the text never goes
// stale as format plugins are added or removed; see
// registeredFormatNames.
func buildLongHelp(formats []string) string {
	return fmt.Sprintf(`ogrep searches for a pattern in files, including MS Office
documents and plain text, streaming through each document instead of
loading it fully into memory. Supported formats: %s.

Usage:

  ogrep [flags] PATTERN [PATH...]

If no PATH is given, the current directory is searched -- unless stdin
has real piped data (e.g. "cat file | ogrep pattern"), in which case
stdin itself is searched instead, matching ripgrep. A literal "-" also
always means stdin, wherever it appears in PATH. Stdin is always
searched as plain text, regardless of its actual content -- ogrep never
tries to sniff or parse a document format (docx/pptx/xlsx/etc.) from a
pipe. PATTERN is a regular expression by default; use -F/--fixed-strings
for a literal search.

Context lines (-A/-B/-C) and format filtering (--type) work like
ripgrep's: -C N shows N text units of context on both sides of a match
(overridden per-side by an explicit -A/-B), and --type restricts the
search to files recognized by a specific extractor (%s), independent of
file extension.

Config file:

  An optional TOML config file is read from
  $XDG_CONFIG_HOME/ogrep/config.toml (typically
  ~/.config/ogrep/config.toml) to seed default values for --color,
  --threads, and extra ignore globs. Command-line flags always override
  the config file.

Shell completion:

  This build includes cobra's built-in "completion" subcommand. Install
  it, e.g.:

    bash: ogrep completion bash > \
            "${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion/completions/ogrep"
    zsh:  ogrep completion zsh > "/path/on/\$fpath/_ogrep"
    fish: ogrep completion fish > \
            "$HOME/.config/fish/completions/ogrep.fish"

  Run "ogrep completion --help" for full details.
`, strings.Join(formats, "/"), strings.Join(formats, ", "))
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(2)
	}
}

// cliFlags holds the values bound to command-line flags, pre-seeded
// from the XDG config file (if present) so that config values act as
// defaults that flags can override.
type cliFlags struct {
	ignoreCase   bool
	fixedStrings bool
	wholeWord    bool
	invertMatch  bool
	color        string
	threads      int
	jsonOutput   bool

	maxCount int

	includeGlobs []string
	excludeGlobs []string
	noIgnore     bool
	types        []string

	filesWithMatches bool
	countOnly        bool

	stats bool

	contextBefore int
	contextAfter  int
	context       int
}

func newRootCmd() *cobra.Command {
	cfg, cfgErr := xdg.LoadConfig()

	flags := cliFlags{
		color:   "auto",
		threads: 0,
	}
	if cfg.Color != "" {
		flags.color = cfg.Color
	}
	if cfg.Threads > 0 {
		flags.threads = cfg.Threads
	}

	formats := registeredFormatNames()

	rootCmd := &cobra.Command{
		Use:           "ogrep PATTERN [PATH...]",
		Short:         "Search plain text and MS Office documents for a pattern",
		Long:          buildLongHelp(formats),
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfgErr != nil {
				fmt.Fprintf(os.Stderr, "ogrep: warning: reading config: %v\n", cfgErr)
			}
			return runSearch(cmd, args, flags, cfg)
		},
	}

	// Setting Version turns on cobra's built-in --version/-v handling
	// (it prints via SetVersionTemplate and also answers a synthesized
	// "version" behavior for --version). We use -V rather than cobra's
	// default -v short flag since -v is already taken here by
	// --invert-match.
	rootCmd.Version = version
	rootCmd.Flags().BoolP("version", "V", false, "print the ogrep version and exit")
	rootCmd.SetVersionTemplate("ogrep {{.Version}}\n")

	// Leave completion enabled: cobra's built-in "completion" command
	// (bash/zsh/fish/powershell) is generated automatically as long as
	// we don't disable it here.
	rootCmd.CompletionOptions.DisableDefaultCmd = false

	rootCmd.Flags().BoolVarP(&flags.ignoreCase, "ignore-case", "i", false, "case-insensitive search")
	rootCmd.Flags().BoolVarP(&flags.fixedStrings, "fixed-strings", "F", false, "treat PATTERN as a literal string, not a regex")
	rootCmd.Flags().BoolVarP(&flags.wholeWord, "word-regexp", "w", false, "only match whole words")
	rootCmd.Flags().BoolVarP(&flags.invertMatch, "invert-match", "v", false, "select text units that do NOT match PATTERN")
	rootCmd.Flags().StringVar(&flags.color, "color", flags.color, "when to colorize output: auto, always, never")
	rootCmd.Flags().IntVarP(&flags.threads, "threads", "j", flags.threads, "number of search worker threads (default: number of CPUs)")
	rootCmd.Flags().BoolVar(&flags.jsonOutput, "json", false, "emit JSON-lines output instead of terminal formatting")

	rootCmd.Flags().IntVarP(&flags.maxCount, "max-count", "m", 0, "stop after N matches per file (0 = unlimited)")

	rootCmd.Flags().StringArrayVar(&flags.includeGlobs, "include", nil, "only search files matching this glob (repeatable)")
	rootCmd.Flags().StringArrayVar(&flags.excludeGlobs, "exclude", nil, "skip files matching this glob (repeatable)")
	rootCmd.Flags().BoolVar(&flags.noIgnore, "no-ignore", false, "don't respect .gitignore/.ogrepignore files")
	rootCmd.Flags().StringArrayVar(&flags.types, "type", nil, fmt.Sprintf("only search files of this format: %s (repeatable)", strings.Join(formats, ", ")))

	rootCmd.Flags().BoolVarP(&flags.filesWithMatches, "files-with-matches", "l", false, "print only the paths of files with a match, one per line")
	rootCmd.Flags().BoolVarP(&flags.countOnly, "count", "c", false, "print only \"path:count\" per matching file, instead of each match")

	rootCmd.Flags().BoolVar(&flags.stats, "stats", false, "print summary search statistics after the results")

	rootCmd.Flags().IntVarP(&flags.contextBefore, "before-context", "B", 0, "show N text units of context before each match")
	rootCmd.Flags().IntVarP(&flags.contextAfter, "after-context", "A", 0, "show N text units of context after each match")
	rootCmd.Flags().IntVarP(&flags.context, "context", "C", 0, "show N text units of context before and after each match (an explicit -A/-B overrides that side)")

	return rootCmd
}

// resolveRoots applies ogrep's rg-like defaulting: an explicit path
// list is used as given (including a literal "-" for stdin, handled by
// the orchestrator wherever it appears). With no paths at all, ogrep
// searches "." unless stdin actually looks like real piped data
// (stdinHasPipedData), in which case it reads stdin itself -- via the
// same "-" convention -- e.g. `cat file | ogrep pattern`.
func resolveRoots(pathArgs []string, stdinHasPipedData bool) []string {
	if len(pathArgs) > 0 {
		return pathArgs
	}
	if stdinHasPipedData {
		return []string{domain.StdinPath}
	}
	return []string{"."}
}

// stdinHasPipedData reports whether f looks like it actually carries
// data worth reading -- a regular file, a FIFO (a real `cmd | ogrep`
// pipe), or a socket -- as opposed to a character device or an
// unreadable/closed descriptor.
//
// This deliberately is NOT "stdin is not a terminal": a tty and
// /dev/null are both character devices, so term.IsTerminal(f) being
// false does not mean there's anything to read -- cron jobs, CI
// pipelines, and `docker run` (without -i) all give a process /dev/null
// on stdin, none of them a real pipe. Treating that the same as a
// genuine piped-in pattern search would silently replace "search the
// current directory" (today's, and every prior release's, behavior)
// with "search nothing" for every such invocation. Stat erroring (e.g.
// a closed fd 0) is treated the same as "nothing to read", the same
// fail-safe direction.
func stdinHasPipedData(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	mode := info.Mode()
	return mode.IsRegular() || mode&os.ModeNamedPipe != 0 || mode&os.ModeSocket != 0
}

func runSearch(cmd *cobra.Command, args []string, flags cliFlags, cfg xdg.Config) error {
	pattern := args[0]
	roots := resolveRoots(args[1:], stdinHasPipedData(os.Stdin))

	if flags.filesWithMatches && flags.countOnly {
		return fmt.Errorf("-l/--files-with-matches and -c/--count are mutually exclusive")
	}

	if len(flags.types) > 0 {
		if err := validateTypes(flags.types); err != nil {
			return err
		}
	}

	// -C/--context sets both sides to the same value, but an explicit
	// -A/-B for a given side overrides -C for that side only, matching
	// ripgrep's own convention.
	contextBefore := flags.contextBefore
	contextAfter := flags.contextAfter
	if flags.context > 0 {
		if !cmd.Flags().Changed("before-context") {
			contextBefore = flags.context
		}
		if !cmd.Flags().Changed("after-context") {
			contextAfter = flags.context
		}
	}

	opts := domain.SearchOptions{
		IgnoreCase:   flags.ignoreCase,
		FixedStrings: flags.fixedStrings,
		WholeWord:    flags.wholeWord,
		InvertMatch:  flags.invertMatch,
		NoIgnore:     flags.noIgnore,

		MaxCount: flags.maxCount,

		ContextBefore: contextBefore,
		ContextAfter:  contextAfter,

		IncludeGlobs: flags.includeGlobs,
		ExcludeGlobs: append(append([]string{}, cfg.Ignore...), flags.excludeGlobs...),
		Types:        flags.types,

		Threads: flags.threads,

		FilesWithMatches: flags.filesWithMatches,
		CountOnly:        flags.countOnly,
	}

	var sink ports.OutputSink
	if flags.jsonOutput {
		sink = output.NewJSON(cmd.OutOrStdout())
	} else {
		mode := output.ColorMode(flags.color)
		switch mode {
		case output.ColorAuto, output.ColorAlways, output.ColorNever:
		default:
			return fmt.Errorf("invalid --color value %q (want auto, always, or never)", flags.color)
		}
		summary := output.SummaryModeOff
		switch {
		case flags.filesWithMatches:
			summary = output.SummaryModePathOnly
		case flags.countOnly:
			summary = output.SummaryModeCount
		}
		sink = output.NewTerminal(cmd.OutOrStdout(), mode, os.Stdout, summary)
	}

	orch := app.New(registry.Default, walk.New(), match.NewFactory(), sink)
	orch.StdinExtractor = text.Extractor{}

	stats, err := orch.Run(context.Background(), pattern, roots, opts)
	if err != nil {
		return err
	}

	if flags.stats {
		fmt.Fprintf(cmd.OutOrStdout(), "\n%d files walked\n%d files searched\n%d files matched\n%d total matches\n",
			stats.FilesWalked, stats.FilesSearched, stats.FilesMatched, stats.TotalMatches)
	}

	if stats.TotalMatches == 0 {
		os.Exit(1)
	}
	return nil
}

// registeredFormatNames returns the Name() of every extractor currently
// registered in registry.Default, sorted for stable/reproducible output.
// Both validateTypes' error message and the CLI help text below build
// their format lists from this, so a new format plugin only needs to
// self-register (see internal/adapters/extract/all) to show up
// everywhere ogrep mentions supported formats.
func registeredFormatNames() []string {
	registered := registry.Default.All()
	names := make([]string, 0, len(registered))
	for _, e := range registered {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// validateTypes checks each requested --type value against the actually
// registered extractors' Name()s, returning a clear error listing valid
// choices if an unknown type is given, rather than silently matching no
// files.
func validateTypes(types []string) error {
	names := registeredFormatNames()
	valid := make(map[string]bool, len(names))
	for _, n := range names {
		valid[n] = true
	}

	for _, t := range types {
		if !valid[t] {
			return fmt.Errorf("unknown --type %q (valid types: %s)", t, strings.Join(names, ", "))
		}
	}
	return nil
}
