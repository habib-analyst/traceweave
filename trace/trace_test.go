package trace

import (
	"context"
	"net/http"
	"testing"
)

func TestTraceparentRoundTrip(t *testing.T) {
	c := Context{TraceID: NewTraceID(), SpanID: NewSpanID()}
	h := c.Traceparent()
	got, err := ParseTraceparent(h)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != c {
		t.Fatalf("round trip mismatch: %v != %v", got, c)
	}
}

func TestTraceparentRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "00-xyz", "00-short-short-01", "garbage"} {
		if _, err := ParseTraceparent(bad); err == nil {
			t.Fatalf("accepted bad traceparent %q", bad)
		}
	}
}

func TestHeaderInjectExtract(t *testing.T) {
	c := Context{TraceID: NewTraceID(), SpanID: NewSpanID()}
	hdr := http.Header{}
	c.Inject(hdr)
	got, ok := Extract(hdr)
	if !ok || got != c {
		t.Fatalf("extract: ok=%v got=%v want=%v", ok, got, c)
	}
	if _, ok := Extract(http.Header{}); ok {
		t.Fatal("extract from empty header should fail")
	}
}

type memRecorder struct{ spans []*Span }

func (m *memRecorder) Record(s *Span) error { m.spans = append(m.spans, s); return nil }

func TestTracerParenting(t *testing.T) {
	rec := &memRecorder{}
	tr := NewTracer(rec)
	parent := Context{TraceID: NewTraceID(), SpanID: NewSpanID()}
	ctx := WithContext(context.Background(), parent)
	ctx, root := tr.Start(ctx, "http.handler", KindHTTP)
	if root.TraceID != parent.TraceID || root.ParentID != parent.SpanID {
		t.Fatalf("root parenting wrong: trace=%s parent=%s", root.TraceID, root.ParentID)
	}
	_, child := tr.Start(ctx, "db.get_product", KindDB)
	if child.TraceID != root.TraceID || child.ParentID != root.SpanID {
		t.Fatalf("child parenting wrong: %+v", child)
	}
	if err := tr.Finish(child, &DBPayload{Table: "products"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.spans) != 1 || rec.spans[0].EndNano < rec.spans[0].StartNano {
		t.Fatalf("recorded spans wrong: %+v", rec.spans)
	}
	// A fresh context starts a new trace.
	_, orphan := tr.Start(context.Background(), "http.handler", KindHTTP)
	if orphan.TraceID == root.TraceID || orphan.ParentID != "" {
		t.Fatalf("orphan span wrong: %+v", orphan)
	}
}
