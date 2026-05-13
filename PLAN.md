# llmtap — remediation ledger

Two adversarial reviews (2026-05-11) surfaced 29 numbered remediation
items across Critical / High / Medium tiers. Eight additional Low-tier
polish items followed. A third adversarial review (2026-05-12) added
ten more (C1–C10) targeting the auth gate, breaker bookkeeping, cert
rotation, and provider/redaction coverage. **All 47 items are closed
as of v0.1.4.** This document is now a forward-looking ledger.

The per-commit audit trail lives in `git log`. Each remediation
commit carries a `Closes A<n>` / `Closes L<n>` / `Closes C<n>` footer
and an evidence block in the body — `git log -p <commit>` reads as
the full remediation record.

The README's "Roadmap" section is authoritative for what's next; this
document just tracks what hasn't been started yet.

---

## Open work (v0.2+)

These are new capabilities, not security remediations. None of them
are gating production use of v0.1.x — that bar was cleared by the
A-series.

### B1 — Additional provider parsers

- **What:** Bedrock, Gemini, Ollama-native wire formats. Each gets a
  `provider.Provider` implementation in `internal/provider/`,
  alongside the existing OpenAI and Anthropic parsers. Tool-call
  TTFT and finish-reason handling per provider's streaming schema.
- **Why:** Bedrock-Anthropic and Gemini-via-OpenAI-compat already
  work (the proxy is wire-format-aware, not vendor-aware), but
  native Bedrock + Gemini parsers expose richer attribute sets
  (`gen_ai.request.bedrock.*`, Gemini's `safetyRatings`, etc.).
  Ollama-native unlocks local-model dev loops.
- **Done when:** Three new parsers ship, each with their own
  `*_test.go` covering ParseRequest / ParseResponseJSON / WrapStream.
  README upstreams matrix updated.

### B2 — `llmtap record` / `replay` / `diff` for migration A/B

- **What:** Three new subcommands.
  - `llmtap record --out replay.ndjson` — captures the raw request /
    response pairs flowing through the proxy.
  - `llmtap replay replay.ndjson --against api.example.com` — re-runs
    the captured requests against a different upstream.
  - `llmtap diff a.ndjson b.ndjson` — semantic diff (model parameters,
    finish reasons, content) suitable for "moving from gpt-4o to
    gpt-4o-mini, where do the answers actually change?".
- **Why:** Model migrations are an under-tooled corner of the LLM
  ops space. A capture-and-replay loop turns the proxy from passive
  observability into an A/B testing harness.
- **Done when:** New `internal/replay/` package with record + replay
  primitives. New top-level `cmd/llmtap` subcommands. End-to-end
  test using httptest fixtures for both providers.

### B3 — Cost / error-biased sampling

- **What:** `telemetry.sample_strategy: cost_weighted | error_biased
  | random`. Cost-weighted samples requests with probability
  proportional to their predicted cost (so expensive calls are
  always traced, cheap ones at low rate). Error-biased samples 5xx
  at 1.0, 4xx at 0.5, 2xx at the configured base rate.
- **Why:** Today every llmtap deployment with `sample_ratio < 1.0`
  loses cheap and expensive calls at the same rate — the wrong
  trade-off for cost dashboards (which want every expensive call
  intact). A bias toward errors gives debugging surface without
  bloating spans on the happy path.
- **Done when:** New samplers wired into the OTel TracerProvider.
  Documented in README's PromQL examples (sampled-rate math
  changes). Existing `sample_ratio` keeps its meaning under the
  new `random` strategy.

### B4 — Stricter readiness signal

- **What:** Replace the "set to true once Setup returns" loose
  readiness flag (A19) with a real signal: `/readyz` returns 200
  only after at least one OTLP export round-trip has succeeded.
- **Why:** The loose signal can flap green while the OTLP endpoint
  is unreachable — the BatchProcessor accepts spans into its buffer
  until the buffer is full. A k8s rolling update could route
  traffic before the proxy can actually emit telemetry.
- **Done when:** Hook into the OTel SDK's
  `BatchSpanProcessor.ForceFlush` / `Logger.LogRecord` callbacks
  (or use a custom Exporter wrapper) to flip the ready flag on
  first success. Two tests: ready=false until export succeeds,
  ready=true after.

### B5 — Per-upstream rate limit

- **What:** `upstreams[].rate_limit: { rps: int, tpm: int }`. Token
  bucket per upstream, separate from the existing `max_in_flight`
  concurrency cap (A12). On overflow: 429 with the existing
  `Retry-After` shape.
- **Why:** A burst of N concurrent requests is bounded by the
  semaphore today, but a steady stream of `rps` × `max_in_flight`
  requests is not. Some operators want to keep total spend below
  a budget; `tpm` (tokens per minute) addresses that directly,
  since the proxy already knows the per-call token count from
  `ParseResponseJSON`.
- **Done when:** New `internal/ratelimit/` package using
  `golang.org/x/time/rate`. Per-upstream `*rate.Limiter`,
  consulted before the concurrency-cap acquire. Tests for the
  RPS path; TPM path is harder to test deterministically — accept
  a soak-test in the load harness.

---

## Closed work (reference)

The 29 A-series + 8 L-series items below were all delivered
in v0.1.3. Each line lists the commit SHA where the fix landed;
`git show <sha>` displays the full evidence block.

### Critical / High (round-1 adversarial review)

| Item | One-line | Commit |
|---|---|---|
| A1  | Cap metric-label cardinality on `gen_ai.*.model` | `c9986c6` |
| A2  | Close error-body snippet leak under `content.mode=off` (uncovered the `modify`-never-runs root cause) | `fe776f4` |
| A3  | Forward oversize bodies intact; 413 above hard cap | `b449b5f` |
| A4  | Forward 4xx error bodies intact via tee snippet | `93ff1f3` |
| A5  | Keep streaming metrics tied to their trace context | `3a4485d` |
| A6  | Strip Accept-Encoding so gzip responses parse | `e266ab0` |
| A7  | Externalize pricing catalogue via embed + override | `fa52708` |
| A8  | Make `--log-level` actually change verbosity | `b426c4e` |
| A9  | Refuse insecure OTLP to non-local endpoints | `c74d37e` |
| A10 | Bearer-token allow-list at the proxy boundary (argon2id) | `4162a35` |
| A11 | Bound inbound body reads via Server.ReadTimeout | `2a92761` |
| A12 | Cap concurrent in-flight per upstream | `07de838` |
| A13 | Bound the SSE parser buffer with overflow signal | `07dd459` |
| A14 | Demo compose stack defaults to TLS, not `allow_insecure` | `b45eea6` |
| A15 | PR-gating CI + signed release pipeline | `1033fe2` |
| A16 | Content redaction profiles at the proxy boundary | `1f9bd3a` |

### Medium (round-2 adversarial review)

| Item | One-line | Commit |
|---|---|---|
| A17 | Deterministic pricing prefix lookup (sort+walk; later replaced by trie in L6) | `ecbee73` |
| A18 | Upstream cert pinning by SPKI sha256 (resumption-safe) | `22f8391` |
| A19 | `/healthz` + `/readyz` probes ahead of auth gate | `22f8391` |
| A20 | Graceful shutdown waits on active streams | `00b1197` |
| A21 | SSE parser accepts CRLF + CR framing per HTML5 EventSource spec | `cc7f317` |
| A22 | Strict segment-aware operation-path matching | `cc7f317` |
| A23 | Hot reload TLS certificates from disk | `00b1197` |
| A24 | Three-state circuit breaker per upstream | `22f8391` |
| A25 | OTel module version alignment | `ee189c4` *(closed incidentally via the A15 dep-bump sweep)* |
| A26 | TTFT fires on tool-call streams (both providers) | `cc7f317` |
| A27 | Tune per-upstream `*http.Transport` for real concurrency | `22f8391` |
| A28 | Stream non-streaming JSON responses through | `22f8391` |
| A29 | Content-Length pre-check | `b449b5f` *(closed incidentally via A3's `captureHeadAndForward`)* |

### Low (post-Medium polish)

| Item | One-line | Commit |
|---|---|---|
| L1  | `LLMTAP_OTLP_HEADERS` env support | `bab4b63` |
| L2  | `LLMTAP_SAMPLE_RATIO` env support (with clamp) | `bab4b63` |
| L3  | `runtime/debug.ReadBuildInfo` fallback for `go install` users | `00a7faa` |
| L4  | SECURITY.md disclosure policy + Dependabot config | `2293f93` |
| L5  | `gen_ai.client.cost.usd` is now a histogram; sibling `.total` counter | `ed488ce` |
| L6  | O(K) byte-trie pricing lookup (168 ns/op at N=500) | `4d1c0ea` |
| L7  | README honesty pass against post-A28 reality | `36c60a5` |
| L8  | Go-native load test harness + CI gate | `7d5a633` |

### Round 3 (round-3 adversarial review, 2026-05-12)

The third pass focused on the auth gate (DoS amplification surface),
breaker bookkeeping (probe-slot leak), cert rotation (torn-read),
SSE overflow visibility, and gap-fill on the redaction catalogue.

| Item | One-line | Commit |
|---|---|---|
| C1  | Release breaker probe slot on every passthrough exit | `7064fd4` |
| C2  | Auth shape-check + 60s LRU cache before argon2 | `7064fd4` |
| C3  | Loop-constant-time verifier across stored hashes | `7064fd4` |
| C4  | Atomic cert+key load defeats torn-read at rotation | `1629b74` |
| C5  | Extend default redaction to Google/Groq/Replicate/GitHub | `d76cd2f` |
| C6  | SSE overflow as counter, not one-shot bool | `75f9430` |
| C7  | `ServeHTTP` refactored into a flat pipeline | `7064fd4` |
| C8  | `crypto/subtle.ConstantTimeCompare` for SPKI pin match | `7064fd4` |
| C9  | NumCPU-bounded argon2 semaphore; 429 on overload | `7064fd4` |
| C10 | Skip SNI ServerName for IP-literal upstreams | `7064fd4` |

C1, C2, C3, C7, C8, C9, C10 co-committed because the `ServeHTTP`
refactor (C7) is load-bearing on C1's single-release-point design and
the auth verifier surface change (C2/C3/C9) cascades into proxy's
admission gate. `git log -p 7064fd4` reads as the seven-item record.

### CI / release pipeline tail

Three follow-up `fix(ci)` commits closed gaps surfaced when the
release pipeline first fired against `v0.1.0`:

| Commit | What |
|---|---|
| `4419696` | Bump Dockerfile Go base to 1.26-alpine (go.mod's 1.25.0 floor) |
| `92b58b7` | docker login in `sign-image` job so cosign can push the signature |
| `1d41c17` | Filter release-job artifact download to skip the flaky `.dockerbuild` blob |
| `63aa480` | Isolate the load test behind a `//go:build loadtest` tag so `-race` runs don't trip its p99 budget |
