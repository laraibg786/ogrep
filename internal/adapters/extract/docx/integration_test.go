package docx_test

// This file is an integration-style test that proves the docx plugin
// works through the REAL search pipeline (registry -> walker -> matcher
// -> orchestrator -> sink), not just in isolation against Extractor
// directly (see docx_test.go for the unit-level tests). It lives in the
// docx_test (external) package specifically so it only exercises the
// same public surface any other caller of the plugin would use:
// registry.Registry.Register + registry.Registry.For, wired into
// app.SearchOrchestrator exactly as cmd/ogrep does in production.

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/laraibg786/ogrep/internal/adapters/extract/docx"
	"github.com/laraibg786/ogrep/internal/adapters/match"
	"github.com/laraibg786/ogrep/internal/adapters/walk"
	"github.com/laraibg786/ogrep/internal/core/app"
	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// fakeSink collects matches in memory, mirroring the one in
// internal/core/app's own tests, so this test doesn't depend on the
// terminal/json output adapters' formatting.
type fakeSink struct {
	mu      sync.Mutex
	matches []domain.Match
}

func (s *fakeSink) WriteMatch(m domain.Match) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.matches = append(s.matches, m)
	return nil
}

func (s *fakeSink) WriteFileSummary(path string, count int) error { return nil }
func (s *fakeSink) Flush() error                                  { return nil }

const wNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`

func buildDocxFile(t *testing.T, path string) {
	t.Helper()

	doc := `<?xml version="1.0" encoding="UTF-8"?><w:document ` + wNS + `><w:body>` +
		`<w:p><w:r><w:t>nothing interesting here</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>the needle is in this paragraph</w:t></w:r></w:p>` +
		`<w:tbl><w:tr>` +
		`<w:tc><w:p><w:r><w:t>irrelevant</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>needle in a cell</w:t></w:r></w:p></w:tc>` +
		`</w:tr></w:tbl>` +
		`</w:body></w:document>`
	hdr := `<?xml version="1.0" encoding="UTF-8"?><w:hdr ` + wNS + `><w:p><w:r><w:t>needle in the header</w:t></w:r></w:p></w:hdr>`

	parts := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="xml" ContentType="application/xml"/></Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"></Relationships>`,
		"word/document.xml": doc,
		"word/header1.xml":  hdr,
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("creating zip part %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("writing zip part %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip writer: %v", err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
}

// TestDocxThroughRealOrchestrator builds a fresh registry containing
// only the docx Extractor, writes a fixture .docx to a real temp file
// on disk, and runs it through the real app.SearchOrchestrator (walker,
// literal matcher, and an in-memory sink), asserting the matches that
// come back carry the correct Location.Human strings. This is the
// end-to-end proof that the plugin is wired correctly, not just correct
// in isolation.
func TestDocxThroughRealOrchestrator(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.docx")
	buildDocxFile(t, path)

	reg := registry.New()
	reg.Register(docx.Extractor{})

	sink := &fakeSink{}
	orch := app.New(reg, walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "needle", []string{dir}, domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.FilesSearched != 1 {
		t.Fatalf("FilesSearched = %d, want 1", stats.FilesSearched)
	}
	if stats.TotalMatches != 3 {
		t.Fatalf("TotalMatches = %d, want 3, got matches: %+v", stats.TotalMatches, sink.matches)
	}

	var humans []string
	for _, m := range sink.matches {
		if m.Format != "docx" {
			t.Errorf("match Format = %q, want docx", m.Format)
		}
		if m.Path != path {
			t.Errorf("match Path = %q, want %q", m.Path, path)
		}
		humans = append(humans, m.Location.Human())
	}
	sort.Strings(humans)

	want := []string{"Header 1", "Paragraph 2", "Table 1, Row 1, Cell 2"}
	if len(humans) != len(want) {
		t.Fatalf("got Human labels %v, want %v", humans, want)
	}
	for i := range want {
		if humans[i] != want[i] {
			t.Errorf("Human labels = %v, want %v", humans, want)
		}
	}
}
