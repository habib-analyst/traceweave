// Package replay is traceweave's core: deterministic re-execution of a
// recorded request from its trace alone. The prompt is rebuilt from the DB
// span payloads (never the live database), the template comes from the
// prompt span payload, and the model is re-run with the recorded seed.
// Overrides implement CONTEXT SURGERY: replace one span's recorded context
// and observe how the answer changes. Counterfactual surgery per candidate
// span is also the attribution procedure: the span whose surgical fix
// turns a wrong answer correct is the span that caused it.
package replay

import (
	"encoding/json"
	"fmt"

	"traceweave/mockllm"
	"traceweave/service"
	"traceweave/trace"
)

// Overrides describes a context surgery: replacement context for spans.
// A nil Overrides means a faithful replay.
type Overrides struct {
	// Rows replaces the rows of a DB span, keyed by span name.
	Rows map[string][]map[string]string
	// Template, if non-empty, replaces the prompt span's template text.
	Template string
	// Seed, if non-nil, replaces the LLM span's seed.
	Seed *int64
}

// Result is the outcome of a replay.
type Result struct {
	Prompt         string // the prompt the replay produced
	Output         string // the replayed answer
	RecordedOutput string // the answer recorded in the trace
	Identical      bool   // Output == RecordedOutput, byte for byte
}

// decode helpers

func dbPayload(s *trace.Span) (*trace.DBPayload, error) {
	var p trace.DBPayload
	if err := json.Unmarshal(s.Payload, &p); err != nil {
		return nil, fmt.Errorf("replay: span %s payload: %w", s.Name, err)
	}
	return &p, nil
}

func promptPayload(s *trace.Span) (*trace.PromptPayload, error) {
	var p trace.PromptPayload
	if err := json.Unmarshal(s.Payload, &p); err != nil {
		return nil, fmt.Errorf("replay: span %s payload: %w", s.Name, err)
	}
	return &p, nil
}

func llmPayload(s *trace.Span) (*trace.LLMPayload, error) {
	var p trace.LLMPayload
	if err := json.Unmarshal(s.Payload, &p); err != nil {
		return nil, fmt.Errorf("replay: span %s payload: %w", s.Name, err)
	}
	return &p, nil
}

// Replay re-executes the request recorded in tr. The DB rows and template
// are taken from span payloads (or from the surgery overrides), the prompt
// is re-rendered, and the model is re-run with the recorded parameters.
func Replay(tr *trace.Trace, ov *Overrides) (*Result, error) {
	userSpan := tr.ByName("db.get_user")
	prodSpan := tr.ByName("db.get_product")
	promptSpan := tr.ByName("prompt.build")
	llmSpan := tr.ByName("llm.generate")
	if userSpan == nil || prodSpan == nil || promptSpan == nil || llmSpan == nil {
		return nil, fmt.Errorf("replay: trace %s is missing pipeline spans", tr.TraceID)
	}
	rowsOf := func(s *trace.Span) ([]map[string]string, error) {
		if ov != nil {
			if rows, ok := ov.Rows[s.Name]; ok {
				return rows, nil
			}
		}
		p, err := dbPayload(s)
		if err != nil {
			return nil, err
		}
		return p.Rows, nil
	}
	userRows, err := rowsOf(userSpan)
	if err != nil {
		return nil, err
	}
	prodRows, err := rowsOf(prodSpan)
	if err != nil {
		return nil, err
	}
	if len(userRows) == 0 || len(prodRows) == 0 {
		return nil, fmt.Errorf("replay: empty rows in trace %s", tr.TraceID)
	}
	pp, err := promptPayload(promptSpan)
	if err != nil {
		return nil, err
	}
	tmpl := pp.Template
	if ov != nil && ov.Template != "" {
		tmpl = ov.Template
	}
	rendered := service.Render(tmpl, userRows[0], prodRows[0])

	lp, err := llmPayload(llmSpan)
	if err != nil {
		return nil, err
	}
	seed := lp.Seed
	if ov != nil && ov.Seed != nil {
		seed = *ov.Seed
	}
	out := mockllm.Generate(rendered, mockllm.Params{
		Model: lp.Model, Temperature: lp.Temperature, Seed: seed,
	})
	return &Result{
		Prompt:         rendered,
		Output:         out,
		RecordedOutput: lp.Output,
		Identical:      out == lp.Output,
	}, nil
}

// Truth is the ground-truth context for a request, known to whoever
// generated the workload (clean rows, the reference template). The
// attribution procedure uses it the way an engineer uses a known-good
// reference: swap recorded context for trusted context, one span at a
// time, and see which swap fixes the answer.
type Truth struct {
	UserRow    map[string]string
	ProductRow map[string]string
	Template   string // reference (correct) template text
}

// Judge decides whether an answer is correct.
type Judge func(output string) bool

// nextGoodSeed returns the smallest seed >= from that does not trigger the
// mock model's misread quirk — the surgical fix for a model-seed fault.
func nextGoodSeed(from int64) int64 {
	for mockllm.QuirkSeed(from) {
		from++
	}
	return from
}

// Attribute runs the counterfactual attribution procedure on a trace whose
// recorded answer is wrong: replay faithfully (must reproduce the wrong
// answer), then apply surgery at one candidate span at a time, in
// pipeline order. The first span whose fix makes the answer correct is
// the attributed cause. Returns the span name, the fixing overrides (so
// the caller can verify the surgery), or ("unknown", nil, nil) if no
// single-span fix works.
func Attribute(tr *trace.Trace, truth *Truth, judge Judge) (string, *Overrides, error) {
	base, err := Replay(tr, nil)
	if err != nil {
		return "", nil, err
	}
	if !base.Identical {
		return "", nil, fmt.Errorf("attribute: trace %s is not replayable (replay output != recorded output)", tr.TraceID)
	}
	if judge(base.Output) {
		return "", nil, fmt.Errorf("attribute: trace %s answer is not wrong; nothing to attribute", tr.TraceID)
	}
	// Candidate 1+2: each DB span's rows replaced by trusted rows.
	for _, spanName := range []string{"db.get_product", "db.get_user"} {
		clean := truth.ProductRow
		if spanName == "db.get_user" {
			clean = truth.UserRow
		}
		ov := &Overrides{Rows: map[string][]map[string]string{spanName: {clean}}}
		res, err := Replay(tr, ov)
		if err != nil {
			return "", nil, err
		}
		if judge(res.Output) {
			return spanName, ov, nil
		}
	}
	// Candidate 3: the prompt template replaced by the reference template.
	ov := &Overrides{Template: truth.Template}
	res, err := Replay(tr, ov)
	if err != nil {
		return "", nil, err
	}
	if judge(res.Output) {
		return "prompt.build", ov, nil
	}
	// Candidate 4: the model call re-run with a non-quirk seed.
	lp, err := llmPayload(tr.ByName("llm.generate"))
	if err != nil {
		return "", nil, err
	}
	good := nextGoodSeed(lp.Seed)
	ov = &Overrides{Seed: &good}
	res, err = Replay(tr, ov)
	if err != nil {
		return "", nil, err
	}
	if judge(res.Output) {
		return "llm.generate", ov, nil
	}
	return "unknown", nil, nil
}

// AttributeLogOnly is the NO-REPLAY baseline: an engineer with only span
// names, kinds and timings — the classic trace waterfall, no payloads.
// The procedure attributes the wrong answer to the non-root span with the
// largest duration (the "stare at the slowest span" heuristic), breaking
// ties toward the LLM span. It cannot see any span's content, which is
// precisely the point of the contrast.
func AttributeLogOnly(tr *trace.Trace) string {
	root := tr.Root()
	best := ""
	var bestDur int64 = -1
	for _, s := range tr.Spans {
		if s == root {
			continue // skip the root
		}
		d := s.EndNano - s.StartNano
		if d > bestDur || (d == bestDur && s.Name == "llm.generate") {
			best, bestDur = s.Name, d
		}
	}
	return best
}
