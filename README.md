# traceweave

**A tracer whose traces are replayable.** Every span carries the full
materialized context of the operation — the exact DB rows a query returned,
the exact prompt bytes sent to the model, the model parameters and seed —
so any recorded request can be deterministically **re-executed from the
trace alone** and its output diffed against the original. When an answer
is wrong, counterfactual **context surgery** (edit one span's recorded
context, replay, observe) bisects the trace and names the exact span whose
context made the wrong answer inevitable.

```
http.handler [http] 719.881µs
  db.get_user [db] 21.602µs
  db.get_product [db] 13.37µs
  prompt.build [prompt] 1.402µs
  llm.generate [llm] 1.443µs
```

## Why this exists

Tracers (OpenTelemetry) record that a span happened. LLM observability
tools (Langfuse, LangSmith, Helicone) log prompts and completions. Neither
lets you *re-run the request*: the database has moved on, the prompt was
rebuilt from rows nobody kept, the seed is gone. Debugging a hallucination
becomes archaeology.

traceweave's position: **a trace should be a complete, hermetic
description of a request's inputs.** If every span carries its
materialized context, the trace is not a log of the computation — it is
enough to *redo* the computation, byte for byte, and to redo it with one
piece of context changed. That turns "the model hallucinated" into a
bisectable question with a measurable answer.

Nearest prior art, honestly: Lookspan replays captured LLM calls to catch
prompt/model regressions, and Agentfork forks agent chains and re-executes
them live. Neither captures DB context into spans, neither re-executes a
request hermetically, and neither attributes a wrong answer to a span.
See `KILLGATE.md` for the full analysis.

## What is in the box

| Piece | What it does |
|---|---|
| `trace/` | Span model with typed payloads (http / db / prompt / llm), W3C `traceparent` propagation over HTTP headers and in-process contexts |
| `store/` | Append-only JSONL span store; reassembles traces by id |
| `minidb/` | Tiny file-backed table store — the demo's database hop |
| `mockllm/` | Deterministic mock model: a pure function of (prompt, seed, params), with one documented misread quirk standing in for a model failure mode |
| `service/` | Instrumented demo API: `GET /answer?user=u1&product=p1` — DB reads → prompt build → model call, all spans in ONE trace |
| `replay/` | The replay engine, context surgery, the counterfactual attribution procedure, and the log-only baseline |
| `experiment/` | The measured workload: fault injection + metrics |
| `cmd/traceweave/` | CLI: `serve`, `tree`, `span`, `replay`, `surgery`, `attribute`, `experiment` |

## Quickstart

```bash
go build -o traceweave ./cmd/traceweave

# Run the measured experiment (deterministic; regenerates results.json)
./traceweave experiment --n 120 --seed 20261023 --dir run --out results.json

# Inspect a trace
./traceweave tree  --store run/traces.jsonl <trace-id>
./traceweave span  --store run/traces.jsonl <trace-id> llm.generate

# Replay it: re-execute from span payloads, diff against the recording
./traceweave replay --store run/traces.jsonl <trace-id>

# Attribute a wrong answer (needs a ground-truth file: clean rows)
./traceweave attribute --store run/traces.jsonl --truth truth.json <trace-id>

# Or run the demo service and make your own traces
./traceweave serve --addr :8080 --dir demo
curl 'localhost:8080/answer?user=u1&product=p1'
```

## The debugging workflow

1. A user reports a wrong answer. You have its trace id.
2. `replay` re-executes the request from the trace. If the replayed output
   is byte-identical (it is, for every trace in the measured workload),
   the trace faithfully describes the failure — you are debugging the
   real thing, not a reconstruction.
3. `attribute` runs counterfactual surgery: it swaps each span's recorded
   context for trusted context (clean DB rows, the reference template, a
   non-quirk seed), one span at a time, and replays. The first swap that
   turns the answer correct names the responsible span.
4. You fix that span's real-world counterpart (the stale replica row, the
   bad template deploy, the seed handling) — with proof, not vibes.

## Measured results

Workload: 120 support-answer requests, faults injected into exactly one
span of 32 requests (a poisoned product row, a faulty prompt template, or
a model-quirk seed), ground truth known to the harness. Full numbers in
`results.json`; method and limits in `PAPER.md`.

| Metric | Result |
|---|---|
| Replay determinism (byte-identical re-execution) | **120/120 = 1.000** |
| Wrong answers | 32 (all faulted; 0 wrong among unfaulted) |
| Attribution accuracy — traceweave (counterfactual surgery) | **32/32 = 1.000** |
| Surgery fix rate (answer correct after fix at attributed span) | **32/32 = 1.000** |
| Attribution accuracy — log-only baseline (names + timings only) | 4/32 = 0.125 |

The baseline is the honest contrast: an engineer staring at the trace
waterfall, attributing to the slowest span. Timings carry no signal about
*content* faults, and it shows.

## Honest limits

- The mock LLM is deterministic **by design** — that is what makes replay
  byte-exact here. For real LLMs, replay determinism requires capturing
  seed/temperature (traceweave records both) **and** a model that honors
  them; hosted models often don't, bit-for-bit. The boundary is stated,
  not hidden: traceweave guarantees hermetic context reconstruction; output
  determinism is exactly as strong as your model's seed discipline.
- Single node, single process demo; the store is one append-only file.
- The demo pipeline is linear (two reads, one prompt, one call). The
  attribution procedure is per-span counterfactual and assumes a single
  faulty span per request — multi-fault attribution is future work.
- Span payloads include full prompt text and DB rows. In production that
  is sensitive data; a real deployment needs payload redaction/encryption
  policy, which is out of scope for this build.

## Tests

```bash
go test ./...   # 10 tests: propagation, capture completeness,
                # replay determinism, surgery, attribution, workload invariants
```
