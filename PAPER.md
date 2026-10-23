# traceweave: Replayable Traces for LLM Request Debugging

## Abstract

Distributed tracers record that operations happened; LLM observability
tools record prompts and completions. Neither records enough to
*re-execute* a request: the database rows a prompt was built from are
gone by the time anyone debugs the answer. traceweave is a tracer whose
spans carry their full materialized context — DB rows returned, prompt
bytes sent, model parameters and seed — making each trace a hermetic
description of the request's inputs. A replay engine re-executes the
request from the trace alone and diffs the output byte-for-byte.
Counterfactual edits to one span's recorded context ("context surgery")
turn root-cause analysis into bisection: the span whose surgical fix
turns a wrong answer correct is the span that caused it. On a 120-request
workload with 32 single-span faults, replay was byte-identical for all
120 traces, counterfactual attribution was correct for 32/32 wrong
answers, and surgery at the attributed span fixed 32/32 — versus 4/32
for a log-only baseline that sees only span names and timings.

## 1. Position

Debugging a wrong LLM answer today is archaeology. The engineer has a
trace waterfall (names, durations), maybe the prompt text, and a database
that no longer contains the rows the prompt was built from. Every
hypothesis ("stale data? bad template? model quirk?") requires
reconstructing state that was never captured.

traceweave's claim is that this is a *capture* problem, not an analysis
problem. If the tracer stores, per span, the materialized context the
operation actually consumed and produced, then:

1. the request can be **re-executed hermetically** — no live database,
   no live services, only the trace;
2. re-execution can be **diffed** against the recording, proving the
   trace is a faithful description of the failure;
3. individual pieces of context can be **swapped for trusted versions**
   and the request re-executed — a counterfactual experiment per span,
   which is a decision procedure for "which span's context caused this
   answer", not a heuristic.

Related work is surveyed in `KILLGATE.md`. The nearest neighbours —
Lookspan (replay of captured LLM calls for regression catching) and
Agentfork (live re-execution of forked agent chains) — do not capture DB
context, do not re-execute requests hermetically, and do not attribute.

## 2. Design

### 2.1 Span model

A trace is a tree of spans propagated with W3C-style `traceparent`
headers across HTTP and in-process contexts. Span payloads are typed:

- **http**: method, path, query, status, response body.
- **db**: table, operation, params, columns, **the exact rows returned**,
  and a sha256 rows hash over their canonical encoding.
- **prompt**: template id, template text, and the rendered prompt bytes.
- **llm**: model, **the full prompt exactly as sent** (+ its sha256),
  temperature, **seed**, and the recorded output.

The store is an append-only JSONL file; traces are reassembled by id.

### 2.2 The instrumented pipeline

The demo service answers `GET /answer?user=..&product=..`:

1. `db.get_user` — read the user row.
2. `db.get_product` — read the product row.
3. `prompt.build` — render a template from the two rows.
4. `llm.generate` — a deterministic mock model composes the answer from
   the prompt's structured fields, with phrasing chosen by a hash of
   (prompt, seed, params).

### 2.3 Replay

Replay loads the trace, takes the DB rows from the db span payloads (the
live database is never consulted), takes the template from the prompt
span payload, re-renders the prompt, and re-runs the model with the
recorded model/temperature/seed. The replayed output is compared
byte-for-byte with the recorded output.

### 2.4 Context surgery and attribution

An *override* replaces one span's recorded context: rows for a db span,
the template text, or the seed. Attribution of a wrong answer proceeds
in pipeline order over candidate spans — `db.get_product`, `db.get_user`,
`prompt.build` (reference template), `llm.generate` (smallest non-quirk
seed ≥ recorded) — applying trusted context (ground-truth rows / the
reference template) one span at a time. The first single-span surgery
that makes the answer correct is the attributed cause, and the surgery
itself is returned so the fix can be verified independently.

The procedure assumes a single faulty span per request (true by
construction in the workload; stated as a limit below).

### 2.5 The log-only baseline

The baseline models an engineer with a conventional tracer: span names,
kinds, and durations only — no payloads. Its procedure attributes the
wrong answer to the non-root span with the largest duration (the
"stare at the slowest span" heuristic), ties broken toward the LLM span.
It is deterministic and uses only information a normal trace waterfall
shows.

## 3. Method

Workload (deterministic, RNG seed 20261023): 120 requests over 8 users ×
12 products. Each request independently had a 20% chance of exactly one
injected fault (realized: 32 faulted requests):

- **DB poison** (fault span `db.get_product`): the product read returns
  the row's `stale_price` as its price and `discontinued` as its status —
  a stale-replica read. The db span records the poisoned rows, because
  that is what the system actually saw.
- **Template fault** (fault span `prompt.build`): a faulty template
  version that fills the PRICE slot with the stock value and vice versa.
- **Model fault** (fault span `llm.generate`): the request runs with a
  seed that triggers the mock model's documented misread quirk (it
  answers with the STOCK value as the price; quirk seeds are those with
  seed % 7 == 3). The seed actually used is recorded in the span.

An answer is *correct* iff it states the product's true price and true
status. All products have distinct price/stock values and a stale price
different from the true price, so every injected fault produces a wrong
answer and no unfaulted request can answer wrong — both directions are
verified by the measurements, not assumed.

Metrics: replay determinism over all 120 traces; attribution accuracy
and surgery fix rate over the wrong answers; baseline attribution
accuracy over the same wrong answers. The harness's ground truth (fault
schedule, clean rows) is never visible to the attribution procedure
except as the trusted context an engineer would supply from a known-good
source.

## 4. Results

| Metric | Value |
|---|---|
| Requests | 120 |
| Faulted requests | 32 |
| Wrong answers | 32 (0 among the 88 unfaulted requests) |
| Replay byte-identical | 120/120 → determinism rate **1.000** |
| Attribution correct (traceweave) | 32/32 → **1.000** |
| Surgery fixes (correct after surgery at attributed span) | 32/32 → **1.000** |
| Attribution correct (log-only baseline) | 4/32 → **0.125** |

Two observations beyond the headline:

- **Capture completeness is load-bearing.** Replay determinism at 1.000
  is a *test of the tracer*: it can only hold if the db rows, template,
  prompt, parameters, and seed in the payloads are exactly what the live
  pipeline used. The test suite additionally asserts the llm span's
  prompt hash equals the hash of its prompt bytes and that the prompt
  span's rendered prompt is byte-identical to the prompt the model span
  recorded.
- **Timings carry no content signal.** The baseline's 0.125 is near
  chance for four candidate spans, and its guesses concentrate on
  whichever DB read happens to be slowest. A content fault (wrong value,
  right speed) is invisible in a waterfall. This is the quantitative form
  of the paper's position: without payloads, attribution is guessing.

A worked example from the measured run (trace
`c9f14bf315f48cfc910d9a4930fc14f9`): the answer stated a Juniper Speaker
costs $28; the recorded prompt shows `PRICE: 28 / STOCK: 199` — the
template fault. Attribution returned `prompt.build`; the baseline
guessed `db.get_user`; surgery with the reference template produced the
correct $199 answer.

## 5. Limits

- **Determinism boundary.** The mock model is a pure function, so replay
  is byte-exact. traceweave captures everything needed for determinism
  (prompt bytes, params, seed), but for real LLMs byte-exact replay also
  requires the model to honor its seed — hosted models frequently do
  not. Hermetic *context reconstruction* is guaranteed; *output*
  determinism is exactly as strong as the model's seed discipline.
- **Single-fault assumption.** Attribution tries single-span surgeries.
  Requests with two simultaneous faulty spans would be attributed to
  neither (reported `unknown`); the workload does not contain any.
- **Trusted context requirement.** Surgery needs ground truth from
  somewhere: clean rows, a reference template. In the workload the
  harness supplies it; in production it comes from a source of truth the
  engineer trusts (the primary DB, the template registry). Attribution
  quality is bounded by that trust.
- **Linear demo pipeline.** Two reads, one prompt, one call. Branching,
  parallel, and multi-call pipelines need a replay scheduler; the span
  model does not preclude it, but it is not built here.
- **Payload sensitivity.** Full prompts and rows in traces are sensitive
  data. Redaction/encryption policy is a production requirement this
  build deliberately leaves out of scope.
- **Single node.** One process, one append-only file store.

## 6. Reproduce

```bash
go test ./...            # 10 tests, all passing
go build -o traceweave ./cmd/traceweave
./traceweave experiment --n 120 --seed 20261023 --dir run --out results.json
```

`results.json` in this repository is the output of exactly that command.
