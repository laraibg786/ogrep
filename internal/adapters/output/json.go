package output

import (
	"encoding/json"
	"io"
	"sync"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

// JSON is a JSON-lines OutputSink: one JSON object per match, suitable
// for tool consumption via --json. Field names are stable across
// releases.
type JSON struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewJSON builds a JSON sink writing newline-delimited JSON objects to w.
func NewJSON(w io.Writer) *JSON {
	return &JSON{enc: json.NewEncoder(w)}
}

// jsonSpan mirrors domain.Span with explicit field names for the wire
// format (independent of the Go struct's field names, so renaming the
// Go type doesn't silently change the JSON contract).
type jsonSpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// WriteMatch implements ports.OutputSink. The record's fixed fields
// (type/path/format/location/text/spans) are always present; the
// remaining keys are format-specific and come entirely from
// m.Location.Fields(), so this sink never needs to know what those keys
// are for any given format.
func (j *JSON) WriteMatch(m domain.Match) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	spans := make([]jsonSpan, len(m.Spans))
	for i, s := range m.Spans {
		spans[i] = jsonSpan{Start: s.Start, End: s.End}
	}

	rec := map[string]any{
		"type":     "match",
		"path":     m.Path,
		"format":   m.Format,
		"location": m.Location.Human(),
		"text":     m.Text,
		"spans":    spans,
	}
	for k, v := range m.Location.Fields() {
		rec[k] = v
	}
	return j.enc.Encode(rec)
}

// jsonSummary is the on-the-wire shape of a per-file summary record.
type jsonSummary struct {
	Type       string `json:"type"`
	Path       string `json:"path"`
	MatchCount int    `json:"match_count"`
}

// WriteFileSummary implements ports.OutputSink.
func (j *JSON) WriteFileSummary(path string, matchCount int) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.enc.Encode(jsonSummary{Type: "summary", Path: path, MatchCount: matchCount})
}

// Flush implements ports.OutputSink. json.Encoder writes directly
// through to w with no internal buffering, so there is nothing to
// flush; the method exists to satisfy ports.OutputSink.
func (j *JSON) Flush() error { return nil }
