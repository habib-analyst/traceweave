// Package store is traceweave's span store: an append-only JSONL file.
// One line per span; traces are reassembled by trace id at read time.
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"traceweave/trace"
)

// Store appends spans to a JSONL file and loads traces back from it.
type Store struct {
	path string
	mu   sync.Mutex
}

// New opens (creating if needed) the span store at path.
func New(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return &Store{path: path}, nil
}

// Record appends one span as a JSON line. Safe for concurrent use.
func (s *Store) Record(sp *trace.Span) error {
	raw, err := json.Marshal(sp)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}

// LoadAll reads every span in the file.
func (s *Store) LoadAll() ([]*trace.Span, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []*trace.Span
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var sp trace.Span
		if err := json.Unmarshal(line, &sp); err != nil {
			return nil, fmt.Errorf("store: corrupt line: %w", err)
		}
		out = append(out, &sp)
	}
	return out, sc.Err()
}

// LoadTrace returns the trace with the given id, spans sorted by start time.
func (s *Store) LoadTrace(traceID string) (*trace.Trace, error) {
	all, err := s.LoadAll()
	if err != nil {
		return nil, err
	}
	tr := &trace.Trace{TraceID: traceID}
	for _, sp := range all {
		if sp.TraceID == traceID {
			tr.Spans = append(tr.Spans, sp)
		}
	}
	if len(tr.Spans) == 0 {
		return nil, fmt.Errorf("store: trace %s not found", traceID)
	}
	sort.Slice(tr.Spans, func(i, j int) bool { return tr.Spans[i].StartNano < tr.Spans[j].StartNano })
	return tr, nil
}

// TraceIDs lists the distinct trace ids present, in first-seen order.
func (s *Store) TraceIDs() ([]string, error) {
	all, err := s.LoadAll()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ids []string
	for _, sp := range all {
		if !seen[sp.TraceID] {
			seen[sp.TraceID] = true
			ids = append(ids, sp.TraceID)
		}
	}
	return ids, nil
}
