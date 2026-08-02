# ogrep

A ripgrep-style command-line search tool that searches plain text
files, MS Office documents (`.docx`, `.pptx`, `.xlsx`), OpenDocument
files (`.odt`, `.ods`, `.odp`), and structured data files (`.json`,
`.jsonc`, `.jsonl`/`.ndjson`, `.yaml`/`.yml`, `.xml`, `.toml`). Most
formats stream through each document instead of loading it fully into
memory; YAML, JSONC, and TOML are the exceptions, each parsed as a
size-bounded in-memory tree (capped at 64 MiB per file for YAML/JSONC,
8 MiB for TOML -- real-world TOML files are config-file-sized, not
tens of megabytes) since none of them has a streaming parser that also
tracks the path/line information this tool needs.

```
ogrep [flags] PATTERN [PATH...]
```

If no `PATH` is given, the current directory is searched. `PATTERN` is
a regular expression by default; use `-F`/`--fixed-strings` for a
literal search. Run `ogrep --help` for the full flag reference
(context lines, `--type` format filtering, JSON output, and more).

## Usage examples

Search a directory tree (plain text and any
`.docx`/`.pptx`/`.xlsx`/`.odt`/`.ods`/`.odp` files in it)
case-insensitively:

```
$ ogrep -i budget .
todo.txt:1 Finish the quarterly budget review
notes/meeting.txt:1 Budget approved for Q3.
notes/meeting.txt:2 Next steps: circulate the budget doc.
```

Each match is printed as one `path:location` line followed by the
matched text — `location` is format-specific (the line number for
text, `Paragraph N` for docx, `Slide N (Shape "...")` for pptx,
`Sheet1!B45` for xlsx, a jq/yq-pasteable path like `.foo.bar[2]` for
json/yaml, an XPath like `/root/items/item[3]/name` for xml). The
OpenDocument formats mirror their MS Office counterparts: odt reports
the nearest preceding heading (same idea as docx's `Paragraph N`, just
navigable), ods reports `Sheet1:B45` (same cell addressing as xlsx),
and odp reports `Slide N`/`Slide N (Notes)` (same as pptx). When
stdout is a real terminal, the location is also wrapped in an OSC 8
hyperlink so an editor can jump straight to the match; piped or
redirected output prints the plain text with no hyperlink. Add
`-c`/`--count` to print just a match count per file instead:

```
$ ogrep -i -c budget .
todo.txt:1
notes/meeting.txt:2
```

Restrict the search to one document format regardless of file
extension, and read JSON instead of terminal output:

```
$ ogrep --type xlsx --json total .
```

The OpenDocument equivalents work the same way. An odt paragraph
match reports the nearest heading, e.g.:

```
$ ogrep --type odt "quarterly review" report.odt
report.odt:Q3 Financials Finish the quarterly review by Friday
```

An ods cell match reports its sheet and cell reference, exactly like
xlsx:

```
$ ogrep --type ods total budget.ods
budget.ods:Summary:B12 Total
```

ODF's `table:number-columns-repeated`/`table:number-rows-repeated`
compactly encode "this cell/row repeats N times", almost always used
for a large trailing run of empty padding cells that LibreOffice
commonly writes. ogrep reports a repeated cell/row group only once, at
its first cell/row reference, matching that common blank-padding case
-- but this means a repeated run of genuinely identical, non-blank
content (e.g. a column holding the same constant value, or a
filled-down formula result) is also only reported once, not once per
occurrence: a search will find the value but won't report every
individual cell it actually appears in. The JSON/`Fields()` output does
include a `repeat` count alongside the one reported location, so a
caller that needs to know how many times a value actually occurs can
at least learn that, even though only one location is emitted.

And an odp match reports its slide number, plus `(Notes)` when the
match is in that slide's speaker notes rather than a shape on the
slide itself:

```
$ ogrep --type odp agenda deck.odp
deck.odp:Slide 1 Meeting Agenda
deck.odp:Slide 4 (Notes) remember to mention the agenda change
```

JSON and YAML files are flattened into `<path> = <value>` lines using
jq/yq path syntax, so a matched line's path can be pasted straight into
`jq`/`yq` for further extraction:

```
$ ogrep foo-bar config.json
config.json:.["foo-bar"] .["foo-bar"] = 1
$ jq '.["foo-bar"]' config.json
1
```

(Non-identifier keys — dashes, spaces, leading digits — render in the
bracketed `.["key"]` form since jq's bare `.key` shorthand would
otherwise misparse them, e.g. as subtraction for a key like `foo-bar`.)
XML matches are instead located by an absolute XPath expression, e.g.
`/root/items/item[3]/name`.

Multi-document YAML files (`---`-separated) get each document's paths
prefixed with `.document[N]` (0-indexed) to keep them distinct — e.g.
`.document[1].limits.["foo-bar"]`. That prefix isn't itself valid
jq/yq syntax; to select the same value in yq, use its own
document-selection syntax instead of pasting the prefix directly:

```
$ yq eval 'select(document_index==1).limits.["foo-bar"]' config.yaml
```

`.jsonc` (JSON With Commas and Comments — comments, trailing commas)
and `.jsonl`/`.ndjson` (JSON Lines — one JSON value per line) are also
supported. JSONC comments are matched like any other text, located by
line and column rather than a jq path (they aren't addressable JSON
values):

```
$ ogrep "owning team" config.jsonc
config.jsonc:line 2:25 (comment) // owning team: billing-core
```

JSONL matches use the same jq-path notation as plain JSON — with no
per-line prefix, since `jq`'s default input mode already applies a
filter to every top-level value in the file, so `jq '.user'` run
against a whole `.jsonl` file works exactly as expected without any
document-index selector:

```
$ ogrep Grace events.jsonl
events.jsonl:.user (line 2:17) .user = "Grace"
```

`.toml` files report the jq/yq-style path as the location for a value
match, and a bare line number for a comment match — the underlying
parser (go-toml/v2's `unstable` package) still tracks exact
line/column for both, available in `--json` output (as `line`/`col`,
alongside `tomlpath` for values) and in the OSC 8 hyperlink used for
click-to-navigate, but the console-facing location stays a single
short, identifying label rather than repeating that position inline.
The value's own text is the real, verbatim line from the file, not a
reconstruction, so a hex/octal/binary integer literal or a legacy
escape sequence keeps its exact source spelling:

```
$ ogrep alpha config.toml
config.toml:.servers[0].name name = "alpha"
```

The path is also available on its own in `--json` output (as
`tomlpath`, alongside `line`/`col`), for scripting against a specific
key:

```
$ ogrep --json alpha config.toml
{"col":8,"format":"toml","line":7,"path":"config.toml","spans":[{"start":8,"end":13}],"text":"name = \"alpha\"","tomlpath":".servers[0].name","type":"match","uri":"file://config.toml:7:8"}
```

TOML comments are searchable too, each reported as its own match at its
real line (marked `"comment":true` in `--json` output):

```
$ ogrep "comment mentioning" config.toml
config.toml:14 # comment mentioning alpha for search
```

By default, `.gitignore` and `.ogrepignore` files are respected the
same way git respects `.gitignore` — nested files layer, with a
deeper file's rules (including negation with `!`) overriding a
shallower one's. Pass `--no-ignore` to search everything anyway,
including files those ignore rules would normally exclude.

## Installing

### From a release archive

Prebuilt, statically-linked binaries (no runtime dependencies) for
Linux, macOS, and Windows on amd64/arm64 are attached to each
[release](../../releases) once releases are being published. Download
the archive for your platform, extract it, and put the `ogrep`
binary on your `PATH`.

### With `go install`

```sh
go install github.com/laraibg786/ogrep/cmd/ogrep@latest
```

This only works once this repository has actually been pushed to
`github.com/laraibg786/ogrep`, which has not happened yet as of this
writing — the module path is set up in anticipation of that, but there
is no remote to install from today.

### From source

Requires Go (see `go.mod` for the minimum version). From the repo
root:

```sh
go build -o ogrep ./cmd/ogrep
```

This produces an `ogrep` binary in the current directory. Since
the project has no CGO dependencies, a plain build is already static;
you can drop `CGO_ENABLED=0` in explicitly for a fully hermetic build
that doesn't depend on a local C toolchain being available at all:

```sh
CGO_ENABLED=0 go build -o ogrep ./cmd/ogrep
```

Check the installed version with:

```sh
ogrep --version
```

(A source build reports `ogrep dev` since the version string is
only stamped in by the release process below.)

## Releasing (maintainers)

Releases are built with [goreleaser](https://goreleaser.com) from the
`.goreleaser.yaml` config at the repo root, which cross-compiles
static (`CGO_ENABLED=0`) binaries for linux/darwin/windows on
amd64/arm64, packages them into `.tar.gz` (`.zip` on Windows) archives
alongside this README and a LICENSE, and writes a `checksums.txt`.

To try the build locally without needing a git tag, a remote, or a
publish token:

```sh
goreleaser release --snapshot --clean --skip=publish
```

Artifacts land in `dist/` (gitignored).

To cut a real, published release, this repo will first need an actual
git remote (e.g. on GitHub) and a `GITHUB_TOKEN` with permission to
create releases on it — neither exists yet as of this writing. Once
they do, a release is just:

```sh
git tag vX.Y.Z
git push --tags
goreleaser release --clean
```

goreleaser picks the version up from the git tag and stamps it into
each binary (`ogrep --version`) via ldflags.

Once this repo has a real remote on GitHub, pushing a `vX.Y.Z`-shaped
tag (`git push --tags` above) also triggers `.github/workflows/
release.yml` automatically: it runs the test suite as a safety gate
and then runs `goreleaser release --clean` in CI via the
`goreleaser-action`, using the repo's automatically-provided
`GITHUB_TOKEN`, so no local `goreleaser` invocation is required at all
for a normal release. The manual steps above remain valid too — they're
still how to do a local dry run (`--snapshot --skip=publish`), and they
still work for a real release for anyone releasing from a machine
outside CI. `.goreleaser.yaml`'s `release.disable` field is templated
so it only actually attempts to publish when running inside GitHub
Actions (`GITHUB_ACTIONS=true`); every other environment, including
local dry runs in a repo with no remote, keeps skipping the release
pipe exactly as before.

`.github/workflows/ci.yml` separately runs `gofmt`, `go vet`, a build,
and `go test -race` on every push to `main` and every pull request, plus
a lightweight build-only sanity check on `ubuntu-latest`, `macos-latest`,
and `windows-latest` (goreleaser cross-compiles for all three, so a
platform-specific compile break is worth catching before release time).

## License

No `LICENSE` file has been added to this repository yet — that's a
decision for the project owner to make. Until one exists, no license
is granted for use, so treat this repository as "all rights reserved"
by default.
