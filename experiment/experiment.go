// Package experiment runs traceweave's measured core: a workload of
// support-answer requests against the instrumented demo service, with a
// known fraction of requests faulted in exactly one span (a poisoned DB
// row, a faulty prompt template, or a model-quirk seed). It then measures
// replay determinism, traceweave's counterfactual attribution accuracy,
// the surgery fix rate, and the log-only baseline's attribution accuracy.
package experiment

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"traceweave/minidb"
	"traceweave/replay"
	"traceweave/service"
	"traceweave/store"
	"traceweave/trace"
)

// Fault span names, used as ground-truth labels.
const (
	FaultDB       = "db.get_product"
	FaultTemplate = "prompt.build"
	FaultModel    = "llm.generate"
)

// Products builds the demo product table. Every product has a stale_price
// different from its price and a status the poisoner can flip, so a
// poisoned read always changes the correct answer.
func Products() *minidb.Table {
	type p struct{ id, name, price, stale, status, stock string }
	items := []p{
		{"p1", "Aurora Headphones", "129", "99", "in stock", "34"},
		{"p2", "Boreal Keyboard", "89", "74", "in stock", "57"},
		{"p3", "Cinder Mouse", "49", "39", "low stock", "8"},
		{"p4", "Drift Monitor", "399", "349", "in stock", "21"},
		{"p5", "Ember Webcam", "79", "64", "in stock", "43"},
		{"p6", "Fable Microphone", "149", "119", "low stock", "6"},
		{"p7", "Glacier SSD 1TB", "109", "89", "in stock", "62"},
		{"p8", "Harbor Dock", "179", "154", "in stock", "17"},
		{"p9", "Ion Charger", "29", "24", "low stock", "9"},
		{"p10", "Juniper Speaker", "199", "169", "in stock", "28"},
		{"p11", "Kite Router", "139", "114", "in stock", "39"},
		{"p12", "Lumen Desk Lamp", "59", "47", "low stock", "7"},
	}
	t := &minidb.Table{Name: "products", Columns: []string{"id", "name", "price", "stale_price", "status", "stock"}}
	for _, it := range items {
		t.Rows = append(t.Rows, map[string]string{
			"id": it.id, "name": it.name, "price": it.price,
			"stale_price": it.stale, "status": it.status, "stock": it.stock,
		})
	}
	return t
}

// Users builds the demo user table.
func Users() *minidb.Table {
	type u struct{ id, name, tier string }
	items := []u{
		{"u1", "Amara Chen", "gold"},
		{"u2", "Bilal Ortiz", "free"},
		{"u3", "Priya Nair", "silver"},
		{"u4", "Jonas Weber", "gold"},
		{"u5", "Sofia Rossi", "free"},
		{"u6", "Ken Watanabe", "silver"},
		{"u7", "Lena Fischer", "gold"},
		{"u8", "Omar Haddad", "free"},
	}
	t := &minidb.Table{Name: "users", Columns: []string{"id", "name", "tier"}}
	for _, it := range items {
		t.Rows = append(t.Rows, map[string]string{"id": it.id, "name": it.name, "tier": it.tier})
	}
	return t
}

// Results is the measured outcome of a workload run.
type Results struct {
	Requests            int     `json:"requests"`
	FaultedRequests     int     `json:"faulted_requests"`
	WrongAnswers        int     `json:"wrong_answers"`
	WrongAmongUnfaulted int     `json:"wrong_among_unfaulted"`
	ReplayIdentical     int     `json:"replay_identical"`
	ReplayDeterminism   float64 `json:"replay_determinism_rate"`
	AttributionCorrect  int     `json:"attribution_correct"`
	AttributionAccuracy float64 `json:"attribution_accuracy_traceweave"`
	SurgeryFixes        int     `json:"surgery_fixes"`
	SurgeryFixRate      float64 `json:"surgery_fix_rate"`
	BaselineCorrect     int     `json:"baseline_correct"`
	BaselineAccuracy    float64 `json:"attribution_accuracy_log_only_baseline"`
}

// requestRecord is the ground truth for one request.
type requestRecord struct {
	TraceID   string
	TrueFault string // "" if none
	UserRow   map[string]string
	ProdRow   map[string]string
}

// Run executes the workload of n requests in dir (created fresh) and
// returns the measured results. Deterministic for a given seed.
func Run(dir string, n int, seed int64) (*Results, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	products, users := Products(), Users()
	db, err := minidb.Create(filepath.Join(dir, "db"), []*minidb.Table{products, users})
	if err != nil {
		return nil, err
	}
	st, err := store.New(filepath.Join(dir, "traces.jsonl"))
	if err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(seed))

	// Fault plan, decided up front by request index.
	faults := make([]string, n)
	faultTypes := []string{FaultDB, FaultTemplate, FaultModel}
	faulted := 0
	for i := range faults {
		if rng.Float64() < 0.20 {
			faults[i] = faultTypes[rng.Intn(len(faultTypes))]
			faulted++
		}
	}
	svc := &service.Service{
		DB: db, Tracer: trace.NewTracer(st), Model: "mock-1",
		FaultHook: func(reqIndex int) service.Fault {
			switch faults[reqIndex] {
			case FaultDB:
				return service.Fault{PoisonDB: true}
			case FaultTemplate:
				return service.Fault{TemplateV2: true}
			case FaultModel:
				return service.Fault{QuirkSeed: true}
			}
			return service.Fault{}
		},
	}
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	prodByID := map[string]map[string]string{}
	for _, r := range products.Rows {
		prodByID[r["id"]] = r
	}
	userByID := map[string]map[string]string{}
	for _, r := range users.Rows {
		userByID[r["id"]] = r
	}
	productIDs := make([]string, len(products.Rows))
	for i, r := range products.Rows {
		productIDs[i] = r["id"]
	}
	userIDs := make([]string, len(users.Rows))
	for i, r := range users.Rows {
		userIDs[i] = r["id"]
	}

	var records []requestRecord
	client := &http.Client{}
	for i := 0; i < n; i++ {
		uid := userIDs[rng.Intn(len(userIDs))]
		pid := productIDs[rng.Intn(len(productIDs))]
		req, err := http.NewRequest("GET", srv.URL+"/answer?user="+uid+"&product="+pid, nil)
		if err != nil {
			return nil, err
		}
		// The client starts a trace and propagates it W3C-style; the
		// service's root span must become a child of this context.
		tc := trace.Context{TraceID: trace.NewTraceID(), SpanID: trace.NewSpanID()}
		tc.Inject(req.Header)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()
		if body["trace_id"] != tc.TraceID {
			return nil, fmt.Errorf("request %d: trace id mismatch: client %s, service %s", i, tc.TraceID, body["trace_id"])
		}
		records = append(records, requestRecord{
			TraceID: body["trace_id"], TrueFault: faults[i],
			UserRow: userByID[uid], ProdRow: prodByID[pid],
		})
	}

	res := &Results{Requests: n, FaultedRequests: faulted}
	for _, rec := range records {
		tr, err := st.LoadTrace(rec.TraceID)
		if err != nil {
			return nil, err
		}
		prod := rec.ProdRow
		judge := func(output string) bool {
			return strings.Contains(output, "$"+prod["price"]) &&
				strings.Contains(output, "currently "+prod["status"])
		}
		rep, err := replay.Replay(tr, nil)
		if err != nil {
			return nil, err
		}
		if rep.Identical {
			res.ReplayIdentical++
		}
		if judge(rep.RecordedOutput) {
			continue // correct answer; nothing to attribute
		}
		res.WrongAnswers++
		if rec.TrueFault == "" {
			res.WrongAmongUnfaulted++
			continue
		}
		truth := &replay.Truth{
			UserRow: rec.UserRow, ProductRow: rec.ProdRow,
			Template: service.TemplateV1,
		}
		attributed, fix, err := replay.Attribute(tr, truth, judge)
		if err != nil {
			return nil, err
		}
		if attributed == rec.TrueFault {
			res.AttributionCorrect++
		}
		if fix != nil {
			fixed, err := replay.Replay(tr, fix)
			if err != nil {
				return nil, err
			}
			if judge(fixed.Output) {
				res.SurgeryFixes++
			}
		}
		if replay.AttributeLogOnly(tr) == rec.TrueFault {
			res.BaselineCorrect++
		}
	}
	if res.Requests > 0 {
		res.ReplayDeterminism = float64(res.ReplayIdentical) / float64(res.Requests)
	}
	if res.WrongAnswers > 0 {
		denom := float64(res.WrongAnswers - res.WrongAmongUnfaulted)
		if denom > 0 {
			res.AttributionAccuracy = float64(res.AttributionCorrect) / denom
			res.SurgeryFixRate = float64(res.SurgeryFixes) / denom
			res.BaselineAccuracy = float64(res.BaselineCorrect) / denom
		}
	}
	return res, nil
}
