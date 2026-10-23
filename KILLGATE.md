# traceweave — Kill-Gate Analysis

## The claim under test

> Tracers RECORD spans; LLM observability tools log prompts; nobody ships a
> tracer whose traces are REPLAYABLE — traceweave captures, per span, the full
> materialized context (the exact prompt string sent, the exact DB rows
> returned, the model parameters and seed) so that any request can be
> deterministically RE-EXECUTED later and its output diffed against the
> original, turning "the model hallucinated" into a bisectable question:
> which span's context first made the wrong answer inevitable.

## What already exists (searched 2026-10-23)

| Tool | What it does | Replay? | DB context? | Surgery / attribution? |
|---|---|---|---|---|
| OpenTelemetry (+ W3C Trace Context) | Span/trace recording standard; attributes, events, propagation | No — spans are records, nothing re-executes them | No — attributes by convention, row payloads not captured | No |
| Langfuse | OTel-based LLM tracing: prompts, completions, cost per generation span; prompt registry | "Replay" = re-run a prompt in the playground / re-send one LLM call | No — retrieval spans store snippets as text, not the DB rows a query returned as a span payload | No |
| LangSmith / Helicone / Arize Phoenix | Same class: log prompts + completions, evals, dashboards | Prompt-level re-run only | No | No |
| Lookspan (dev.to, v0.4.0) | Local-first observability for LLM apps; captures prompts/responses/tool calls; "replay & diff" re-runs a captured trace and compares outputs | Partial — re-runs the LLM calls from captured prompts to catch prompt/model regressions | No — no database span, no row payloads, no request-level context reconstruction | No — diff shows outputs changed, not which upstream context caused it |
| Agentfork (Oct 2026) | Forks an LLM *agent chain* at any step and replays the remainder to test code changes | Partial — re-executes subsequent steps LIVE (re-calls tools/LLMs against real systems); non-hermetic by design | No — agent state, not DB rows in a request trace | No — forking is manual trial, not counterfactual attribution of a wrong answer to a span |
| AgentSpec replay | Replays a recorded agent trace by playing back recorded tool results | Playback only — explicitly does NOT re-execute the LLM | No | No |
| LMCache trace replay | Replays recorded storage-API calls against a fresh cache server for regression/perf hunting | Different layer (KV-cache storage ops), no LLM context at all | n/a | No |
| replay.io | Time-travel debugging for browser JS | Different domain entirely | n/a | n/a |
| Temporal + Langfuse plugin | Workflow replay exists in Temporal, but traces are deliberately made replay-*invisible* (deterministic span IDs, export suppressed) — the opposite goal | Workflow state, not request debugging | No | No |

## The precise differentiation

No existing tool combines all three of:

1. **A span-tree tracer** (W3C-style propagation) spanning HTTP request →
   DB query → LLM call in ONE trace — the OpenTelemetry shape, not an agent
   framework or an LLM-call logger.
2. **Materialized context per span**: the DB span carries the actual rows
   returned (payloads, not just SQL text), the LLM span carries the exact
   prompt bytes + model parameters + seed. A trace is therefore a complete,
   hermetic description of the request's inputs.
3. **Hermetic re-execution + counterfactual surgery + attribution**:
   replay rebuilds the prompt from the recorded DB rows (never the live DB),
   re-runs the model with the recorded seed, and diffs byte-for-byte.
   Editing one span's payload and replaying ("context surgery") is a
   counterfactual experiment; running it per candidate span is an automated
   bisection that attributes a wrong answer to the exact span whose context
   caused it — with accuracy measured against a log-only baseline.

Lookspan comes closest on (a weak form of) replay but has no DB context, no
hermetic request reconstruction, and no surgery/attribution. Agentfork forks
agent workflows but re-executes live and attributes nothing.

## Neighbours in Habib's own portfolio (not prior art, but must not collide)

- **report-fidelity**: scores whether an agent's *report* faithfully covers
  its execution context. Different object (report vs. trace), no replay.
- **pauseproof**: tamper-evident hash-chained audit log for human approvals.
  Integrity of the record, not re-execution of the request.

## Verdict

**PASS.** The stop condition ("a replayable-tracing tool already exists as
an available product") is not met for the claim as precisely stated:
replayable *request traces* with materialized DB context, hermetic
re-execution, and counterfactual attribution. The claim is narrowed to
exactly that, and Lookspan + Agentfork are named in the README/PAPER as the
nearest prior art with their limits stated.
