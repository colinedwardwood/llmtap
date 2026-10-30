# llmtap v0.1.4 — Comprehensive Multi-Role Review (2026-07-14)

Eight role-based reviews (product manager, marketing, principal architect, SRE,
DevOps, security engineer round 4, QA, developer experience) followed by an
adversarial zero-trust audit that re-verified every major claim against source.
This document is the integrated findings record. The PRD derived from it lives
at `docs/PRD.md`; the sequenced work plan at `docs/DEVELOPMENT-PLAN.md`.

**Verdict in one paragraph.** The engineering fundamentals are unusually strong
for a v0.1 (clean package DAG, zero-goroutine streaming tee, careful breaker /
cert-rotation / auth code, signed SLSA-3 releases). But v0.1.4 is optimized for
an adversarial security reviewer, not a first-time adopter or an on-call
engineer. Two of the product's headline promises are broken out of the box —
the documented Anthropic path 404s on every request, and the flagship
cost/token dashboard panels query Prometheus series that don't exist — and the
proxy that exists to make LLM traffic observable is itself nearly unobservable
(no breaker/auth/export-failure/stream metrics, constant-true readiness).

---

## 1. Adversarially confirmed defects (fix before anything else)

| ID | Sev | Finding | Evidence |
|---|---|---|---|
| R1 | **Critical** | **Anthropic routing 404s as documented.** Default upstream prefix `/anthropic` + `ANTHROPIC_BASE_URL=https://host/anthropic` → SDK sends `/anthropic/v1/messages`; `Rewrite` uses `r.SetURL(target)` which *joins* paths, never strips the routing prefix, so api.anthropic.com receives `POST /anthropic/v1/messages` → 404. OpenAI works only because `/v1` coincides with the upstream's real path. No test mounts a prefix ≠ upstream path. | `internal/proxy/proxy.go:155-157`, `internal/config/config.go:237`, `README.md:43` |
| R2 | **Critical** | **Flagship dashboard panels are empty in the shipped demo.** Code emits `gen_ai.client.cost.usd.total` (unit `USD`); Prometheus v2.55's OTLP receiver (suffixes hardcoded on) renders `gen_ai_client_cost_usd_USD_total`. Dashboard queries `gen_ai_client_cost_usd_total`; README queries `gen_ai_client_cost_usd_total_total` — both empty. Token panel `..._token_usage_token_sum` is also wrong (brace-units are skipped → real series is `..._token_usage_sum`). | `deploy/grafana/dashboards/llmtap-overview.json:72,87`, `README.md:214,229`, `internal/telemetry/telemetry.go:306,315` |
| R3 | **Critical** (adoption) | **Quick-start step 2 cannot be executed.** `docker compose exec llmtap cat …` fails — image is distroless (no shell/cat). The compose comment's fallback host path `./deploy/compose/data/tls/tls.crt` is a *named volume* that never appears on the host. Both documented cert paths are dead ends. | `Dockerfile:28`, `README.md:28`, `deploy/compose/docker-compose.yml:15,122` |
| R4 | High | **Circuit breaker poisoned by client disconnects and mid-stream 413s.** `ErrorHandler` writes 502 for *any* upstream error including `context.Canceled` from a client hangup; `release` maps unwritten status→502; breaker counts all 5xx. A client deploy that cancels ≥5 in-flight requests — or five oversized chunked uploads aborted by `capCountingReader` — opens the circuit against a healthy upstream: a traffic-correlated false outage. | `proxy.go:175-181,511-521,976-985`, `breaker.go:124` |
| R5 | High | **`content.mode: logs` is a silent no-op.** Validation accepts it, README documents it, but capture is gated on `Mode == CaptureEvents` only; `Providers.Logger` is constructed and never consumed. Operators believe they have an audit trail they don't. | `config.go:468`, `proxy.go:571`, `telemetry.go:44,129` |
| R6 | High | **Graceful shutdown drains in the wrong order.** `WaitForStreams(shutCtx)` runs *before* `server.Shutdown` while the listener still accepts; sustained traffic burns the whole budget, then `Shutdown` runs with an expired context and hard-kills everything. | `internal/proxy/server.go:151-170` |
| R7 | High | **The proxy is unobservable.** No breaker-state metric or trip/recover log, no auth-rejection counter, no active-streams gauge, no telemetry-export-failure counter (no `otel.SetErrorHandler` anywhere). `llmtap.requests.total` tells three lies: status 0 on upstream errors (modify never ran), passthrough traffic never counted, rejects (401/413/429/503) never counted. Runbooks and alerts cannot be written. | `breaker.go:178`, `proxy.go:195,532-541,679-689` |
| R8 | High | **`/readyz` is theater.** `Ready` is a constant `true` from `Setup`; it can never go false — not on collector outage, not during shutdown. Rolling updates route to pods that can't export or are about to die. README oversells it. (Known as PLAN.md B4; worse than B4 states.) | `telemetry.go:136`, `proxy.go:875-887` |
| R9 | High | **Pricing catalogue is stale and misses silently.** `prices.yaml` is a 2025-Q4 snapshot (tops out at o3-mini / claude-3-5). Unknown models return `(0, false)` with no `pricing.misses` counter — the FinOps dashboard undercounts exactly when spend grows. | `internal/pricing/prices.yaml:1-2`, `pricing.go:185-193` |
| R10 | High | **No OpenAI Responses API parser.** `/v1/responses` — OpenAI's recommended default for all new projects, the Agents SDK substrate — passes through unenriched. The newest traffic is exactly the traffic teams want observed. | `internal/provider/` (absent) |
| R11 | High | **Supply-chain pinning contradicts the project's own posture.** All third-party actions pinned by mutable tag (release jobs hold `id-token: write` + `packages: write`); syft installed via `curl \| sh` from anchore/syft `main`; Docker bases tag-pinned not digest-pinned; `dependabot.yml` comment claims SHA-pinning that isn't implemented. | `.github/workflows/ci.yml`, `release.yml:113-116`, `Dockerfile:2,28` |
| R12 | High | **Auth gate is a CPU-DoS lever.** Only successes are cached; any well-shaped garbage token forces argon2 across *every* stored hash (64 MiB + ~100 ms/core each). A laptop's worth of garbage saturates the NumCPU semaphore and 429s legitimate clients. Cost scales linearly with allow-list size. | `internal/auth/auth.go:252-308` |
| R13 | High | **Hung SSE streams are unbounded and invisible.** `ResponseHeaderTimeout` covers headers only; after first byte there is no deadline anywhere. Zombie streams hold semaphore slots forever, starve the concurrency cap, and permanently wedge `WaitForStreams`. | `proxy.go:250`, `server.go:44-49` |
| R14 | High | **`internal/telemetry` has zero tests** — the package that is the product's purpose. Hidden by the aggregate 60% coverage gate. The breaker's injected fake clock is never used; all breaker timing tests use real sleeps (flake-by-construction). | `internal/telemetry/`, `breaker.go:54`, `ci.yml:36-44` |

## 2. Medium findings

- **M1 — Truncate-before-redact on error snippets**: snippet capped at 1024 bytes, *then* redacted; a key straddling the boundary falls below the regex length floor and ships partially unmasked (`proxy.go:663-668,769`).
- **M2 — Redaction misses 2026 secret shapes**: AWS *secret* access key (only `AKIA…` ID matched), raw PEM blocks (only JSON-embedded caught), `hf_…`, `xai-…`; Azure 32-hex is genuinely unanchoreable — document it (`redact.go:113-158`).
- **M3 — Uncapped `assembled` builder** per stream under `content.mode: events`; bounded by model max output, but produces multi-hundred-KB span attributes that OTLP collectors truncate/reject. No span attribute-length limits set (`openai.go:359`, `anthropic.go:204`).
- **M4 — Non-strict YAML + silent env failures**: typo'd config keys silently ignored (`yaml.Unmarshal`, `config.go:277`); unparseable `LLMTAP_ALLOW_INSECURE`/`LLMTAP_OTLP_INSECURE` booleans and malformed OTLP header pairs silently dropped (`config.go:297-336`).
- **M5 — Prefix matching not segment-aware**: `/v1foo` routes to the `/v1` upstream; routing matches the raw uncleaned path while enrichment cleans it — a parser differential (`config.go:579-592`).
- **M6 — No `llmtap check` subcommand**; validation only reachable by starting the proxy. `llmtap version` prints to stderr; `make run` fails out of the box (`cmd/llmtap/main.go:49-65`, `Makefile:29`).
- **M7 — Docker image never built in CI**; no compose smoke test; dashboard JSON unvalidated; no `.dockerignore`; no `HEALTHCHECK` (`ci.yml`, `Dockerfile`).
- **M8 — No k8s story**: no Helm chart/manifests, no probes/PDB/preStop/GOMEMLIMIT guidance; all protective state (breaker, semaphores, auth cache, label-cardinality set) is per-replica RAM with undocumented multi-replica semantics — including per-replica `_other` bucketing that can split one model's series.
- **M9 — Default config ships unprotected**: `Default()` sets no `max_in_flight` and no breaker; no sizing guidance anywhere (`config.go:232-265`).
- **M10 — Half-open probe serialization**: one probe admitted, everyone else 503s until it resolves; at LLM latencies (60–120 s) recovery is a capacity cliff (`breaker.go:100-113`).
- **M11 — Cert observability gap**: reload failure is a warn-log only; no `cert_not_after` gauge or reload-failure counter — rotation breakage silently ages into an expiry outage (`certmgr.go:224-229`). mTLS client CA is not hot-reloaded, unlike server certs (`server.go:92-103`).
- **M12 — Provider interface fuses parsing to `trace.Span`**: blocks B2 (record/replay/diff needs parsers as pure functions), forces span recorders into parser tests, and B1's richer attributes will be span-only. `WrapStream` closer/TTFT boilerplate is duplicated per provider (~80 lines) and will quintuple under B1 (`provider.go:82-115`, `openai.go:350-426`, `anthropic.go:195-291`).
- **M13 — B3 sampling is mis-specced**: span starts before parse with zero attributes, so cost-weighted head sampling has nothing to bias on; error-biased head sampling is impossible (status unknown at start) — needs a processor/tail approach (`proxy.go:565-574`).
- **M14 — `responseInterceptor` closure pyramid** (190 lines, shared mutable closure state, single-goroutine invariant undocumented) is the attach point for B2/B5 and the hardest code in the repo to test (`proxy.go:638-828`).
- **M15 — Upstream errors return a bare empty-body 502**; SDK users can't tell llmtap from OpenAI. No trace/request ID on proxy-generated errors, hampering complaint-to-span correlation (`proxy.go:175-181`).
- **M16 — Load harness doesn't measure the product**: no streaming, no TLS, no auth, noop meters, exporter-less tracer; README performance claims rest partly on it (`test/load/load_test.go:74-127`).
- **M17 — Provider fixtures are mid-2024 vintage**: no reasoning/cached-token usage fields, no Anthropic `thinking` deltas (TTFT won't fire on extended-thinking streams — the A26 bug one shape later), no `service_tier`/`refusal`, no cache-token cost math.
- **M18 — GDPR/compliance guidance absent**: content capture makes operators processors of whatever users type; no retention/residency/DPA note; redaction presented as the privacy story without caveats. Release artifacts redistribute Apache-2.0 deps without NOTICE reproduction.
- **M19 — W3C `baggage` accepted from clients and propagated upstream** — latent injection channel (`telemetry.go:106-108`).

## 3. Product & marketing findings

- **Positioning is sound but the window is closing.** "The wiretap, not the destination" is defensible discipline, not a moat; Envoy AI Gateway (CNCF, OTel GenAI metrics today) and OTel eBPF/collector genai work are the structural absorbers. Be the reference semconv emitter and the easiest packaging of the idea.
- **ICP**: platform/SRE teams (50–500 eng) already running an o11y stack, answering "what are our teams spending on OpenAI?"; secondarily security/compliance orgs and AI-coding-agent governance (zero new code needed — packaging only).
- **The missing wedge feature is per-caller cost attribution**: auth tokens already identify callers; mapping token→`caller` label turns llmtap from a model-cost tool into a chargeback tool.
- **"Any LLM API" overclaims** (two wire formats today) and the competitor framing strawmans Helicone while omitting LiteLLM/OpenLLMetry.
- **Repo photographs badly**: zero GitHub topics, no homepage URL, no badges, no screenshot/GIF of the dashboard it ships, no CTA, no CHANGELOG (SECURITY.md *promises* one), no CONTRIBUTING/templates, internal codenames (A9/A14/PLAN.md) leaking into user-facing files.
- **Name collision is time-sensitive**: `DivyaanshuXD/LLMTap` (same category, owns `llmtap` on npm, created 2026-03). Claim topics/docs-URL/Grafana-marketplace surface now.
- **Success metrics to track**: time-to-first-span (<5 min target), enriched-vs-passthrough request ratio (truest product-health number), pricing-table PR contributors, production-signal issues, ecosystem citations — not stars.

## 4. What is genuinely strong (protect it)

- Package DAG and dependency direction; constants-only `genai` package.
- Span lifecycle correctness (ends exactly once across EOF/error/Close — verified independently by two reviewers).
- Zero-goroutine streaming tee; `FlushInterval=-1` byte-through design.
- Breaker FSM, cert torn-read defense, SPKI pinning (constant-time, resumption-safe), auth header stripping, no smuggling vector found (round 4 checked TE/CL, hop-by-hop, Host, Via, path traversal, log injection — clean).
- Config validation error messages (aggregate, actionable) and `config.example.yaml` quality.
- Secure-by-default posture; supply-chain *ambition* (cosign/SLSA-3/SBOM) — the pinning gaps (R11) are fixable in an afternoon.
- Test suite shape: behavior-driven, httptest-based, regression-anchored.

## 5. Adversarial survival score

- **Scaling: 35%** — per-replica protective state with no coordination story; zombie streams; wrong-order shutdown.
- **Security: 60%** — TLS/pinning/argon2/redaction work is real; auth CPU-DoS, truncate-before-redact, and uncleaned-path routing drag it down.
- **Maintainability: 40%** — plan-ticket archaeology in comments, zero-test core package behind an aggregate gate, dead interface surface, observability-of-the-observer gaps.

Full role reports are preserved in the agent transcripts for this session
(PM, marketing, architect, SRE, DevOps, security-round-4, QA, DX, adversarial).
