package all

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laraibg786/ogrep/internal/registry"
)

// TestLockFilesAreNotRecognizedByAnyFormat is a regression test for the
// walker's file-walking having no special-cased knowledge of MS
// Office's transient lock files (e.g. "~$report.docx", created while a
// document is open in Word/PowerPoint/Excel): none of docx/pptx/xlsx's
// Sniff implementations ever validate that tiny placeholder content as
// a real zip package, and text's Sniff explicitly refuses the three
// OOXML extensions regardless of content, so a lock file is silently
// unrecognized by every registered extractor here -- exactly like any
// other file no format claims -- without the walker needing to know
// these three extensions are special at all.
func TestLockFilesAreNotRecognizedByAnyFormat(t *testing.T) {
	for _, name := range []string{"~$report.docx", "~$notes.pptx", "~$budget.xlsx"} {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte("Jane Doe"), 0o644); err != nil {
			t.Fatal(err)
		}

		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			t.Fatal(err)
		}

		if _, ok := registry.Default.For(path, f, info.Size()); ok {
			t.Errorf("%s: expected no extractor to claim a lock file, but one did", name)
		}
		f.Close()
	}
}

// claim opens a file at path with the given content and returns the
// name of whichever registered extractor claims it, or "" if none does.
func claim(t *testing.T, path string, content []byte) string {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := registry.Default.For(path, f, info.Size())
	if !ok {
		return ""
	}
	return e.Name()
}

// TestValidJSONClaimedByDedicatedPlugin is a regression test for the
// registry dispatch ordering: a valid JSON file must be claimed by the
// dedicated jsondoc extractor, not by the generic text fallback --
// since text.Extensions() no longer hints ".json" (see text.go),
// jsondoc must win via its own extension hint, not by accident of
// registration order.
func TestValidJSONClaimedByDedicatedPlugin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	if got := claim(t, path, []byte(`{"a": 1}`)); got != "json" {
		t.Errorf("doc.json: claimed by %q, want %q", got, "json")
	}
}

// TestMalformedJSONFallsBackToText is a regression test for the
// graceful-degradation contract: a .json file whose content doesn't
// actually parse as JSON from its very first token must fall back to
// the permissive text extractor, not be claimed by no one.
//
// This is deliberately broken from the very first token (not, say,
// `{"a": `): jsondoc's own Sniff only trial-decodes the first token (a
// deliberate tradeoff -- checking further tokens, or fully parsing,
// risks false-rejecting large valid documents, or costs as much as
// Extract itself just to decide dispatch), so content starting with a
// valid opening token still passes Sniff -- and is claimed -- even if
// it's broken deeper in; that case is NOT a dispatch-fallback scenario,
// it's an Extract-time error-channel scenario, already covered by
// jsondoc's own tests.
func TestMalformedJSONFallsBackToText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.json")
	if got := claim(t, path, []byte("totally not json despite the extension")); got != "text" {
		t.Errorf("broken.json: claimed by %q, want fallback to %q", got, "text")
	}
}

// TestValidYAMLClaimedByDedicatedPlugin mirrors
// TestValidJSONClaimedByDedicatedPlugin for YAML, covering both the
// ".yaml" and ".yml" extensions.
func TestValidYAMLClaimedByDedicatedPlugin(t *testing.T) {
	for _, name := range []string{"doc.yaml", "doc.yml"} {
		path := filepath.Join(t.TempDir(), name)
		if got := claim(t, path, []byte("a: 1\n")); got != "yaml" {
			t.Errorf("%s: claimed by %q, want %q", name, got, "yaml")
		}
	}
}

// TestMalformedYAMLFallsBackToText mirrors
// TestMalformedJSONFallsBackToText for YAML.
func TestMalformedYAMLFallsBackToText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.yaml")
	if got := claim(t, path, []byte("a: [1, 2\n")); got != "text" {
		t.Errorf("broken.yaml: claimed by %q, want fallback to %q", got, "text")
	}
}

// TestProseYAMLFallsBackToText is a regression test for yamldoc's
// structure gate: goccy/go-yaml parses arbitrary plain text as a valid
// bare scalar document (see yamldoc's hasStructure doc comment), so
// plain prose with a .yaml extension but no real mapping/sequence
// structure must still fall back to text rather than being claimed by
// yamldoc.
func TestProseYAMLFallsBackToText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prose.yaml")
	if got := claim(t, path, []byte("just some prose, not a mapping or sequence\n")); got != "text" {
		t.Errorf("prose.yaml: claimed by %q, want fallback to %q", got, "text")
	}
}

// TestOversizedYAMLFallsBackToText is a regression test for yamldoc's
// size-gated Sniff: since goccy/go-yaml builds a full in-memory AST with
// no streaming alternative, a file above its size gate must decline the
// claim in Sniff so the registry falls back to text's streaming,
// size-agnostic plain-text grep instead of risking an unbounded-memory
// parse.
func TestOversizedYAMLFallsBackToText(t *testing.T) {
	// One well-formed "line" repeated past the 64 MiB gate yamldoc uses
	// (see maxSniffSize in yamldoc.go); the content is valid YAML in
	// isolation, so only the size gate -- not a parse failure -- should
	// be responsible for declining the claim.
	const oversize = 65 * 1024 * 1024

	yamlLine := "a: 1\n"
	yamlContent := make([]byte, 0, oversize+len(yamlLine))
	for len(yamlContent) < oversize {
		yamlContent = append(yamlContent, yamlLine...)
	}

	path := filepath.Join(t.TempDir(), "big.yaml")
	if got := claim(t, path, yamlContent); got != "text" {
		t.Errorf("big.yaml: claimed by %q, want fallback to %q for an oversized file", got, "text")
	}
}

// TestValidXMLClaimedByDedicatedPlugin mirrors
// TestValidJSONClaimedByDedicatedPlugin for XML.
func TestValidXMLClaimedByDedicatedPlugin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.xml")
	if got := claim(t, path, []byte(`<root><a>1</a></root>`)); got != "xml" {
		t.Errorf("doc.xml: claimed by %q, want %q", got, "xml")
	}
}

// TestMalformedXMLFallsBackToText mirrors TestMalformedJSONFallsBackToText
// for XML: broken from the very first token (an unclosed opening tag),
// so xmldoc's own cheap Sniff (a single-token trial decode, the same
// tradeoff jsondoc makes) correctly declines it.
func TestMalformedXMLFallsBackToText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.xml")
	if got := claim(t, path, []byte(`<root`)); got != "text" {
		t.Errorf("broken.xml: claimed by %q, want fallback to %q", got, "text")
	}
}

// TestLargeXMLIsClaimedByXmldocNotText confirms xmldoc's streaming
// design actually delivers what it promises: a file well beyond any size
// a DOM-based parser would want to buffer whole is still claimed by
// xmldoc, not declined into a text-plugin fallback -- there's no size
// gate to trigger one (unlike yamldoc). Uses a few thousand elements
// (large enough to meaningfully exceed a "toy" fixture, small enough not
// to slow the test suite down) rather than the tens-of-megabytes scale
// of yamldoc's own size-gate test, since there's no threshold here to
// cross in the first place.
func TestLargeXMLIsClaimedByXmldocNotText(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<root>")
	for i := 0; i < 20_000; i++ {
		fmt.Fprintf(&sb, "<item id=\"%d\">value %d</item>", i, i)
	}
	sb.WriteString("</root>")

	path := filepath.Join(t.TempDir(), "large.xml")
	if got := claim(t, path, []byte(sb.String())); got != "xml" {
		t.Errorf("large.xml: claimed by %q, want %q", got, "xml")
	}
}

// TestValidJSONCClaimedByDedicatedPlugin mirrors
// TestValidJSONClaimedByDedicatedPlugin for JSONC (JSON With Commas and
// Comments): jsoncdoc fully parses in Sniff (unlike jsondoc/xmldoc's
// cheap first-token trial decode), so it can confidently claim content
// with comments/trailing commas that would fail jsondoc's own Sniff.
func TestValidJSONCClaimedByDedicatedPlugin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.jsonc")
	if got := claim(t, path, []byte("// comment\n{\"a\": 1,}\n")); got != "jsonc" {
		t.Errorf("doc.jsonc: claimed by %q, want %q", got, "jsonc")
	}
}

// TestMalformedJSONCFallsBackToText mirrors
// TestMalformedJSONFallsBackToText for JSONC: content that doesn't look
// JSON-ish at all (not even a valid opening token) must fall back to
// text. See TestJSONCFallsBackToJSONNotTextWhenContentLooksLikeJSON
// below for the more surprising case where malformed-but-JSON-shaped
// content lands on jsondoc instead.
func TestMalformedJSONCFallsBackToText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.jsonc")
	if got := claim(t, path, []byte("totally not json despite the extension")); got != "text" {
		t.Errorf("broken.jsonc: claimed by %q, want fallback to %q", got, "text")
	}
}

// TestJSONCFallsBackToJSONNotTextWhenContentLooksLikeJSON documents a
// real, non-obvious dispatch nuance: when jsoncdoc's full-parse Sniff
// correctly declines malformed JSONC, the registry's fallback pool
// doesn't necessarily land on text next -- it lands on whichever
// "rest"-group extractor's own Sniff says yes first, in registration
// order, and jsondoc has no extension awareness at all (its cheap Sniff
// only trial-decodes the first token of whatever bytes it's given). So
// malformed-but-json-shaped content with a .jsonc extension is claimed
// by jsondoc, not text, once jsoncdoc declines -- text is only reached
// when the content doesn't look JSON-ish at all (see
// TestMalformedJSONCFallsBackToText above, which is deliberately not
// JSON-shaped, to actually reach text).
func TestJSONCFallsBackToJSONNotTextWhenContentLooksLikeJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.jsonc")
	// Starts with a valid "{" token (jsondoc's cheap Sniff accepts this)
	// but is truncated, so jsoncdoc's full parse correctly declines it.
	if got := claim(t, path, []byte(`{"a": `)); got != "json" {
		t.Errorf("broken.jsonc (json-shaped but truncated): claimed by %q, want %q", got, "json")
	}
}

// TestValidJSONLClaimedByDedicatedPlugin mirrors
// TestValidJSONClaimedByDedicatedPlugin for JSONL/NDJSON, covering both
// the ".jsonl" and ".ndjson" extensions.
func TestValidJSONLClaimedByDedicatedPlugin(t *testing.T) {
	content := []byte(`{"a":1}` + "\n" + `{"a":2}` + "\n")
	for _, name := range []string{"doc.jsonl", "doc.ndjson"} {
		path := filepath.Join(t.TempDir(), name)
		if got := claim(t, path, content); got != "jsonl" {
			t.Errorf("%s: claimed by %q, want %q", name, got, "jsonl")
		}
	}
}

// TestMalformedJSONLFallsBackToText mirrors
// TestMalformedJSONFallsBackToText for JSONL: jsonldoc inherits
// jsondoc's exact first-token-trial-decode tradeoff (it delegates its
// own Sniff to jsondoc's Sniff on just the first line), so content
// broken from its first line's first token correctly falls back to
// text.
func TestMalformedJSONLFallsBackToText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.jsonl")
	if got := claim(t, path, []byte("totally not json despite the extension\n")); got != "text" {
		t.Errorf("broken.jsonl: claimed by %q, want fallback to %q", got, "text")
	}
}

// TestExistingOOXMLAndTextDispatchUnaffected is a regression test
// confirming the new structured-data plugins don't interfere with
// dispatch for the pre-existing formats: an unrelated, genuinely
// unknown extension still falls back to text, unchanged.
func TestExistingOOXMLAndTextDispatchUnaffected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if got := claim(t, path, []byte("just some plain notes\n")); got != "text" {
		t.Errorf("notes.txt: claimed by %q, want %q", got, "text")
	}
}
