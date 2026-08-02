package odt

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/laraibg786/ogrep/internal/adapters/match"
	"github.com/laraibg786/ogrep/internal/adapters/walk"
	"github.com/laraibg786/ogrep/internal/core/app"
	"github.com/laraibg786/ogrep/internal/core/domain"
	"github.com/laraibg786/ogrep/internal/registry"
)

// fakeSink collects matches in memory, mirroring the one used by
// docx/pptx/xlsx's own integration tests, so this test doesn't depend
// on the terminal/json output adapters.
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

// TestIntegrationSearchOrchestrator builds a real odt fixture on disk
// and runs it through the actual SearchOrchestrator (walker + registry
// + matcher + this extractor), asserting matches come back with
// correctly rendered heading-based Location.Human strings for both a
// body paragraph and a table cell.
func TestIntegrationSearchOrchestrator(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p>unrelated intro text</text:p>` +
				`<text:h text:outline-level="1">Introduction</text:h>` +
				`<text:p>needle in the body</text:p>` +
				`<table:table><table:table-row><table:table-cell><text:p>needle in a cell</text:p></table:table-cell></table:table-row></table:table>`),
	})

	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "doc.odt")
	if err := os.WriteFile(fixturePath, data, 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	reg := registry.New()
	reg.Register(Extractor{})

	sink := &fakeSink{}
	orch := app.New(reg, walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "needle", []string{dir}, domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.TotalMatches != 2 {
		t.Fatalf("TotalMatches = %d, want 2", stats.TotalMatches)
	}

	var humans []string
	for _, m := range sink.matches {
		if m.Format != "odt" {
			t.Errorf("match Format = %q, want %q", m.Format, "odt")
		}
		if m.Path != fixturePath {
			t.Errorf("match Path = %q, want %q", m.Path, fixturePath)
		}
		humans = append(humans, m.Location.Human())
	}
	sort.Strings(humans)

	want := []string{"Introduction", "Introduction"}
	if len(humans) != len(want) {
		t.Fatalf("got Human strings %v, want %v", humans, want)
	}
	for i := range want {
		if humans[i] != want[i] {
			t.Errorf("got Human strings %v, want %v", humans, want)
		}
	}
}

// TestIntegrationRegexPattern confirms a regex matcher works correctly
// end-to-end through the same real orchestrator wiring.
func TestIntegrationRegexPattern(t *testing.T) {
	data := buildOdt(t, "application/vnd.oasis.opendocument.text", map[string]string{
		"content.xml": wrapContent(
			`<text:p>cat</text:p><text:p>cot</text:p><text:p>cut</text:p><text:p>dog</text:p>`),
	})

	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "doc.odt")
	if err := os.WriteFile(fixturePath, data, 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	reg := registry.New()
	reg.Register(Extractor{})

	sink := &fakeSink{}
	orch := app.New(reg, walk.New(), match.NewFactory(), sink)

	stats, err := orch.Run(context.Background(), "c[aou]t", []string{dir}, domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.TotalMatches != 3 {
		t.Errorf("TotalMatches = %d, want 3", stats.TotalMatches)
	}
}
