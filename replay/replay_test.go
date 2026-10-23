package replay_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"traceweave/experiment"
	"traceweave/minidb"
	"traceweave/replay"
	"traceweave/service"
	"traceweave/store"
	"traceweave/trace"
)

// runOne fires a single request at an instrumented service with the given
// fault and returns its loaded trace, the clean product row, and the
// client trace context that was propagated into the service.
func runOne(t *testing.T, fault service.Fault) (*trace.Trace, map[string]string, trace.Context) {
	t.Helper()
	dir := t.TempDir()
	products, users := experiment.Products(), experiment.Users()
	db, err := minidb.Create(filepath.Join(dir, "db"), []*minidb.Table{products, users})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(dir, "traces.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	svc := &service.Service{
		DB: db, Tracer: trace.NewTracer(st), Model: "mock-1",
		FaultHook: func(int) service.Fault { return fault },
	}
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	tc := trace.Context{TraceID: trace.NewTraceID(), SpanID: trace.NewSpanID()}
	req, _ := http.NewRequest("GET", srv.URL+"/answer?user=u1&product=p1", nil)
	tc.Inject(req.Header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	tr, err := st.LoadTrace(body["trace_id"])
	if err != nil {
		t.Fatal(err)
	}
	return tr, products.Rows[0], tc
}

func TestCaptureCompleteness(t *testing.T) {
	tr, _, clientCtx := runOne(t, service.Fault{})
	// The trace is one tree under http.handler, whose parent is the
	// client span propagated via the traceparent header.
	root := tr.ByName("http.handler")
	if root == nil {
		t.Fatal("no http.handler span")
	}
	if root.TraceID != clientCtx.TraceID || root.ParentID != clientCtx.SpanID {
		t.Fatalf("propagation broken: root trace=%s parent=%s, client %s/%s",
			root.TraceID, root.ParentID, clientCtx.TraceID, clientCtx.SpanID)
	}
	for _, want := range []string{"db.get_user", "db.get_product", "prompt.build", "llm.generate"} {
		s := tr.ByName(want)
		if s == nil {
			t.Fatalf("missing span %s", want)
		}
		if s.TraceID != root.TraceID {
			t.Fatalf("span %s in a different trace", want)
		}
	}
	if got := len(tr.Spans); got != 5 {
		t.Fatalf("want 5 spans, got %d", got)
	}
	var lp trace.LLMPayload
	llmSpan := tr.ByName("llm.generate")
	if err := json.Unmarshal(llmSpan.Payload, &lp); err != nil {
		t.Fatal(err)
	}
	// The recorded prompt hash matches the prompt bytes in the payload —
	// and the prompt span's rendered prompt is byte-identical to what the
	// model span says it received.
	sum := sha256.Sum256([]byte(lp.Prompt))
	if hex.EncodeToString(sum[:]) != lp.PromptHash {
		t.Fatal("prompt hash does not match prompt bytes")
	}
	var pp trace.PromptPayload
	if err := json.Unmarshal(tr.ByName("prompt.build").Payload, &pp); err != nil {
		t.Fatal(err)
	}
	if pp.Rendered != lp.Prompt {
		t.Fatal("prompt span rendered != llm span prompt")
	}
	// Re-running the mock on the recorded prompt+seed reproduces the
	// recorded output — capture was complete.
	res, err := replay.Replay(tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Identical {
		t.Fatalf("replay not identical:\nrecorded %q\nreplayed %q", res.RecordedOutput, res.Output)
	}
	if res.Prompt != lp.Prompt {
		t.Fatal("replayed prompt differs from recorded prompt")
	}
}

func TestSurgeryOnPoisonedRow(t *testing.T) {
	tr, clean, _ := runOne(t, service.Fault{PoisonDB: true})
	judge := func(out string) bool {
		return strings.Contains(out, "$"+clean["price"]) && strings.Contains(out, "currently "+clean["status"])
	}
	base, err := replay.Replay(tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !base.Identical || judge(base.Output) {
		t.Fatalf("poisoned trace should replay identically and be wrong: identical=%v out=%q", base.Identical, base.Output)
	}
	attr, fix, err := replay.Attribute(tr, &replay.Truth{
		UserRow:    map[string]string{"id": "u1", "name": "Amara Chen", "tier": "gold"},
		ProductRow: clean,
		Template:   service.TemplateV1,
	}, judge)
	if err != nil {
		t.Fatal(err)
	}
	if attr != "db.get_product" {
		t.Fatalf("attributed %q, want db.get_product", attr)
	}
	fixed, err := replay.Replay(tr, fix)
	if err != nil {
		t.Fatal(err)
	}
	if !judge(fixed.Output) {
		t.Fatalf("surgery at attributed span did not fix the answer: %q", fixed.Output)
	}
}

func TestAttributeTemplateAndModelFaults(t *testing.T) {
	_, clean, _ := runOne(t, service.Fault{})
	truth := &replay.Truth{
		UserRow:    map[string]string{"id": "u1", "name": "Amara Chen", "tier": "gold"},
		ProductRow: clean,
		Template:   service.TemplateV1,
	}
	judge := func(out string) bool {
		return strings.Contains(out, "$"+clean["price"]) && strings.Contains(out, "currently "+clean["status"])
	}
	for name, tc := range map[string]struct {
		fault service.Fault
		want  string
	}{
		"template": {service.Fault{TemplateV2: true}, "prompt.build"},
		"model":    {service.Fault{QuirkSeed: true}, "llm.generate"},
	} {
		tr, _, _ := runOne(t, tc.fault)
		attr, _, err := replay.Attribute(tr, truth, judge)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if attr != tc.want {
			t.Fatalf("%s fault: attributed %q, want %q", name, attr, tc.want)
		}
	}
}
