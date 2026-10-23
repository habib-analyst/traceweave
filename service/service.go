// Package service is the instrumented demo application: an HTTP API whose
// handler reads a user record and a product record from a tiny DB layer,
// builds a prompt from the retrieved rows, and calls a (mock) LLM. All
// hops are spans in ONE trace, each carrying its materialized context.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"strings"

	"traceweave/minidb"
	"traceweave/mockllm"
	"traceweave/trace"
)

// Prompt templates. TemplateV2 is a faulty deploy: it fills the PRICE slot
// with the stock value and vice versa. It exists so the workload can
// contain prompt-side faults that are visible in the prompt span payload.
const TemplateV1 = `You are a support assistant. Answer using only the facts below.
USER: {{user_name}}
TIER: {{tier}}
PRODUCT: {{product_name}}
PRICE: {{price}}
STATUS: {{status}}
STOCK: {{stock}}
QUESTION: What is the price and availability of {{product_name}}?`

const TemplateV2 = `You are a support assistant. Answer using only the facts below.
USER: {{user_name}}
TIER: {{tier}}
PRODUCT: {{product_name}}
PRICE: {{stock}}
STATUS: {{status}}
STOCK: {{price}}
QUESTION: What is the price and availability of {{product_name}}?`

// TemplateByID resolves a template id to its text.
func TemplateByID(id string) (string, bool) {
	switch id {
	case "v1":
		return TemplateV1, true
	case "v2":
		return TemplateV2, true
	}
	return "", false
}

// Render fills a template's slots from the retrieved rows. Deterministic:
// the same template and rows always produce the same prompt bytes. Shared
// by the live service and the replay engine.
func Render(tmpl string, user, product map[string]string) string {
	r := strings.NewReplacer(
		"{{user_name}}", user["name"],
		"{{tier}}", user["tier"],
		"{{product_name}}", product["name"],
		"{{price}}", product["price"],
		"{{status}}", product["status"],
		"{{stock}}", product["stock"],
	)
	return r.Replace(tmpl)
}

// Fault describes a defect injected into one request by the workload
// harness. It simulates the world being wrong (a stale DB read, a bad
// template deploy, a bad seed reaching the model) — the tracer records
// whatever actually happened, fault included.
type Fault struct {
	PoisonDB   bool // the product read returns stale/wrong values
	TemplateV2 bool // the faulty template is deployed for this request
	QuirkSeed  bool // a seed that triggers the mock model's misread quirk
}

// Service is the demo application.
type Service struct {
	DB     *minidb.DB
	Tracer *trace.Tracer
	Model  string
	// FaultHook, if set, is consulted once per request with the request
	// counter (0-based, in arrival order). Test/workload code only.
	FaultHook func(reqIndex int) Fault

	reqCount int
}

// Handler returns the HTTP handler for the demo API.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/answer", s.handleAnswer)
	return mux
}

// DefaultSeed derives a deterministic, non-quirk seed for a request.
func DefaultSeed(userID, productID string) int64 {
	h := fnv.New64a()
	h.Write([]byte(userID + "|" + productID))
	seed := int64(h.Sum64() % 100000)
	if mockllm.QuirkSeed(seed) {
		seed++
	}
	return seed
}

// quirkSeed returns a deterministic seed that triggers the model quirk.
func quirkSeed() int64 {
	for seed := int64(0); ; seed++ {
		if mockllm.QuirkSeed(seed) {
			return seed
		}
	}
}

func (s *Service) handleAnswer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if tc, ok := trace.Extract(r.Header); ok {
		ctx = trace.WithContext(ctx, tc)
	}
	idx := s.reqCount
	s.reqCount++
	var fault Fault
	if s.FaultHook != nil {
		fault = s.FaultHook(idx)
	}

	userID := r.URL.Query().Get("user")
	productID := r.URL.Query().Get("product")

	ctx, root := s.Tracer.Start(ctx, "http.handler", trace.KindHTTP)

	answer, err := s.answer(ctx, userID, productID, fault)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		_ = s.Tracer.Finish(root, &trace.HTTPPayload{
			Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
			StatusCode: http.StatusBadRequest, ResponseBody: err.Error(),
		})
		return
	}
	body, _ := json.Marshal(map[string]string{
		"answer":   answer,
		"trace_id": root.TraceID,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	_ = s.Tracer.Finish(root, &trace.HTTPPayload{
		Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
		StatusCode: http.StatusOK, ResponseBody: string(body),
	})
}

// answer runs the instrumented pipeline: DB reads -> prompt build -> LLM.
func (s *Service) answer(ctx context.Context, userID, productID string, fault Fault) (string, error) {
	// Hop 1: read the user record.
	ctxU, spU := s.Tracer.Start(ctx, "db.get_user", trace.KindDB)
	user, err := s.DB.GetByID("users", userID)
	if err != nil {
		return "", err
	}
	_ = s.Tracer.Finish(spU, &trace.DBPayload{
		Table: "users", Operation: "get_by_id",
		Params:   map[string]string{"id": userID},
		Columns:  s.DB.Columns("users"),
		Rows:     []map[string]string{user},
		RowsHash: minidb.RowsHash(s.DB.Columns("users"), []map[string]string{user}),
	})
	_ = ctxU

	// Hop 2: read the product record. A PoisonDB fault simulates a stale
	// replica: the read succeeds but returns wrong values, and the span
	// records the wrong rows — that is what the system actually saw.
	ctxP, spP := s.Tracer.Start(ctx, "db.get_product", trace.KindDB)
	product, err := s.DB.GetByID("products", productID)
	if err != nil {
		return "", err
	}
	if fault.PoisonDB {
		product["price"] = product["stale_price"]
		product["status"] = "discontinued"
	}
	_ = s.Tracer.Finish(spP, &trace.DBPayload{
		Table: "products", Operation: "get_by_id",
		Params:   map[string]string{"id": productID},
		Columns:  s.DB.Columns("products"),
		Rows:     []map[string]string{product},
		RowsHash: minidb.RowsHash(s.DB.Columns("products"), []map[string]string{product}),
	})
	_ = ctxP

	// Hop 3: build the prompt from the retrieved rows.
	tmplID := "v1"
	if fault.TemplateV2 {
		tmplID = "v2"
	}
	tmpl, _ := TemplateByID(tmplID)
	rendered := Render(tmpl, user, product)
	ctxB, spB := s.Tracer.Start(ctx, "prompt.build", trace.KindPrompt)
	_ = s.Tracer.Finish(spB, &trace.PromptPayload{
		TemplateID: tmplID, Template: tmpl, Rendered: rendered,
	})
	_ = ctxB

	// Hop 4: call the model with the exact prompt, params and seed.
	seed := DefaultSeed(userID, productID)
	if fault.QuirkSeed {
		seed = quirkSeed()
	}
	params := mockllm.Params{Model: s.Model, Temperature: 0, Seed: seed}
	output := mockllm.Generate(rendered, params)
	sum := sha256.Sum256([]byte(rendered))
	_, spL := s.Tracer.Start(ctx, "llm.generate", trace.KindLLM)
	_ = s.Tracer.Finish(spL, &trace.LLMPayload{
		Model: s.Model, Prompt: rendered, PromptHash: hex.EncodeToString(sum[:]),
		Temperature: params.Temperature, Seed: seed, Output: output,
	})
	return output, nil
}
