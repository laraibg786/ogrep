package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// testLocation is a minimal domain.Location implementation for tests
// that don't need a specific format's location shape.
type testLocation struct {
	human  string
	fields map[string]any
}

func (l testLocation) Human() string          { return l.human }
func (l testLocation) Fields() map[string]any { return l.fields }

func sampleMatch() domain.Match {
	return domain.Match{
		Path:     "a.txt",
		Format:   "text",
		Location: testLocation{human: "line 3", fields: map[string]any{"line": 3}},
		Text:     "hello world",
		Spans:    []domain.Span{{Start: 0, End: 5}},
	}
}

func TestTerminalNoColorWhenNever(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, ColorNever, nil, SummaryModeOff)
	if err := term.WriteMatch(sampleMatch()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("expected no ANSI codes with ColorNever, got %q", out)
	}
	if !strings.Contains(out, "a.txt:line 3") {
		t.Errorf("expected location prefix in output, got %q", out)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("expected matched text in output, got %q", out)
	}
}

func TestTerminalColorWhenAlways(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, ColorAlways, nil, SummaryModeOff)
	if err := term.WriteMatch(sampleMatch()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("expected ANSI codes with ColorAlways, got %q", out)
	}
	if !strings.Contains(out, ansiMatch+"hello"+ansiReset) {
		t.Errorf("expected highlighted span around %q, got %q", "hello", out)
	}
}

func TestTerminalAutoWithNilTTYIsNoColor(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, ColorAuto, nil, SummaryModeOff)
	if err := term.WriteMatch(sampleMatch()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Error("expected ColorAuto with no tty file to disable color")
	}
}

func TestJSONWriteMatch(t *testing.T) {
	var buf bytes.Buffer
	sink := NewJSON(&buf)
	if err := sink.WriteMatch(sampleMatch()); err != nil {
		t.Fatal(err)
	}

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output was not valid JSON: %v (%q)", err, buf.String())
	}
	if rec["path"] != "a.txt" {
		t.Errorf("path = %v, want a.txt", rec["path"])
	}
	if rec["text"] != "hello world" {
		t.Errorf("text = %v, want %q", rec["text"], "hello world")
	}
	spans, ok := rec["spans"].([]any)
	if !ok || len(spans) != 1 {
		t.Errorf("spans = %v, want one span", rec["spans"])
	}
}

func TestTerminalWriteFileSummaryOff(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, ColorNever, nil, SummaryModeOff)
	if err := term.WriteFileSummary("a.txt", 3); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output for SummaryModeOff, got %q", buf.String())
	}
}

func TestTerminalWriteFileSummaryPathOnly(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, ColorNever, nil, SummaryModePathOnly)
	if err := term.WriteFileSummary("a.txt", 3); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "a.txt\n" {
		t.Errorf("got %q, want %q", got, "a.txt\n")
	}
}

func TestTerminalWriteFileSummaryCount(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, ColorNever, nil, SummaryModeCount)
	if err := term.WriteFileSummary("a.txt", 3); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "a.txt:3\n" {
		t.Errorf("got %q, want %q", got, "a.txt:3\n")
	}
}

func TestJSONWriteFileSummary(t *testing.T) {
	var buf bytes.Buffer
	sink := NewJSON(&buf)
	if err := sink.WriteFileSummary("a.txt", 5); err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output was not valid JSON: %v", err)
	}
	if rec["type"] != "summary" || rec["match_count"].(float64) != 5 {
		t.Errorf("unexpected summary record: %v", rec)
	}
}

func TestJSONIsLineDelimited(t *testing.T) {
	var buf bytes.Buffer
	sink := NewJSON(&buf)
	sink.WriteMatch(sampleMatch())
	sink.WriteMatch(sampleMatch())
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("got %d lines, want 2", len(lines))
	}
}
