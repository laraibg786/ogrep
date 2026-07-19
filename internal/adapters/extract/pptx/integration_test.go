package pptx

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

// fakeSink collects matches in memory, mirroring the one in
// internal/core/app's own tests, so this test doesn't depend on the
// terminal/json output adapters.
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

// TestIntegrationSearchOrchestrator builds a real pptx fixture on disk
// and runs it through the actual SearchOrchestrator (walker + registry
// + matcher + this extractor), asserting that matches come back with
// correctly rendered Location.Human strings for both slide shapes and
// speaker notes.
func TestIntegrationSearchOrchestrator(t *testing.T) {
	parts := baseParts()
	// Slide 3 is placed in a part named slide1.xml on purpose, and
	// slide 1 in a part named slide3.xml, so a passing test can only be
	// explained by correct sldIdLst-based ordering rather than filename
	// order.
	parts["ppt/presentation.xml"] = presentationXML([]string{"rId1", "rId2", "rId3"})
	parts["ppt/_rels/presentation.xml.rels"] = presentationRelsXML(map[string]string{
		"rId1": "slides/slide3.xml",
		"rId2": "slides/slide1.xml",
		"rId3": "slides/slide2.xml",
	})
	parts["ppt/slides/slide3.xml"] = slideXML([]shapeFixture{
		{name: "Title 1", paragraphs: []string{"nothing interesting here"}},
	})
	parts["ppt/slides/slide1.xml"] = slideXML([]shapeFixture{
		{name: "Title 1", paragraphs: []string{"needle in slide two"}},
	})
	parts["ppt/slides/slide2.xml"] = slideXML([]shapeFixture{
		{name: "Title 1", paragraphs: []string{"unrelated text"}},
	})
	// slide1.xml is the part resolved to presentation position 2 (see
	// the sldIdLst/rels mapping above); its notes association uses a
	// deliberately mismatched numeric suffix (notesSlide7.xml) to prove
	// notes are resolved via the slide's own .rels file rather than by
	// assuming matching numeric filenames.
	parts["ppt/slides/_rels/slide1.xml.rels"] = slideRelsXML("../notesSlides/notesSlide7.xml")
	parts["ppt/notesSlides/notesSlide7.xml"] = notesXML([]shapeFixture{
		{name: "Notes Placeholder 2", paragraphs: []string{"needle in the speaker notes"}},
	})
	data := buildPptx(t, parts)

	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "deck.pptx")
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
		if m.Format != "pptx" {
			t.Errorf("match Format = %q, want %q", m.Format, "pptx")
		}
		if m.Path != fixturePath {
			t.Errorf("match Path = %q, want %q", m.Path, fixturePath)
		}
		humans = append(humans, m.Location.Human())
	}
	sort.Strings(humans)

	want := []string{
		`Slide 2 (Shape "Title 1")`,
		`Slide 2 (Notes)`,
	}
	sort.Strings(want)

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
	parts := baseParts()
	parts["ppt/presentation.xml"] = presentationXML([]string{"rId1"})
	parts["ppt/_rels/presentation.xml.rels"] = presentationRelsXML(map[string]string{
		"rId1": "slides/slide1.xml",
	})
	parts["ppt/slides/slide1.xml"] = slideXML([]shapeFixture{
		{name: "Title 1", paragraphs: []string{"cat", "cot", "cut", "dog"}},
	})
	data := buildPptx(t, parts)

	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "deck.pptx")
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
