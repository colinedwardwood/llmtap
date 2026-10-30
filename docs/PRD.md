# PRD — llmtap: the OTel wiretap for LLM traffic

**Status:** Draft v1 · 2026-07-14
**Source:** eight role-based reviews + adversarial audit (`docs/reviews/2026-07-14-comprehensive-review.md`)
**Scope:** v0.1.5 (correctness release), v0.2 (adoption release), v0.3 (growth release)

---

## 1. Problem

Engineering teams ship LLM features faster than they instrument them: surprise
five-figure bills, agents that fail undebuggably, no audit trail of what leaves
for third-party models. Existing tools are either **gateways** (LiteLLM,
Portkey, Envoy AI Gateway — active middleboxes with routing opinions) or
**destinations** (Langfuse, LangSmith, Helicone — a sixth data silo with its
own SDK/backend). Platform teams that already pay for an o11y stack want
neither.

## 2. Product thesis

> **llmtap is the wiretap, not the destination.** A single Go binary between
> your app and OpenAI/Anthropic-compatible APIs that emits OTel GenAI-semconv
> traces, metrics, and cost to the backend you already pay for. No SDK, no new
> UI, no routing opinions, no sixth silo.

Defensibility comes from discipline (passive, tiny, vendor-neutral) plus two
hard-to-replicate assets: the security/supply-chain posture and being the
**reference-quality GenAI semconv emitter**. The structural long-term threat is
absorption by Envoy AI Gateway / OTel collector+eBPF; the counter is to outrun
on simplicity and stay aligned with the semconv upstream.

**Explicit non-goals:** routing/failover/caching/guardrails (gateway
territory), any UI (destination territory), per-upstream rate limiting beyond
a minimal cap (PLAN.md B5 is de-scoped to "keep minimal or cut").

## 3. Users

| Persona | Job to be done | Wedge |
|---|---|---|
| **P1 Platform/SRE engineer** (50–500-eng org, runs Grafana/Datadog/Honeycomb) | "What are our 12 product teams spending on OpenAI, and which team is it?" | Zero-code cost dashboard + **per-caller chargeback** in the stack they already run |
| **P2 Security/compliance engineer** | Audit trail of what leaves for third-party models; hard egress policy boundary | mTLS + NetworkPolicy recipe making llmtap a *mandatory* tap |
| **P3 Eng leadership governing AI coding agents** | Visibility into Claude Code / Cursor / aider fleet spend and behavior | Base-URL-level tap covers every agent uniformly; packaging + dashboard only, no new code |

Not for: solo devs (Helicone/Langfuse free tiers) or teams needing
routing/failover (LiteLLM).

## 4. Success metrics

| Metric | Target |
|---|---|
| Time-to-first-span (clone → span visible in Grafana) | < 5 min, no API key required |
| Enriched vs passthrough request ratio on live traffic | > 90% on OpenAI+Anthropic+Responses traffic |
| Pricing-table coverage of a current-model test corpus | 100%, enforced by CI test |
| p99 proxy-added latency (published, streaming + non-streaming, with real export) | < 10 ms non-streaming; TTFT delta ≈ 0 |
| Proxy self-health alertability | Breaker state, export failures, active streams, rejects all metered; runbook shippable |
| Non-author contributors (pricing PRs count) | > 0 within one quarter of launch |
| Ecosystem citations (OTel registry, Grafana marketplace, backend-vendor docs) | Listed in all three |

## 5. Requirements

Priorities: **P0** = broken promise, release-blocking. **P1** = required for
the adoption release. **P2** = growth.

### 5.1 Correctness of the core promise (P0)

- **FR-1 Prefix-strip routing.** Requests to a configured upstream prefix MUST
  reach the upstream with the routing prefix removed (opt-out
  `strip_prefix: false` for prefixes that are real upstream paths, e.g. `/v1`).
  Acceptance: e2e test where routing prefix ≠ upstream path; documented
  Anthropic flow works against a fixture of the real API surface.
- **FR-2 Correct dashboard + PromQL.** Shipped dashboard panels and README
  queries MUST return data against the shipped compose stack (accounting for
  OTLP→Prometheus name translation incl. unit suffixes). Acceptance: CI test
  asserting every dashboard query references an emitted series name; compose
  smoke test sees non-empty cost and token panels.
- **FR-3 Executable quick start.** Every command in README quick start MUST
  work as written (cert via `docker compose cp` or bind mount; copy-pasteable
  curl with a real body; per-SDK trust matrix: `SSL_CERT_FILE`,
  `NODE_EXTRA_CA_CERTS`, Go, curl). Insecure-loopback demo becomes the
  60-second happy path; TLS becomes the clearly-labeled production path.
- **FR-4 `content.mode: logs` honesty.** Either implemented via the existing
  log pipeline or rejected at validation with "not implemented". No silent
  no-op.
- **FR-5 Breaker correctness.** Client disconnects (`context.Canceled`) and
  mid-stream body-cap aborts MUST NOT count as upstream failures. Mid-stream
  cap violations SHOULD return 413, not 502.
- **FR-6 Shutdown ordering.** Stop accepting connections before/concurrently
  with stream draining; a deploy under sustained load MUST NOT hard-kill
  short requests. Readiness flips false at SIGTERM.
- **FR-7 Fresh pricing with loud misses.** Current-generation OpenAI /
  Anthropic / Gemini model families priced; `llmtap.pricing.misses.total`
  counter; pricing snapshot date in `llmtap version`; CI test that the fixture
  corpus resolves 100%.

### 5.2 Operability (P1) — "an SRE can put this in the request path"

- **FR-8 Self-health metrics.** `llmtap.breaker.state` (gauge/upstream),
  `llmtap.auth.rejected.total`, `llmtap.streams.active`,
  `llmtap.requests.rejected.total{reason}`, `llmtap.telemetry.export_errors.total`
  (via `otel.SetErrorHandler`), `llmtap.tls.cert_not_after` + reload-failure
  counter. `llmtap.requests.total` counts *all* outcomes incl. passthrough and
  rejects; upstream-error spans/metrics map status 0→502 with error recorded.
- **FR-9 True readiness (PLAN B4).** `/readyz` 200 only after first successful
  OTLP export round-trip; flips false on SIGTERM before drain.
- **FR-10 Stream hygiene.** Configurable post-header idle timeout per stream;
  trip emits span event + metric.
- **FR-11 Production defaults & sizing.** Default `max_in_flight` + breaker on
  (or a shipped `config.production.yaml`); README "Sizing" section with
  measured req/s/core and MiB/stream; GOMEMLIMIT guidance.
- **FR-12 Kubernetes packaging.** Helm chart (Deployment, Service, ConfigMap,
  Secret-mounted TLS/tokens, probes, PDB, preStop, resources) + cert-manager
  and NetworkPolicy egress-enforcement examples; documented per-replica
  breaker/cap semantics. `auth.tokens_file` for Secret injection.
- **FR-13 Config safety.** Strict YAML (`KnownFields`), warn on unparseable
  env values, segment-aware prefix matching, `llmtap check --config`
  subcommand, target-URL scheme/host validation, startup warning on
  expired/near-expiry cert.
- **FR-14 Dashboard v2.** Error-rate + proxy-self-health row (breaker, export
  failures, active streams, rejects), corrected metric names, starter alert
  rules; published to Grafana.com marketplace.

### 5.3 Security & privacy hardening (P1)

- **FR-15 Supply-chain pinning.** SHA-pin all third-party actions; pin syft
  install (version + ref, no `curl | sh` from main); digest-pin Docker bases;
  remove `|| true` on provenance copy; signed `checksums.txt`; guard `:latest`
  against pre-release tags; PR-time `docker build`; `.dockerignore`.
- **FR-16 Redaction correctness.** Redact-before-truncate on error snippets;
  cap the `assembled` stream accumulator (with `llmtap.content_truncated`
  attribute) and set OTel span attribute-length limits; add PEM-block, `hf_`,
  `xai-` patterns; document unanchoreable formats (Azure hex, AWS secret key)
  in the privacy section.
- **FR-17 Auth DoS resistance.** Negative-result cache or rate limit ahead of
  argon2; document allow-list size ceiling; drop inbound `baggage` by default.
- **FR-18 Privacy & compliance docs.** "Privacy & compliance" section: capture
  modes as data-processing decisions, residency = collector location,
  retention guidance, redaction is best-effort not a DPA; NOTICE files in
  release artifacts.

### 5.4 Product growth (P2)

- **FR-19 OpenAI Responses API parser** (`/v1/responses`, streaming + tool
  events). The single highest-impact provider gap in 2026.
- **FR-20 Per-caller cost attribution.** Auth token / mTLS CN / configured
  header → `caller` span attribute + cardinality-capped metric label.
- **FR-21 2026 signal gap-fill.** Reasoning/cached-token usage breakdown (and
  cost math), Anthropic `thinking` deltas fire TTFT, `error.type`, TTFB
  metric from the existing `FirstByteAt`, emitted `schema_url`, published
  semconv compliance matrix.
- **FR-22 Instant feedback & demo mode.** Per-request summary log line or
  `llmtap tail`; mock-upstream demo (`llmtap demo` or compose service) so the
  dashboard lights up with zero API keys. `llmtap gen-cert` helper.
- **FR-23 Gemini native parser** + README compatibility matrix (OpenRouter,
  vLLM, Ollama-compat, Azure OpenAI, Groq). Bedrock deferred (SigV4);
  Ollama-native deferred (compat endpoint covers it).
- **FR-24 record/replay/diff (PLAN B2)** — sequenced last; requires the
  decode/emit parser split and observer refactor; raw capture must inherit
  redaction. **FR-25 Cost-weighted sampling (PLAN B3, re-scoped)** — hoist
  parse above span start; error-biased sampling via processor/tail, not head
  sampler.

### 5.5 Launch readiness (marketing, parallel track)

- GitHub topics + homepage + badge row (CI, release, license, Go Report Card,
  SLSA-3, OpenSSF Best Practices); dashboard screenshot + 20 s GIF above the
  fold; CHANGELOG.md (SECURITY.md already promises it), CONTRIBUTING.md,
  issue/PR templates; scrub internal codenames (A-numbers) from user-facing
  files; fix "any LLM API" overclaim; honest comparison table (LiteLLM /
  Helicone / OpenLLMetry); published latency benchmark; Show HN + Grafana
  marketplace + OTel community engagement; "AI coding agent governance"
  recipe. Name-collision mitigation (npm `llmtap` is squatted by a same-space
  project): claim docs URL and marketplace surface early.

## 6. Release gates

| Release | Gate |
|---|---|
| **v0.1.5** | All P0 (FR-1…7) + FR-15 pinning; compose smoke test green in CI; a new user completes the quick start on a clean machine in <10 min |
| **v0.2** | All P1; SRE sign-off criteria: alert on breaker-open, export-failure, cert-expiry from shipped dashboard; Helm install documented and smoke-tested; telemetry package ≥70% coverage, per-package floors |
| **v0.3** | Responses API enrichment ratio >90% on a mixed corpus; caller attribution demo; launch-week assets live |

## 7. Risks

| Risk | Mitigation |
|---|---|
| Envoy AI Gateway / OTel collector absorbs the niche | Ship the adoption release fast; be the reference semconv emitter; engage the OTel GenAI SIG |
| GenAI semconv churn (`gen_ai.system` → `gen_ai.provider.name`) | Emit `schema_url`; compliance matrix per release; track SIG |
| Pricing-table maintenance tax | CI corpus test fails loudly; CONTRIBUTING invites pricing PRs as the first-PR magnet |
| Solo-maintainer bus factor | Keep scope disciplined (non-goals above); prefer boring code; contributor docs |
| Responses API schema complexity underestimated | Build the shared `streamCollector` + decode/emit split first (see dev plan) |
