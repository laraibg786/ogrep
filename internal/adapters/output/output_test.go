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
	human        string
	fields       map[string]any
	hyperlinkURI string
}

func (l testLocation) Human() string                                        { return l.human }
func (l testLocation) Fields(spans []domain.Span) map[string]any            { return l.fields }
func (l testLocation) HyperlinkURI(path string, spans []domain.Span) string { return l.hyperlinkURI }

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
	if rec["uri"] != "" {
		t.Errorf("uri = %v, want empty string (sampleMatch's location has no hyperlink)", rec["uri"])
	}
	if _, present := rec["location"]; present {
		t.Errorf("location = %v, want no generic \"location\" key", rec["location"])
	}
	if rec["line"] != float64(3) {
		t.Errorf("line = %v, want numeric 3", rec["line"])
	}
	spans, ok := rec["spans"].([]any)
	if !ok || len(spans) != 1 {
		t.Errorf("spans = %v, want one span", rec["spans"])
	}
}

func TestJSONWriteMatchIncludesHyperlinkURI(t *testing.T) {
	var buf bytes.Buffer
	sink := NewJSON(&buf)
	match := domain.Match{
		Path:     "a.txt",
		Format:   "text",
		Location: testLocation{human: "3", fields: map[string]any{"line": 3}, hyperlinkURI: "file:///tmp/a.txt:3:1"},
		Text:     "hello world",
	}
	if err := sink.WriteMatch(match); err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output was not valid JSON: %v (%q)", err, buf.String())
	}
	if rec["uri"] != "file:///tmp/a.txt:3:1" {
		t.Errorf("uri = %v, want %q", rec["uri"], "file:///tmp/a.txt:3:1")
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

func TestTerminalWriteMatchIsSingleLine(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, ColorNever, nil, SummaryModeOff)
	if err := term.WriteMatch(sampleMatch()); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "a.txt:line 3 hello world\n" {
		t.Errorf("got %q, want %q", got, "a.txt:line 3 hello world\n")
	}
}

func TestTerminalWriteMatchWithHyperlink(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, ColorNever, nil, SummaryModeOff)
	match := domain.Match{
		Path:     "a.txt",
		Location: testLocation{human: "line 3", fields: map[string]any{}, hyperlinkURI: "file://a.txt:3:1"},
		Text:     "hello world",
	}
	if err := term.WriteMatch(match); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "\x1b]8;;file://a.txt:3:1\x1b\\a.txt:line 3\x1b]8;;\x1b\\") {
		t.Errorf("expected hyperlink-wrapped location, got %q", out)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("expected matched text in output, got %q", out)
	}
}

func TestTerminalWriteMatchNoHyperlinkWhenURIEmpty(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, ColorNever, nil, SummaryModeOff)
	if err := term.WriteMatch(sampleMatch()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "\x1b]8;;") {
		t.Error("expected no hyperlink codes when HyperlinkURI returns \"\"")
	}
}

func TestTerminalWriteMatchEscapesControlCharsInLocation(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, ColorNever, nil, SummaryModeOff)
	match := domain.Match{
		Path:     "a\nb\tc.txt",
		Location: testLocation{human: "line 3", fields: map[string]any{}},
		Text:     "hello world",
	}
	if err := term.WriteMatch(match); err != nil {
		t.Fatal(err)
	}
	want := `a\nb\tc.txt:line 3 hello world` + "\n"
	if got := buf.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTerminalWriteMatchEscapesEscAndBelInLocation(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, ColorNever, nil, SummaryModeOff)
	match := domain.Match{
		Path:     "a\x1bb\x07c.txt",
		Location: testLocation{human: "line 3", fields: map[string]any{}},
		Text:     "hello world",
	}
	if err := term.WriteMatch(match); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("expected no raw ESC/BEL bytes in output, got %q", out)
	}
	want := `a\x1bb\x07c.txt:line 3 hello world` + "\n"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestTerminalWriteMatchPadsContinuationLines(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf, ColorNever, nil, SummaryModeOff)
	match := domain.Match{
		Path:     "a.txt",
		Location: testLocation{human: "line 3", fields: map[string]any{}},
		Text:     "first line\nsecond line",
	}
	if err := term.WriteMatch(match); err != nil {
		t.Fatal(err)
	}
	loc := "a.txt:line 3"
	want := loc + " first line\n" + strings.Repeat(" ", len(loc)+1) + "second line\n"
	if got := buf.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
