# llmtap — Software Development Plan (Round 4)

**Status:** Draft v1 · 2026-07-14 · **progress:** M0 + M1 + M2 + M3 (D1–D41) implemented and protocol/compliance-verified on the working tree; launch track / v0.4 next
**Inputs:** `docs/reviews/2026-07-14-comprehensive-review.md` (findings R1–R14,
M1–M19), `docs/PRD.md` (FR-1…FR-25). Item IDs below are **D-series**
(continuing the A/L/C ledger convention in `PLAN.md`); each carries its
source finding and PRD requirement so commits can close both.

Milestones: **M0** supply-chain afternoon → **v0.1.5** correctness release →
**v0.2** adoption/operability release → **v0.3** growth release. Estimates are
focused solo-dev days.

---

## M0 — Supply-chain afternoon (~0.5 day, do first, no dependencies)

| ID | Task | Source | Est |
|---|---|---|---|
| D1 | SHA-pin all third-party actions in `ci.yml`/`release.yml` (keep slsa-generator tag-pinned — verifier requires it); digest-pin Docker base images | R11 / FR-15 | 2h |
| D2 | Pin syft install (tagged ref + version, kill `curl \| sh` from main); pin govulncheck version | R11 / FR-15 | 30m |
| D3 | Release hardening: drop `\|\| true` on provenance copy (assert file exists), signed `checksums.txt`, guard `:latest` on pre-release tags, deterministic build date | DevOps M6 / FR-15 | 2h |
| D4 | Add `.dockerignore`; PR-time `docker build` job in CI; align go.mod ↔ CI Go versions | M7 / FR-15 | 1h |

## Milestone 1 — v0.1.5 "the promises are true" (~2 weeks)

Fix everything a first-time evaluator or the adversarial audit can point at.

### Lane A — Routing & proxy correctness

| ID | Task | Source | Est |
|---|---|---|---|
| D5 | **Prefix-strip in `Rewrite`** (`strip_prefix`, default true when prefix isn't the upstream's real path); e2e test with prefix ≠ upstream path; fix the Anthropic 404 | R1 / FR-1 | 1–2d |
| D6 | Segment-aware prefix matching (`/v1foo` no longer matches `/v1`); route on cleaned path consistently with enrichment | M5 / FR-13 | 0.5d |
| D7 | Breaker declassification: `errors.Is(err, context.Canceled)` and `errBodyTooLarge` excluded from failure accounting; mid-stream cap → 413 not 502; ErrorHandler records error on span; map status 0→502 in `finishSpan`/`recordMetrics` | R4, R7(part) / FR-5, FR-8 | 1d |
| D8 | Shutdown reorder: `server.Shutdown` first/concurrent, `WaitForStreams` as bounded post-check; shutdown-under-sustained-load test | R6 / FR-6 | 1d |
| D9 | Reject `content.mode: logs` in validation with "not implemented" (full implementation moves to D31) | R5 / FR-4 | 0.5d |

### Lane B — Demo & docs truthfulness

| ID | Task | Source | Est |
|---|---|---|---|
| D10 | Fix dashboard + README PromQL against actual OTLP→Prometheus translation (`_USD_total`, `_sum` unit-suffix behavior); add per-PR test: every dashboard query references an emitted metric name | R2 / FR-2 | 0.5d |
| D11 | Quick-start rewrite: insecure-loopback 60-second demo first; TLS as production path; `docker compose cp` cert flow (or bind-mount `./data/tls` matching the existing comment + .gitignore); copy-pasteable curl with real body; SDK trust matrix (Python httpx `SSL_CERT_FILE`, Node `NODE_EXTRA_CA_CERTS`, Go, curl) | R3 / FR-3 | 0.5d |
| D12 | Compose smoke test in CI: up → healthz → proxied request against stub upstream → assert cost/token series non-empty in Prometheus | R2, M7 / FR-2 | 1d |
| D13 | Pricing refresh: current OpenAI/Anthropic/Gemini families; `llmtap.pricing.misses.total` counter; snapshot date in `llmtap version`; CI corpus test `TestCorpusModelsAllPriced` | R9 / FR-7 | 1d |
| D14 | Fix `make run`; `llmtap version`/help → stdout; scrub A/C-codename references from SECURITY.md, compose comments, config doc comments; fix "any LLM API" claim; create CHANGELOG.md (backfill v0.1.0–v0.1.4) + CONTRIBUTING.md + issue/PR templates | M6, marketing / launch | 1d |

### Lane C — Config & redaction safety

| ID | Task | Source | Est |
|---|---|---|---|
| D15 | Strict YAML decode (`KnownFields(true)`); stderr warnings on unparseable env booleans / malformed OTLP header pairs (match the sample-ratio precedent); target URL scheme+host validation | M4 / FR-13 | 0.5d |
| D16 | `llmtap check [--config]` subcommand reusing Load+Validate | M6 / FR-13 | 0.5d |
| D17 | Redact-before-truncate on error snippets; cap `assembled` (64–256 KiB + `llmtap.content_truncated` attr); set OTel span attribute-length limits via `WithSpanLimits` | M1, M3 / FR-16 | 1d |
| D18 | Redaction pattern additions: raw PEM blocks, `hf_`, `xai-`; README privacy note on unanchoreable formats (Azure hex, AWS secret key) | M2 / FR-16 | 0.5d |

**v0.1.5 exit gate:** clean-machine quick start <10 min; Anthropic e2e green;
compose smoke test green; corpus fully priced; adversarial backlog items 1–5,7
closed.

## Milestone 2 — v0.2 "an SRE will deploy this" (~3–4 weeks)

### Lane D — Refactors that unblock everything (do first in M2)

| ID | Task | Source | Est |
|---|---|---|---|
| D19 | Extract `requestObserver` struct from `responseInterceptor` (explicit fields, unit-testable finalize/metrics/span paths; document or atomicize the single-goroutine invariant) | M14 | 2d |
| D20 | Split provider parsing from span emission (`decode() → typedStruct` + `emit(span, ...)`); extend `Info` with a params bag; shared `streamCollector` for WrapStream mechanics; drop dead `onFirstToken` param; move path helpers to `paths.go`; move provider-name validation from config to the registry check | M12, arch #6 | 3d |
| D21 | Dead-surface sweep: `Providers.Logger` (consumed by D31 or deleted), `Info.FirstByteAt` → TTFB metric, `pricing.Cost` alias | arch #13 | 0.5d |

### Lane E — Operability (SRE gate)

| ID | Task | Source | Est |
|---|---|---|---|
| D22 | Self-metrics batch: `llmtap.breaker.state` + trip/recover log line, `llmtap.auth.rejected.total`, `llmtap.streams.active`, `llmtap.requests.rejected.total{reason}`; `llmtap.requests.total` counts all outcomes incl. passthrough | R7 / FR-8 | 1–2d |
| D23 | `otel.SetErrorHandler` → slog + `llmtap.telemetry.export_errors.total`; cert observability (`llmtap.tls.cert_not_after` gauge, reload-failure counter, startup near-expiry warning) | R7, M11 / FR-8, FR-13 | 1d |
| D24 | True readiness (PLAN B4): flip on first successful OTLP export; flip false on SIGTERM; document preStop; README wording fixed | R8 / FR-9 | 2–3d |
| D25 | Post-header stream idle timeout (configurable) with span event + metric | R13 / FR-10 | 1–2d |
| D26 | Production defaults: `max_in_flight` + breaker in `Default()` (or shipped `config.production.yaml`); README "Sizing" section with measured numbers; GOMEMLIMIT guidance; JSON error body on proxy-generated 502 + request/trace ID header on error responses | M9, M15 / FR-11 | 1d |
| D27 | Helm chart + k8s examples (probes, PDB, preStop, resources, cert-manager Certificate, NetworkPolicy egress recipe); `auth.tokens_file`; document per-replica breaker/cap/cardinality semantics; container `HEALTHCHECK` via `llmtap healthcheck` subcommand + OCI labels | M8 / FR-12 | 3–4d |
| D28 | Dashboard v2: error-rate + self-health row + starter alert rules; publish to Grafana.com marketplace | R7 / FR-14 | 1d |

### Lane F — Security & test debt

| ID | Task | Source | Est |
|---|---|---|---|
| D29 | Auth DoS: negative-result cache / rate limit ahead of argon2; document allow-list ceiling; drop inbound `baggage` by default | R12, M19 / FR-17 | 1d |
| D30 | Test-debt batch 1: `internal/telemetry` tests via in-memory OTLP collector (raise gate to 70%, add per-package floors); breaker FSM unit tests on the existing fake clock (delete the sleeps); config precedence chain + full env table | R14, QA / v0.2 gate | 3d |
| D31 | Implement `content.mode: logs` on the existing log pipeline (consumes `Providers.Logger`); or formally cut it and remove from docs/config | R5 / FR-4 | 2–3d |
| D32 | Test-debt batch 2: fuzz targets (SSE tee, redact idempotence, config, PHC) + CI fuzz job; client-abort mid-stream test; streaming redaction chunk-boundary test; golden span/metric snapshots | QA #3,5,6,9 | 2d |
| D33 | Load harness realism: streaming scenario, real meters, heap-delta assertion; publish benchmark numbers (feeds marketing) | M16 / FR-11 | 1d |
| D34 | Privacy & compliance docs section (GDPR posture, retention, residency); NOTICE files in release artifacts | M18 / FR-18 | 0.5d |

**v0.2 exit gate:** alertable breaker/export/cert signals from the shipped
dashboard; Helm install smoke-tested; telemetry ≥70% w/ per-package floors;
half-open recovery-cliff behavior documented (fix optional: multi-probe
half-open is a candidate stretch item).

## Milestone 3 — v0.3 "growth" (~4–5 weeks, order within milestone flexible)

| ID | Task | Source | Est |
|---|---|---|---|
| D35 | **OpenAI Responses API parser** (streaming + tool/built-in-tool events) on the D20 seams | R10 / FR-19 | 4–5d |
| D36 | 2026 signal gap-fill: reasoning/cached-token breakdown + corrected cost math, Anthropic `thinking_delta` TTFT, `error.type`, `service_tier`/`refusal`, emitted `schema_url`, semconv compliance matrix; recorded-payload fixture corpus in `testdata/` | M17 / FR-21 | 3d |
| D37 | Per-caller cost attribution (token/mTLS-CN/header → `caller` attr + capped metric label) | PM #5 / FR-20 | 1–2d |
| D38 | Instant feedback: per-request summary log line / `llmtap tail`; mock-upstream demo mode (no API key); `llmtap gen-cert` | PM #6, DX / FR-22 | 2–3d |
| D39 | Gemini native parser + README compatibility matrix (OpenRouter, vLLM, Ollama-compat, Azure, Groq) | PM #7 / FR-23 | 3d + 0.5d |
| D40 | Cost-weighted sampling (parse hoisted above span start per D20); ADR conceding error-biased needs processor/tail sampling | M13 / FR-25 | 2d |
| D41 | record/replay/diff (PLAN B2) on the D19/D20 seams; raw capture inherits redaction | PLAN B2 / FR-24 | 5d+ |

### Launch track (parallel, mostly non-code)

Topics/homepage/badges (1h) → screenshot+GIF (0.5d) → positioning rewrite +
comparison table (1d) → Grafana marketplace publish (0.5d) → benchmark post
(uses D33) → Show HN week (after v0.2) → OTel GenAI SIG engagement + CFP →
"AI coding agent governance" recipe (0.5d, zero code). OpenSSF Best Practices
badge. Move security contact off the employer address.

## Sequencing rationale

- **M0 first** because the pinning gaps contradict the project's core trust
  claim and cost half a day.
- **D5 (prefix strip) is the release blocker** — half the product's advertised
  surface 404s. D10–D12 restore the demo; together they make the README true.
- **D19/D20 gate all of M3**: Responses API, Gemini, record/replay, and
  sampling all land on `responseInterceptor` and the provider seam. Building
  three new parsers before the `streamCollector` extraction quintuples
  boilerplate; building record/replay before the observer refactor lands it in
  the least testable code in the repo.
- **Operability (Lane E) before growth (M3)**: the SRE review's verdict was
  "conditional no" — features don't matter if nobody can run it in the request
  path. Items D22–D24 (~1 week) reach canary-grade.
- **B5 (rate limits) is deliberately absent**: de-scoped per PM review to
  protect the "passive tap" identity. Revisit only on user demand.

## Effort summary

| Milestone | Focus | Est |
|---|---|---|
| M0 | Supply chain | 0.5 d |
| v0.1.5 | Correctness + demo truth | ~10 d |
| v0.2 | Refactor seams + operability + test debt | ~18–22 d |
| v0.3 | Responses/Gemini/attribution/replay + launch | ~20–25 d |
