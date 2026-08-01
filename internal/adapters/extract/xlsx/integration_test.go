package xlsx_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/laraibg786/ogrep/internal/adapters/extract/xlsx"
	"github.com/laraibg786/ogrep/internal/adapters/match"
	"github.com/laraibg786/ogrep/internal/adapters/walk"
	"github.com/laraibg786/ogrep/internal/core/app"
	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// fakeSink collects matches in memory, mirroring the orchestrator's own
// test helper, so this test can assert on real end-to-end search
// results without depending on a specific terminal/json rendering.
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

// TestIntegrationOrchestratorFindsXlsxMatches builds a fresh registry
// containing only the xlsx Extractor, writes a real xlsx fixture to a
// temp file on disk, and drives it through the real
// app.SearchOrchestrator end to end, confirming matches come back with
// correctly-formed Location.Human strings.
func TestIntegrationOrchestratorFindsXlsxMatches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.xlsx")

	data := buildXlsx(t, multiSheetFixture())
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}

	reg := registry.New()
	reg.Register(xlsx.Extractor{})

	sink := &fakeSink{}
	orch := app.New(reg, walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "Second Sheet Value", []string{dir}, domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.TotalMatches != 1 {
		t.Fatalf("TotalMatches = %d, want 1", stats.TotalMatches)
	}

	m := sink.matches[0]
	if m.Location.Human() != "Budget 2024:B45" {
		t.Errorf("Location.Human() = %q, want %q", m.Location.Human(), "Budget 2024:B45")
	}
	if m.Path != path {
		t.Errorf("Path = %q, want %q", m.Path, path)
	}
	if m.Format != "xlsx" {
		t.Errorf("Format = %q, want %q", m.Format, "xlsx")
	}
}

// TestIntegrationOrchestratorRegexAcrossSheets confirms a regex pattern
// matches cells spread across multiple resolved sheets, exercising the
// full walk -> sniff -> extract -> match -> sink pipeline with more
// than one match.
func TestIntegrationOrchestratorRegexAcrossSheets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbook.xlsx")

	data := buildXlsx(t, multiSheetFixture())
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}

	reg := registry.New()
	reg.Register(xlsx.Extractor{})

	sink := &fakeSink{}
	orch := app.New(reg, walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "^(Hello World|FormulaResult)$", []string{dir}, domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.TotalMatches != 2 {
		t.Fatalf("TotalMatches = %d, want 2", stats.TotalMatches)
	}

	var humans []string
	for _, m := range sink.matches {
		humans = append(humans, m.Location.Human())
	}
	wantSet := map[string]bool{"Sheet1:A1": true, "Budget 2024:C1": true}
	for _, h := range humans {
		if !wantSet[h] {
			t.Errorf("unexpected match location %q", h)
		}
		delete(wantSet, h)
	}
	if len(wantSet) != 0 {
		t.Errorf("missing expected matches: %v", wantSet)
	}
}

// TestIntegrationContextLinesOperateOnRealLinesNotWholeCell is a
// regression test for the line-splitting fix, mirroring the docx
// package's identical test: -A/-C context must return the single
// adjacent LINE within a cell containing a manual line break, not the
// whole remaining multi-line text of that cell (which is what would
// happen if a cell's lines were still joined into one TextUnit with an
// embedded "\n" -- context lines are bounded by TextUnit boundaries).
func TestIntegrationContextLinesOperateOnRealLinesNotWholeCell(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.xlsx")

	cellXML := `<c r="A1" t="inlineStr"><is><t xml:space="preserve">before line` + "\n" +
		`needle line` + "\n" +
		`after line</t></is></c>`
	data := buildXlsx(t, singleSheetFixture(cellXML))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}

	reg := registry.New()
	reg.Register(xlsx.Extractor{})
	sink := &fakeSink{}
	orch := app.New(reg, walk.New(), match.NewFactory(), sink)

	_, err := orch.Run(context.Background(), "needle", []string{dir}, domain.SearchOptions{ContextBefore: 1, ContextAfter: 1})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var texts []string
	for _, m := range sink.matches {
		texts = append(texts, m.Text)
	}
	sort.Strings(texts)
	want := []string{"after line", "before line", "needle line"}
	if len(texts) != len(want) {
		t.Fatalf("got %d units %v, want %v", len(texts), texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Errorf("unit texts = %v, want %v", texts, want)
		}
	}
}
