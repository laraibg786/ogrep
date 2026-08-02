package all

import (
	"os"
	"path/filepath"
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
