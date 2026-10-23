// Package trace defines the traceweave span model and W3C-style context
// propagation. Unlike a conventional tracer, a traceweave span carries the
// full materialized context of the operation (typed payload), which is what
// makes a trace replayable later.
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Span kinds.
const (
	KindHTTP     = "http"
	KindDB       = "db"
	KindPrompt   = "prompt"
	KindLLM      = "llm"
	KindInternal = "internal"
)

// Span is one node of a trace tree. Payload holds the materialized context
// for the span kind (see the payload structs below); it is what the replay
// engine consumes.
type Span struct {
	TraceID   string          `json:"trace_id"`
	SpanID    string          `json:"span_id"`
	ParentID  string          `json:"parent_id,omitempty"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	StartNano int64           `json:"start_unix_nano"`
	EndNano   int64           `json:"end_unix_nano"`
	Status    string          `json:"status"` // "ok" or "error"
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Duration returns the span wall-clock duration.
func (s *Span) Duration() time.Duration {
	return time.Duration(s.EndNano - s.StartNano)
}

// HTTPPayload is the materialized context of the root HTTP span.
type HTTPPayload struct {
	Method       string `json:"method"`
	Path         string `json:"path"`
	RawQuery     string `json:"raw_query"`
	StatusCode   int    `json:"status_code"`
	ResponseBody string `json:"response_body"`
}

// DBPayload is the materialized context of a database span: the query AND
// the exact rows it returned. The rows are the point — replay never touches
// the live database, it reads them from here.
type DBPayload struct {
	Table     string              `json:"table"`
	Operation string              `json:"operation"` // e.g. "get_by_id"
	Params    map[string]string   `json:"params"`
	Columns   []string            `json:"columns"`
	Rows      []map[string]string `json:"rows"`
	RowsHash  string              `json:"rows_hash"` // sha256 over the canonical row encoding
}

// PromptPayload is the materialized context of a prompt-construction span.
type PromptPayload struct {
	TemplateID string `json:"template_id"`
	Template   string `json:"template"` // the template text with {{slots}}
	Rendered   string `json:"rendered"` // the exact prompt bytes produced
}

// LLMPayload is the materialized context of a model call: the FULL prompt
// exactly as sent, the parameters, and the seed — everything needed to
// re-execute the call deterministically.
type LLMPayload struct {
	Model       string  `json:"model"`
	Prompt      string  `json:"prompt"`
	PromptHash  string  `json:"prompt_hash"`
	Temperature float64 `json:"temperature"`
	Seed        int64   `json:"seed"`
	Output      string  `json:"output"`
}

// Context carries trace identity through in-process calls and HTTP headers.
type Context struct {
	TraceID string
	SpanID  string // current (parent) span id
}

// NewTraceID returns a 32-hex-char trace id.
func NewTraceID() string { return randHex(16) }

// NewSpanID returns a 16-hex-char span id.
func NewSpanID() string { return randHex(8) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

type ctxKey struct{}

// WithContext stores a trace Context in a Go context.
func WithContext(ctx context.Context, tc Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, tc)
}

// FromContext extracts the trace Context, if any.
func FromContext(ctx context.Context) (Context, bool) {
	tc, ok := ctx.Value(ctxKey{}).(Context)
	return tc, ok
}

// Traceparent renders the W3C traceparent header value:
// version-traceid-spanid-flags, e.g. 00-<32hex>-<16hex>-01.
func (c Context) Traceparent() string {
	return fmt.Sprintf("00-%s-%s-01", c.TraceID, c.SpanID)
}

// ParseTraceparent parses a W3C traceparent header value.
func ParseTraceparent(h string) (Context, error) {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) != 4 {
		return Context{}, fmt.Errorf("traceparent: want 4 parts, got %d", len(parts))
	}
	if len(parts[1]) != 32 || len(parts[2]) != 16 {
		return Context{}, fmt.Errorf("traceparent: bad id lengths")
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return Context{}, fmt.Errorf("traceparent: bad trace id: %w", err)
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		return Context{}, fmt.Errorf("traceparent: bad span id: %w", err)
	}
	return Context{TraceID: parts[1], SpanID: parts[2]}, nil
}

// Inject writes the trace context into HTTP headers.
func (c Context) Inject(h http.Header) {
	h.Set("traceparent", c.Traceparent())
}

// Extract reads a trace context from HTTP headers.
func Extract(h http.Header) (Context, bool) {
	v := h.Get("traceparent")
	if v == "" {
		return Context{}, false
	}
	tc, err := ParseTraceparent(v)
	if err != nil {
		return Context{}, false
	}
	return tc, true
}

// Recorder is anything that can persist a finished span.
type Recorder interface {
	Record(s *Span) error
}

// Tracer creates spans and hands them to a Recorder when finished.
type Tracer struct {
	rec Recorder
}

// NewTracer returns a Tracer writing to rec.
func NewTracer(rec Recorder) *Tracer { return &Tracer{rec: rec} }

// Start begins a span. If ctx carries a trace context, the new span is its
// child; otherwise a new trace is started. The returned context carries the
// new span as the parent for nested operations.
func (t *Tracer) Start(ctx context.Context, name, kind string) (context.Context, *Span) {
	s := &Span{Name: name, Kind: kind, Status: "ok", StartNano: time.Now().UnixNano()}
	if parent, ok := FromContext(ctx); ok && parent.TraceID != "" {
		s.TraceID = parent.TraceID
		s.ParentID = parent.SpanID
	} else {
		s.TraceID = NewTraceID()
	}
	s.SpanID = NewSpanID()
	return WithContext(ctx, Context{TraceID: s.TraceID, SpanID: s.SpanID}), s
}

// Finish sets the end time and a typed payload, then records the span.
func (t *Tracer) Finish(s *Span, payload any) error {
	s.EndNano = time.Now().UnixNano()
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		s.Payload = raw
	}
	if t.rec != nil {
		return t.rec.Record(s)
	}
	return nil
}

// Trace is a whole request: all spans sharing a TraceID.
type Trace struct {
	TraceID string  `json:"trace_id"`
	Spans   []*Span `json:"spans"`
}

// Root returns the local root of the trace: the span whose parent is not
// itself in the trace. With W3C propagation the root's parent is the
// caller's span in another process, so "parentless" is not the test.
func (tr *Trace) Root() *Span {
	ids := make(map[string]bool, len(tr.Spans))
	for _, s := range tr.Spans {
		ids[s.SpanID] = true
	}
	for _, s := range tr.Spans {
		if s.ParentID == "" || !ids[s.ParentID] {
			return s
		}
	}
	return nil
}

// ByName returns the first span with the given name.
func (tr *Trace) ByName(name string) *Span {
	for _, s := range tr.Spans {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// Children returns the direct children of a span, in start order.
func (tr *Trace) Children(spanID string) []*Span {
	var out []*Span
	for _, s := range tr.Spans {
		if s.ParentID == spanID {
			out = append(out, s)
		}
	}
	return out
}
