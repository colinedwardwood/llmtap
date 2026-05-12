# llmtap remediation plan

Derived from the 2026-05-11 adversarial review. Ordered by blast radius, not by
ease. Phase 0 must land before any external user is told to point production
traffic at this proxy. Phases 1–3 can ship as point releases on top.

Each task has: **What** (the change), **Where** (the file or surface),
**Why** (the consequence of skipping), and **Done when** (the verifiable bar).

---

## Phase 0 — Stop the bleeding (v0.1.1)

These are the correctness defects that destroy user data, lie in dashboards,
or silently strip telemetry. They are blockers; nothing else in this plan
matters until they land.

### 0.1 Oversize request body no longer corrupts the upstream call

- **What:** Replace the read-into-memory-then-fallback dance in
  `readCappedBody` with a streaming policy. Two acceptable shapes:
  - **Preferred:** never buffer the full body when above the cap. Use
    `io.TeeReader(r.Body, &capturedHead)` with a hard ceiling of e.g. 1 MiB
    for *enrichment*, and forward the original `r.Body` byte-for-byte to the
    upstream. The proxy is allowed to give up on enrichment; it is **not**
    allowed to give up on the bytes.
  - **Acceptable fallback:** if the body is over a configurable hard limit
    (e.g. `request.max_body_bytes`, default 32 MiB), respond `413 Payload Too
    Large` directly to the client — never half-forward.
- **Where:** `internal/proxy/proxy.go` (`readCappedBody`, `ServeHTTP`).
  Add `Request.MaxBodyBytes` to `internal/config/config.go`. Update
  `config.example.yaml` and the README "Performance" table (the "4 MiB cap" claim).
- **Why:** Today, any chat-completions request > 4 MiB silently arrives at
  the upstream with zero bytes and the client gets `502`. Vision API,
  multi-modal, Whisper, and long-context Anthropic requests all break.
  This violates the README's "forwards every request unchanged" contract,
  which is the entire trust argument for the project.
- **Done when:** `TestProxyOversizeBodyForwardsIntact` passes (5 MiB chat
  request reaches the upstream byte-identical) AND a new
  `TestProxyHardCapRejectsCleanly` returns 413 with no upstream call.

### 0.2 Error response bodies forwarded intact

- **What:** Stop replacing `resp.Body` with a 64 KiB-capped snippet. Read
  into a side buffer for the `http.response.body_snippet` span attribute,
  but restore the **full original body** to the client. Either buffer the
  whole 4xx body (they're small) or use `io.TeeReader` to peel off the
  first 64 KiB while the rest flows through.
- **Where:** `internal/proxy/proxy.go` (`modify`, the `statusCode >= 400`
  branch).
- **Why:** Clients debugging upstream 4xx errors get a silently truncated
  error body and can't deserialize it. Same trust-contract violation as 0.1,
  smaller blast radius.
- **Done when:** New test `TestProxyForwardsLarge4xxIntact` sends a 200 KiB
  upstream error body and asserts the client receives every byte AND the
  span has a `body_snippet` of exactly 1024 bytes.

### 0.3 Streaming metrics keep their trace context

- **What:** Capture the request `ctx` into the streaming `onDone` closure
  and pass it to `finalize` instead of `context.Background()`. The closure
  in `responseInterceptor` already has access to it; thread it through.
- **Where:** `internal/proxy/proxy.go` — the `onDone` lambda at line ~298
  currently reads:
  ```go
  func() {
      h.activeStreams.Add(-1)
      finalize(context.Background())
  },
  ```
  Replace `context.Background()` with the per-request `ctx` captured in
  `ServeHTTP`.
- **Why:** Every streaming response — i.e. the dominant case — emits
  histograms (`gen_ai.client.operation.duration`,
  `gen_ai.client.time_to_first_token`, token usage, cost) with no trace
  context. The metric→trace exemplar jump in Grafana is broken precisely
  for the calls the operator most wants to investigate. Defeats the
  project's stated point.
- **Done when:** New test `TestStreamingMetricsCarryTraceContext` records
  the metric reader's last observation, extracts the exemplar's TraceID,
  and asserts it equals the span's TraceID.

### 0.4 Gzipped upstream responses parse correctly

- **What:** Two acceptable shapes:
  - Strip `Accept-Encoding` from the forwarded request in the `Rewrite`
    hook so the `http.Transport`'s built-in transparent decompression
    always engages. (Trade-off: slightly more bytes between us and the
    upstream.)
  - OR: inspect `resp.Header.Get("Content-Encoding")` in `modify`. If
    `gzip`, decompress into the side buffer used for parsing, leave
    `resp.Body` unchanged for the client.
- **Where:** `internal/proxy/proxy.go` — `Rewrite` and `modify`.
- **Why:** If a client explicitly sets `Accept-Encoding: gzip`,
  `httputil.ReverseProxy` does not auto-decompress and the JSON parser
  silently fails. Result: `$0` cost, `0` tokens recorded for every gzipped
  call. A FinOps disaster dressed up as a parser quirk.
- **Done when:** New test `TestProxyGzippedResponseStillCountsTokens`
  serves a gzipped JSON response from a fake upstream and asserts the
  recorded span carries `gen_ai.usage.input_tokens > 0`.

### 0.5 Pricing table is externalisable

- **What:** Add `pricing.path` and `pricing.fail_open` to config. When
  `pricing.path` is set, load YAML of shape:
  ```yaml
  openai:
    gpt-4o-mini: {input_usd_per_mtok: 0.15, output_usd_per_mtok: 0.60}
  anthropic:
    claude-3-5-sonnet: {input_usd_per_mtok: 3.0, output_usd_per_mtok: 15.0}
  ```
  The built-in table becomes the fallback (and gains a `Source` field so
  dashboards can show "list" vs "negotiated"). Embed via `//go:embed
  prices.yaml` rather than the current Go-literal map.
- **Where:** `internal/pricing/pricing.go`, `internal/pricing/prices.yaml`
  (new), `internal/config/config.go`, README.
- **Why:** README explicitly tells operators they can override per their
  negotiated rate. The code provides no mechanism. Every production
  cost number is currently the wrong number, and the project gaslights
  the user about it.
- **Done when:** Loading `pricing.path` overrides built-in prices, tests
  cover override + fallback + malformed file, and the README example
  works as written.

### 0.6 Map iteration nondeterminism in pricing lookup

- **What:** Sort prefixes by length-descending once at table construction
  and store as a slice of `(prefix, rate)` pairs. Lookup walks the slice
  and returns the first prefix match.
- **Where:** `internal/pricing/pricing.go` (`lookup`).
- **Why:** Today, equal-length prefixes (`gpt-4o-mini-fast` vs
  `gpt-4o-mini-cool`) produce nondeterministic cost depending on Go map
  iteration order. Latent correctness bomb — fires the first time someone
  adds a same-length sibling.
- **Done when:** A new test `TestPricingEqualLengthIsDeterministic`
  registers two equal-length prefixes and asserts repeatable lookup
  across 1000 iterations.

---

## Phase 1 — Trust contract (v0.2)

Privacy, security defaults, and operator basics. The bar is "I would let
this proxy a customer's production API keys."

### 1.1 Content redaction profiles

- **What:** Introduce `content.redact` config with three profiles:
  - `default` — strips `sk-[A-Za-z0-9_-]{20,}`, `xoxb-…`, AWS access keys
    (`AKIA[0-9A-Z]{16}`), GCP service account JSON markers, common JWT
    shape, RFC-5322 emails.
  - `strict` — `default` plus credit-card Luhn, US SSN, E.164 phone.
  - `off` — passthrough (current behaviour, must be explicit opt-in).
  Default value is `default` so the privacy story is not "off by
  accident". Apply before `span.AddEvent` and before the otelslog bridge
  emits content into logs.
- **Where:** `internal/redact/redact.go` (new package),
  `internal/provider/{openai,anthropic}.go` content-event emitters,
  `internal/config/config.go`.
- **Why:** With `content.mode: events` users absolutely will paste API
  keys into prompts, and they will hit the o11y backend in cleartext.
  "Pair it with redaction at your collector" is not a credible answer
  for a proxy whose pitch is "no SDK, no other system".
- **Done when:** Golden tests with sample payloads (curated, not real
  secrets) prove each profile redacts what it claims and nothing else.
  README "Privacy" section rewritten to point at the profiles.

### 1.2 Upstream host pinning

- **What:** Treat `Upstream.Target` as a *pinned* host. The
  `ReverseProxy.Transport` is constructed per-upstream with a
  `tls.Config{ServerName: target.Hostname()}` and (optionally) a
  pinned-cert allow-list per upstream (`pin_sha256: [<spki-hash>, …]`).
- **Where:** `internal/proxy/proxy.go` (`New`, where the `*ReverseProxy`
  is built per upstream).
- **Why:** Today, any operator-installed corporate-MITM root in the
  system trust store sees the bearer token. For an "audit boundary"
  proxy, that's theatre. Pinning makes llmtap a real boundary.
- **Done when:** Two new tests: (a) a swap to a forged cert chained to a
  different CA is rejected, (b) configured `pin_sha256` rejects a valid
  cert with a non-matching SPKI.

### 1.3 Health and readiness endpoints

- **What:** Reserve two paths on the listener that bypass the upstream
  router: `/healthz` (always 200 OK once `Run` returns from `Listen`)
  and `/readyz` (200 once OTLP exporters have completed their first
  export attempt successfully, else 503).
- **Where:** `internal/proxy/proxy.go` (`ServeHTTP` short-circuit before
  `cfg.Match`), `internal/telemetry/telemetry.go` (readiness signal).
  Document that no `upstream.prefix` may begin with `/healthz` or
  `/readyz`.
- **Why:** k8s, ALB, Cloudflare Tunnel, and any L7 LB need a probe
  target. Today there is none. Probe traffic gets attributed to the
  first-matching upstream prefix.
- **Done when:** `GET /healthz` returns 200 with no upstream traffic;
  `GET /readyz` returns 503 before OTLP comes up, 200 after; tests
  cover both.

### 1.4 Server shutdown waits on active streams

- **What:** `Server.Run` on `ctx.Done()` calls `h.WaitForStreams(shutCtx)`
  before `server.Shutdown`. The wait polls `h.activeStreams` (already
  present) with a small backoff and respects the shutdown deadline. If
  the deadline expires while streams are in-flight, log a `WARN` with
  the count and proceed.
- **Where:** `internal/proxy/server.go` (`Run`),
  `internal/proxy/proxy.go` (add `WaitForStreams`).
- **Why:** Today the `activeStreams` counter exists but nothing observes
  it. SIGTERM during a flight of agent calls kills the spans mid-flight.
  README claims "graceful shutdown"; it is not.
- **Done when:** Test `TestShutdownWaitsForActiveStreams` starts a slow
  fake stream, sends SIGTERM, asserts `Run` blocks until the stream
  finishes (within the deadline) before returning.

### 1.5 Per-upstream concurrency cap

- **What:** Add `upstreams[].max_in_flight` (default unlimited, or a
  conservative global of e.g. 256). Implement with
  `golang.org/x/sync/semaphore` keyed per upstream. When the semaphore is
  full, return `429 Too Many Requests` with `Retry-After: 1`.
- **Where:** `internal/proxy/proxy.go`, `internal/config/config.go`.
- **Why:** OpenAI brownouts will currently take the proxy down via
  goroutine + memory exhaustion. The cap is the fuse.
- **Done when:** A test that fires N+1 concurrent requests with cap=N
  observes exactly one 429.

### 1.6 SSE buffer is bounded

- **What:** In `sseTee.Read`, if `t.buf.Len()` exceeds a cap (e.g.
  `maxEventBytes = 1 MiB`), drop the buffered bytes, set a
  `parser_overflow` span event, and continue forwarding. Bytes still
  flow to the client unchanged — only parsing gives up. Reset the
  buffer to release memory.
- **Where:** `internal/provider/sse.go`.
- **Why:** Today an upstream that sends a single event with no
  terminating `\n\n` grows `bytes.Buffer` until OOM. Remote-DoS surface
  on every llmtap deployment.
- **Done when:** Fuzz-style test feeds a 10 MiB single-event payload,
  asserts memory stays bounded and forwarding completes.

---

## Phase 2 — Production hardness (v0.3)

### 2.1 Hot certificate reload

- **What:** Replace `srv.ServeTLS(ln, certFile, keyFile)` with a manual
  load path: build `tls.Config{GetCertificate: certManager.Get}`. The
  cert manager `os.Stat`s the cert file every N seconds (configurable,
  default 30s) and reloads when `ModTime` changes. Log loaded SHA-256
  of the leaf for audit.
- **Where:** `internal/proxy/server.go`, new `internal/proxy/certmgr.go`.
- **Why:** cert-manager is the default in 2026. Cert rotation requires
  process restart today, which means brief downtime every 90 days
  forever.
- **Done when:** Test writes cert v1, starts server, swaps to cert v2,
  asserts the next TLS handshake presents v2 within the reload window.

### 2.2 Circuit breaker per upstream

- **What:** Wrap each upstream's `Transport` in a circuit breaker that
  trips after N consecutive 5xx (default 10) within a window (default
  30s). Open state returns `503 Service Unavailable` immediately to the
  client; half-open admits a single probe.
- **Where:** `internal/proxy/proxy.go`, plus a tiny breaker package
  (consider `sony/gobreaker` if license compatible; otherwise hand-roll
  in <100 LOC).
- **Why:** Co-fuse with the concurrency cap. Without it, a degraded
  upstream consumes all in-flight slots and we melt anyway.
- **Done when:** Tests for the three transitions (closed→open,
  open→half-open, half-open→closed/open).

### 2.3 Cost metric as histogram

- **What:** Convert `gen_ai.client.cost.usd` from `Float64Counter` to
  `Float64Histogram` with USD-scaled buckets (1e-5, 1e-4, …, 100), and
  add `gen_ai.client.cost.usd.total` as a parallel monotonic
  observable counter for "total spend" dashboards.
- **Where:** `internal/telemetry/telemetry.go` (`newGenAIMeters`),
  `internal/proxy/proxy.go` (`recordMetrics`).
- **Why:** Float counters drift across millions of small adds; PromQL
  `rate()` can go negative at boundaries. Either accept the lie or fix
  it. Histograms also give p50/p95 cost per call, which is more useful
  than a running total anyway.
- **Done when:** Both instruments exist; the existing PromQL examples in
  the README are updated and verified against the demo stack.

### 2.4 Pin the OTel module set

- **What:** Move every `go.opentelemetry.io/otel/*` dependency to a
  single coherent release line. Today the API is `v1.32.0` and the SDK
  is `v1.31.0`; the log surface is `v0.7.0` / `v0.8.0`. Run `go get -u
  go.opentelemetry.io/otel/...@<chosen-version>` and re-vendor.
- **Where:** `go.mod`, `go.sum`.
- **Why:** Mixed-version OTel modules work today and fail on the next
  point release when a constructor signature shifts. Latent runtime
  failure mode.
- **Done when:** All `go.opentelemetry.io/otel/*` lines in `go.mod`
  point to release-aligned versions; CI passes on Go 1.24+.

### 2.5 Tool-call and multimodal streaming coverage

- **What:** Recognise `choices[].delta.tool_calls` in OpenAI streams as
  a content delta for TTFT measurement and for `gen_ai.choice` events.
  Same for Anthropic `content_block_delta` with `type:
  "input_json_delta"`. Multimodal request content (image/audio parts)
  is parsed as `content_type` rather than dropped.
- **Where:** `internal/provider/openai.go`, `internal/provider/anthropic.go`.
- **Why:** Agentic workloads stream tool calls, not text. Today TTFT is
  not recorded for tool-only streams and the assembled content event
  is empty. The README pitches "agents that fail in ways nobody can
  debug" as the problem we solve; we don't.
- **Done when:** New tests for both providers assert TTFT and finish
  reasons are recorded for tool-only streams.

---

## Phase 3 — Project hygiene (continuous)

### 3.1 Continuous integration

- **What:** `.github/workflows/ci.yml` running on every PR and on main:
  - `make test` with `-race -count=1 -timeout=2m`.
  - `make lint` with `golangci-lint` (the config already exists).
  - `govulncheck ./...` — fail on any HIGH or CRITICAL.
  - `trivy fs --severity HIGH,CRITICAL .` on the repo.
  - Build matrix: linux/{amd64,arm64} + darwin/arm64.
  - Coverage report uploaded as artifact; gate at 70% line coverage on
    the `internal/` tree (raise over time).
  Separate `.github/workflows/release.yml` triggers on tag:
  - Build and sign with `cosign` (keyless OIDC).
  - Generate SLSA-3 provenance via
    `slsa-framework/slsa-github-generator`.
  - Build and push Docker image to `ghcr.io` with attestations.
  - SBOM (CycloneDX) attached to the release.
- **Why:** The contribution guideline "run `make test lint`" is on the
  honor system. The README invites users to download unsigned binaries
  holding production credentials.
- **Done when:** PR checks are mandatory in branch protection; a tag
  produces a signed, attested release with an SBOM.

### 3.2 Test backfill

Drive each item in this list to a green test:

- [ ] Oversize request body forwarded intact (0.1).
- [ ] 4xx body > 64 KiB forwarded intact (0.2).
- [ ] Streaming metric carries trace context (0.3).
- [ ] Gzipped response tokens parsed (0.4).
- [ ] Pricing override loaded from file (0.5).
- [ ] Pricing equal-length prefix is deterministic (0.6).
- [ ] Redaction profile golden tests (1.1).
- [ ] Cert pinning rejects foreign CA (1.2).
- [ ] `/healthz`, `/readyz` semantics (1.3).
- [ ] Shutdown waits on streams (1.4).
- [ ] Concurrency cap returns 429 (1.5).
- [ ] SSE buffer bounded under attack (1.6).
- [ ] Cert hot reload (2.1).
- [ ] Circuit breaker transitions (2.2).
- [ ] Tool-call streaming TTFT (2.5).
- [ ] Anthropic streaming end-to-end (currently absent entirely).
- [ ] `Authorization` header survives the full proxy path (assertion).
- [ ] `captureContent: events` actually emits the events (assertion).

### 3.3 Disclosure surface

- **What:** Add `SECURITY.md` with disclosure mailbox + 90-day
  coordinated-disclosure window. Add `CODEOWNERS`. Add
  `.github/dependabot.yml` for `gomod` + `github-actions` weekly.
- **Why:** This project brokers third-party API credentials. A CVE is
  inevitable. Have the disclosure channel before, not after.

### 3.4 Documentation honesty pass

- **What:** Walk the README against the implementation and fix every
  load-bearing claim that is currently false or aspirational:
  - "It never alters bodies" — adjust to enumerate the explicit cases
    (snippet capture for 4xx is bounded, etc.).
  - "Memory: dominated by in-flight request bodies; capped at 4 MiB per
    request" — restate with the new policy from 0.1.
  - "Graceful shutdown. … No tests left dangling." — only after 1.4
    lands.
  - "Production deployments should override per their negotiated rate"
    — point at the actual config knob (0.5).
  - Roadmap "Built-in redaction profiles" — move to "Shipping in 0.2"
    (1.1).

### 3.5 Build info reaches `--version`

- **What:** Wire `runtime/debug.ReadBuildInfo` as a fallback when
  ldflags are not set (today, `go install` users see `dev / unknown /
  unknown`). The Makefile already injects via `-X`; add the same for
  `go install`-style consumers.
- **Where:** `internal/buildinfo/buildinfo.go`.

---

## Sequencing and ownership

- **Phase 0** is one focused PR per item, six PRs total. Estimated 3–5
  engineering days end-to-end. All six block any external "v0.1 GA"
  announcement.
- **Phase 1** is the v0.2 milestone. Estimated 2–3 weeks. The order
  inside the phase is flexible except that 1.3 (`/healthz`) and 1.4
  (shutdown waits) should land together — they're co-features of the
  same "operator can trust the lifecycle" story.
- **Phase 2** is the v0.3 milestone. The order inside is flexible;
  2.4 (OTel pin) is the cheapest and should land first to de-risk
  the rest.
- **Phase 3** runs in parallel from day one. 3.1 (CI) should land
  before any Phase 0 PR — it's the safety net for everything else.

## Non-goals for this plan

- Adding Bedrock / Gemini / Ollama parsers (v0.2+ roadmap, separate
  effort).
- `llmtap record` / `replay` / `diff` (v0.2+ roadmap).
- Cost-weighted sampling (v0.2+ roadmap).

These are good ideas. They are not on the path to "this proxy is safe
to put in front of production API keys", which is what this plan is
for.

---

# Addendum — Second adversarial review (2026-05-11, round 2)

A second pass surfaced additional issues that the first review missed.
The most serious is **0.7 — metric label cardinality**, which has been
promoted to "new FATAL FLAW" status: it is the failure mode most likely
to take down the *operator's* observability stack rather than llmtap
itself. Items are slotted into the existing phase scheme.

## Phase 0 additions — Stop the bleeding

### 0.7 Cap metric-label cardinality on model attributes  *(FATAL FLAW)*

- **What:** Three layered defenses, all required:
  1. **Normalize** the model string before it reaches any metric label.
     Strip provider-specific date/snapshot suffixes (`gpt-4o-2024-08-06`
     → `gpt-4o`; `claude-3-5-sonnet-20241022` → `claude-3-5-sonnet`).
     Reuse the longest-prefix logic already in `pricing.lookup`.
  2. **Allow-list** known model families via a built-in set
     (`internal/genai/models.go`, populated from the pricing table) plus
     an operator-configurable `metrics.allowed_models: [string]` in
     config. Anything not in the union maps to `_other`.
  3. **Hard cap** total observed distinct values per process with an
     LRU-style bounded set (`metrics.max_model_cardinality`, default
     200). On overflow, route the value to `_other` and emit a one-shot
     `llmtap.cardinality.overflow` event.
- **Where:** new `internal/labels/labels.go`; call sites in
  `internal/proxy/proxy.go` (`recordMetrics`) **only** — span attributes
  may keep the raw model string (span cardinality is bounded by
  retention). `internal/genai/genai.go` for the normalization table.
  `internal/config/config.go` for the two new knobs.
- **Why:** Today, every emitted metric carries
  `attribute.String(genai.AttrRequestModel, info.RequestModel)` with the
  client's verbatim model string. Any buggy or hostile client can mint
  unbounded series in Prometheus/Mimir. For a proxy whose pitch is
  "drop it in front of your LLM calls," the dominant operational risk
  is *llmtap takes down the o11y stack the operator already depends
  on*. This eclipses every other Phase 0 item.
- **Done when:**
  - `TestMetricsCardinalityCapped` fires 10,000 unique model strings
    through the proxy and asserts the recorded label set has
    `len ≤ max_model_cardinality + 1` (+1 for `_other`).
  - `TestMetricsNormalization` asserts that
    `gpt-4o-2024-08-06`, `gpt-4o-2024-11-20`, and `gpt-4o-mini-2024-07-18`
    map to `gpt-4o`, `gpt-4o`, `gpt-4o-mini`.
  - README "Performance" gains a "Cardinality" subsection explaining
    the caps.

### 0.8 `--log-level` flag actually changes log level

- **What:** Replace `slog.SetLogLoggerLevel(lvl)` with a `slog.LevelVar`
  threaded into both the otelslog handler and a fallback
  `slog.NewTextHandler` for stderr. Resolve the level *before*
  constructing the logger, not after `slog.SetDefault`. Delete the
  comment block that rationalizes the current no-op.
- **Where:** `cmd/llmtap/main.go` (`runUp`, `setLevel`).
- **Why:** Today, `--log-level=debug`, `--log-level=warn`, and
  `--log-level=error` produce identical output. The handler ignores
  `SetLogLoggerLevel` (that function only governs `log` → `slog`
  bridging). In an incident, operators dialing the level down to debug
  will see nothing new and waste valuable time.
- **Done when:** `TestRunUpLogLevelHonored` captures stderr from
  `runUp(["up", "--log-level=error"], …)` against a config with an
  intentional info-level log site, asserts the info line does not
  appear, and asserts the same site with `--log-level=info` does.

### 0.9 Error-body snippet respects `content.mode=off`

- **What:** Gate `attribute.String("http.response.body_snippet", …)` on
  `captureContent || cfg.Content.Mode != "off"`. When content capture
  is off, attach only `http.response.body_size` and a synthetic
  `error_class` derived from `resp.Header.Get("WWW-Authenticate")` /
  the upstream's machine-readable error code field (parsed from the
  body in a side buffer, never attached as content).
- **Where:** `internal/proxy/proxy.go` (`modify`, the `statusCode >= 400`
  branch).
- **Why:** Today, every 4xx/5xx attaches the first 1024 UTF-8-clean
  bytes of the upstream error response as a span attribute, regardless
  of `content.mode`. OpenAI's auth-failure responses include a prefix
  of the offending API key (`"Incorrect API key provided: sk-proj-…"`).
  This silently violates the project's flagship privacy default. The
  remedy is independent of 1.1 (redaction profiles) because operators
  set `content.mode=off` precisely to avoid having to trust a
  redactor.
- **Done when:** `TestErrorBodySnippetSuppressedWhenContentOff` sends a
  401 from a fake upstream with a body containing `sk-test-LEAKED` and
  asserts the recorded span has no attribute whose value contains
  `sk-test-LEAKED`.

## Phase 1 additions — Trust contract

### 1.7 Reject `telemetry.insecure: true` with non-loopback endpoint

- **What:** Extend `Config.Validate` to refuse startup when
  `Telemetry.Insecure` is true AND `Telemetry.Endpoint` does not
  resolve to a loopback or RFC1918 host. Match the existing
  `listen`-side guardrail in spirit and error-message style. Provide
  an explicit override (`telemetry.acknowledge_insecure: true`) for
  the rare legitimate case (sidecar collector on a unix-socket-style
  bridge).
- **Where:** `internal/config/config.go` (`Validate`, plus a small
  `isLocalEndpoint` helper that splits host:port and runs the same
  loopback/private-IP checks).
- **Why:** Default config has `insecure: true` and
  `endpoint: localhost:4317`. The first user who edits *only* the
  endpoint (`LLMTAP_OTLP_ENDPOINT=otel.grafana.net:443`) ships every
  trace — including any captured prompts — in cleartext over the WAN.
  Symmetric with the existing protection on `listen`.
- **Done when:** `TestValidateRejectsInsecureWAN` asserts a config with
  `endpoint: otel.example.com:4317` + `insecure: true` returns an
  error; the same with `acknowledge_insecure: true` does not.

### 1.8 `LLMTAP_OTLP_HEADERS` environment override

- **What:** Add env support for `Telemetry.Headers`. Parse a
  comma-separated `k=v` list (`Authorization=Bearer abc,X-Scope-OrgID=42`),
  trimming whitespace. Env takes precedence over YAML, consistent with
  the rest of `applyEnv`. Document the parsing rules in the example
  config.
- **Where:** `internal/config/config.go` (`applyEnv`).
- **Why:** Today the OTLP backend's bearer token can only live in YAML
  on disk. Kubernetes secret → env var → process is the standard
  delivery pattern and there is no escape hatch. Forces operators to
  ship config files for what should be a `valueFrom: secretKeyRef`.
- **Done when:** `TestEnvOverridesHeaders` sets
  `LLMTAP_OTLP_HEADERS="Authorization=Bearer abc,X-Tenant=42"` and
  asserts `cfg.Telemetry.Headers["Authorization"] == "Bearer abc"`
  and `cfg.Telemetry.Headers["X-Tenant"] == "42"`.

### 1.9 `LLMTAP_SAMPLE_RATIO` environment override

- **What:** Add env support for `Telemetry.SampleRatio`. Parse as
  `float64` via `strconv.ParseFloat`, clamp to `[0,1]` with a warning
  to stderr if out of range (do not refuse to start — sampling is not
  a correctness knob).
- **Where:** `internal/config/config.go` (`applyEnv`).
- **Why:** Inconsistent with the rest of `applyEnv` (most scalars are
  env-overridable, this one isn't). Operators tuning sample ratio at
  deploy time today must regenerate the config file.
- **Done when:** Trivial test asserts env override sets the field.

### 1.10 Body-upload deadline (slowloris mitigation)

- **What:** Set `Server.ReadTimeout` AND keep `WriteTimeout=0` for
  streaming. Alternatively (preferred): wrap `r.Body` in an
  `http.MaxBytesReader` + a `context.WithTimeout` deadline on the
  body read itself, so the request goroutine cannot be held open
  indefinitely by a slow uploader. Cap default: 30s body read,
  configurable via `http.body_read_timeout`.
- **Where:** `internal/proxy/server.go`, `internal/proxy/proxy.go`
  (`readCappedBody`).
- **Why:** Today, `WriteTimeout: 0` is intentional for streaming
  responses, but there is no symmetric body-read deadline. A slow
  uploader can hold a connection open indefinitely. Cheap remote-DoS
  surface against a single-binary proxy.
- **Done when:** `TestSlowUploadIsTerminated` sends a body at 1 byte
  per 100ms and asserts the proxy gives up within
  `body_read_timeout + epsilon`.

### 1.11 SSE parser handles `\r\n\r\n` and `\r\r` framing

- **What:** Replace `bytes.Index(raw, []byte("\n\n"))` with a small
  scanner that recognizes any of `\n\n`, `\r\n\r\n`, `\r\r` as a
  message terminator. Keep the existing per-line `TrimRight(line, "\r")`
  normalization.
- **Where:** `internal/provider/sse.go` (`drain`).
- **Why:** The SSE spec permits all three frame separators. OpenAI and
  Anthropic ship `\n\n` today; if either switches to CRLF framing,
  llmtap silently degrades to "buffer the whole stream, then flush
  one giant event on EOF". TTFT goes wrong, per-chunk usage is lost,
  and the failure is invisible until someone notices the histograms
  are flat.
- **Done when:** Three new sub-tests in `TestSSETeeForwardsAndParses`
  feed `\r\n\r\n`-framed, `\r\r`-framed, and mixed-framing payloads
  and assert each event is dispatched at the correct boundary.

### 1.12 Strict operation-path matching

- **What:** Replace `strings.HasSuffix(urlPath, "/chat/completions")`
  with a path-segment match (`pathPrefix + "/chat/completions"` exact,
  or a tiny `path.Clean` + split + last-segments comparison). Same
  for `/embeddings` and Anthropic `/messages`.
- **Where:** `internal/provider/openai.go` (`OperationFor`),
  `internal/provider/anthropic.go` (`OperationFor`).
- **Why:** Today a request to e.g. `/v1/files/chat/completions`
  matches `OpChat` and gets the chat-completions parser pointed at
  whatever JSON shape that endpoint returns. Garbage-attribute or
  parse-fail; either way wrong. Defensive and cheap.
- **Done when:** `TestOperationForRejectsImpostorPaths` asserts each
  provider's `OperationFor` returns `""` for paths like
  `/v1/files/chat/completions` and `/v1/anthropic/.well-known/messages`.

### 1.13 Client authentication at the proxy boundary

- **What:** Optional bearer-token allow-list on the listener:
  ```yaml
  auth:
    tokens:
      - "$argon2id$v=19$..."   # hashed; CLI helper to generate
    header: "Authorization"    # default
  ```
  When `auth.tokens` is non-empty, every request must present a
  matching token (after the standard `Bearer ` prefix). On mismatch,
  return `401` with no upstream call. Tokens are compared in constant
  time against the stored hashes.
- **Where:** new `internal/auth/auth.go`; wiring in
  `internal/proxy/proxy.go` (`ServeHTTP`, before `cfg.Match`);
  `internal/config/config.go`.
- **Why:** Today llmtap forwards `Authorization: Bearer sk-…`
  byte-for-byte (advertised feature) but performs *no* check on the
  caller. mTLS (1.2 + the existing `ClientCAFile`) is the strong
  story, but issuing client certs is heavy. A token allow-list is the
  cheap defense-in-depth layer that turns the demo compose stack from
  an open relay into a private tool.
- **Done when:** `TestAuthRequiredWhenTokensConfigured` asserts a
  missing/wrong token gets a 401 with no upstream traffic, and a
  correct one gets the normal proxy path.

## Phase 2 additions — Production hardness

### 2.6 Tune `httputil.ReverseProxy.Transport` per upstream

- **What:** Replace the implicit `http.DefaultTransport` with an
  explicit per-upstream transport:
  ```go
  &http.Transport{
      MaxIdleConns:          256,
      MaxIdleConnsPerHost:   64,
      IdleConnTimeout:       90 * time.Second,
      TLSHandshakeTimeout:   10 * time.Second,
      ResponseHeaderTimeout: 60 * time.Second,
      ExpectContinueTimeout: 1 * time.Second,
      ForceAttemptHTTP2:     true,
  }
  ```
  Expose `MaxIdleConnsPerHost` and `ResponseHeaderTimeout` as per-upstream
  config overrides for the brave.
- **Where:** `internal/proxy/proxy.go` (`New`),
  `internal/config/config.go`.
- **Why:** `http.DefaultTransport` has `MaxIdleConnsPerHost: 2`. Under
  any real concurrency to `api.openai.com`, connections serialize
  through 2 sockets, latency p99 explodes, and the proxy's "zero
  overhead" pitch is a lie. Invisible bottleneck.
- **Done when:** A load test (k6 or vegeta, see 3.6) sustains 200 RPS
  against a local httpbin-style fake upstream with p99 added latency
  under 5ms.

### 2.7 Stream non-streaming JSON responses through

- **What:** Replace the `io.ReadAll(resp.Body)` + re-wrap pattern with
  `io.TeeReader(resp.Body, &captureBuf)` for non-streaming responses.
  Bytes flow to the client as they arrive; the parser runs on the
  captured copy when the body close fires. Cap `captureBuf` at e.g.
  4 MiB (configurable, matches request cap); above that, give up on
  enrichment and continue tee-ing.
- **Where:** `internal/proxy/proxy.go` (`modify`, the non-streaming
  branch).
- **Why:** Today, the entire upstream JSON is read into memory before
  the first byte reaches the client. README claims "~1-2 ms overhead
  added to non-streaming calls". For an embeddings response with 8K
  inputs (megabytes), the real overhead is "first byte delayed by
  the full upstream response time". p99 lies.
- **Done when:** `TestNonStreamingFirstByteIsNotDelayed` measures TTFB
  to client against TTFB from upstream and asserts the delta is under
  10ms even for a 5 MiB JSON response.

### 2.8 Pre-check `Content-Length` before reading

- **What:** In `readCappedBody`, if `r.ContentLength > 0 &&
  r.ContentLength > cfg.Request.MaxBodyBytes`, return 413 immediately
  without reading the body. Falls through to the existing
  read-with-cap path otherwise (covers chunked transfer where
  Content-Length is -1).
- **Where:** `internal/proxy/proxy.go` (`readCappedBody`,
  `ServeHTTP`).
- **Why:** Today a 1 GiB body with `Content-Length: 1073741824` is
  read up to 4 MiB+1 before being rejected. Trivial defense against
  trivial DoS that 0.1 doesn't fully cover (0.1 makes the read
  succeed, but reading still costs CPU and bandwidth).
- **Done when:** `TestContentLengthPreCheck` sends a request with
  `Content-Length: 999999999` and a tiny body, asserts the response
  is 413 and that the upstream fake records zero bytes read.

### 2.9 Pricing trie for O(K) lookup

- **What:** Replace the linear-scan `lookup` with a per-system prefix
  trie built once at pricing-table load. Lookup is O(len(model))
  instead of O(N · avg_prefix_len).
- **Where:** `internal/pricing/pricing.go`.
- **Why:** N=12 today, negligible. With the externalized pricing
  table from 0.5 and the cardinality cap from 0.7 (which steers
  unknowns to `_other`), realistic deployments will register
  100–300 model prefixes. Linear scan becomes a measurable hot path.
- **Done when:** Microbenchmark `BenchmarkPricingLookup` shows the
  per-op cost stays within 200ns at N=500 prefixes.

## Phase 3 additions — Project hygiene

### 3.6 Load test harness in CI

- **What:** Add `test/load/` with k6 or vegeta scripts hitting a
  fake-upstream httptest server through llmtap at 200 RPS for 60s.
  Assert p99 added latency under 5ms, zero 5xx, zero goroutine leak.
  Run on every PR via `make load-test` and in CI.
- **Where:** `test/load/`, `Makefile`, `.github/workflows/ci.yml`
  (after 3.1 lands).
- **Why:** Every performance claim in the README ("~1-2 ms",
  "~0 added latency for streaming") is currently vibes. The
  cardinality cap (0.7) and transport tuning (2.6) both need
  empirical evidence to dial in defaults. Cheap once, valuable
  forever.
- **Done when:** `make load-test` passes locally and in CI; results
  surfaced as a PR comment via GitHub Actions output.

### 3.7 Demo compose stack hardening

- **What:** In `deploy/compose/docker-compose.yml`, replace
  `LLMTAP_ALLOW_INSECURE: "true"` with a generated self-signed cert
  baked into a sidecar volume, and switch the listener to TLS on
  `:4443`. Provide a `make compose-up-insecure` escape hatch for
  users who explicitly want plaintext loopback.
- **Where:** `deploy/compose/docker-compose.yml`, `Makefile`, README
  Quick Start section.
- **Why:** The current demo trains users that `allow_insecure: true`
  is the way to make llmtap work. Copy-paste from demo to prod is
  guaranteed; we should not be teaching the wrong thing in the first
  example operators ever run.
- **Done when:** `make compose-up` brings up the stack on `https://
  localhost:4443` with a self-signed cert; the Quick Start in the
  README walks through the cert-trust step.

### 3.8 Test backfill — addendum items

Extends the checklist in 3.2:

- [ ] Cardinality cap holds under 10k unique models (0.7).
- [ ] Model normalization maps snapshots to families (0.7).
- [ ] `--log-level` actually changes verbosity (0.8).
- [ ] Error snippet suppressed when content.mode=off (0.9).
- [ ] Validation refuses insecure+WAN OTLP (1.7).
- [ ] Env override for OTLP headers (1.8).
- [ ] Env override for sample ratio (1.9).
- [ ] Slow uploader is terminated within deadline (1.10).
- [ ] SSE parser handles `\r\n\r\n` and `\r\r` (1.11).
- [ ] `OperationFor` rejects impostor paths (1.12).
- [ ] Bearer-token allow-list rejects unauthorized clients (1.13).
- [ ] Non-streaming TTFB delta under 10ms (2.7).
- [ ] Content-Length pre-check returns 413 without reading (2.8).
- [ ] Pricing lookup stays under 200ns at N=500 (2.9).

## Revised sequencing

- **0.7 jumps to the front of Phase 0.** It is the dominant
  operational risk; everything else in Phase 0 is correctness for a
  single request, this one is correctness for the operator's o11y
  stack.
- 0.8 and 0.9 are small follow-ups inside Phase 0; they should land
  in the same release.
- **Phase 1 grows by ~7 items** but they're individually small. The
  v0.2 milestone slips by perhaps a week; the trust story improves
  considerably.
- **Phase 2 grows by 3 items**, all of which are unblocked by the
  rest of Phase 0/1. They can be parallelized.
- 3.6 (load test) should land alongside 0.7 — the cardinality cap
  needs empirical tuning, and without a load harness the defaults
  are guesses.

---

# Security Remediations A1–A29  (2026-05-11)

Triage block per REMEDIATION.md. Severity assigned to every item in this
plan so the fix queue has a defined order. Critical/High fix
immediately; Medium logged for the next cycle; Low fixed opportunistically.

Legend: 🔴 Critical/High · 🟡 Medium · 🟢 Low

## Critical (token exposure / operator infrastructure DoS) — fix first

- [x] **A1 — Metric label cardinality cap (item 0.7)** 🔴
  **Finding:** `gen_ai.request.model` / `gen_ai.response.model` flow
  verbatim from client JSON into metric labels. Hostile or buggy
  clients mint unbounded Prometheus/Mimir series → operator o11y stack
  OOM.
  **Fix:** New `internal/labels` package: lowercase + strip date /
  version snapshot suffixes; admission via `sync.Map` with an atomic
  counter capped at `DefaultMaxCardinality = 200`; overflow routed to
  `_other`. Applied at every metric attribute site in
  `proxy.recordMetrics`. Span attributes keep raw model strings.
  **Evidence:** `TestProxyMetricsModelCardinalityIsBounded` fires
  1000 distinct synthetic model names through the proxy and asserts
  the recorded label set ≤ 201 (cap + `_other`) and includes
  `_other`. Unit tests cover normalization, lowercasing, empty input,
  cap engagement, and `-race` concurrency safety.

- [x] **A2 — Error body snippet leaks API key prefix (item 0.9)** 🔴
  **Finding:** On every 4xx/5xx, `http.response.body_snippet` (first
  1024 UTF-8 bytes) is attached to the span regardless of
  `content.mode: off`. OpenAI auth failures echo the offending API
  key prefix. Token exposure in traces.
  **Fix:** Gate `body_snippet` attachment on `captureContent`. Attach
  `http.response.body_size` (an integer, never content) unconditionally
  so operators still see that an error body was observed.
  **Root cause uncovered during this fix:** the `modify` callback in
  `proxy.responseInterceptor` was *never being invoked at all* for any
  response. The ReverseProxy's `ModifyResponse` hook retrieves the
  function from the request context via `.(modifyFn)`, but the value
  was stored with the unnamed type `func(*http.Response) error`. Go
  type assertions distinguish named types from their underlying
  unnamed types, so the assertion silently failed on every request
  since project inception. This means every existing response-side
  enrichment (`http.response.status_code`, span status, streaming
  TTFT, output token usage, finish reasons, cost, the body snippet
  itself) has been silently absent in production. The pre-existing
  `TestProxyEndToEndStreaming` only asserted `value != 2` rather than
  `value == 2`, vacuously passing despite the silent break.
  **Fix (root cause):** Changed `responseInterceptor`'s return type
  from `func(*http.Response) error` to the named `modifyFn`. The
  context assertion now succeeds; `modify` runs on every response.
  **Evidence:**
  - `TestErrorBodySnippetSuppressedWhenContentOff` — leaked secret
    not present on any span attribute when `content.mode=off`, while
    the client still receives the upstream body verbatim.
  - `TestErrorBodySnippetAttachedWhenContentEvents` — snippet IS
    attached when the operator opts into events; proves the path
    works when intended.
  - `TestErrorBodySizeAttachedAlways` — byte-count metadata flows
    independent of content mode.
  - Span status now correctly reports `Error HTTP 400` and
    `http.response.status_code = 400` (previously 0).

## High (data loss / DoS / auth boundary / cost misreporting)

- [x] **A3 — Oversize body silent 502 (item 0.1)** 🔴
  **Finding:** Requests > 4 MiB read fully, then rejected with body
  already closed → upstream gets zero bytes, client gets 502.
  Documented as a failing test in the suite. Violates "forwards every
  request unchanged" contract.
  **Fix:** Split the buffering concept in two: a fixed 1 MiB
  *enrichment buffer* (`maxEnrichmentBodyBytes`) that the parser
  reads from, and a configurable *hard cap* (`config.Request.MaxBodyBytes`,
  default 32 MiB) that gates whether we forward at all. Bodies up to
  the hard cap are forwarded byte-for-byte via an `io.MultiReader` of
  `[captured head + remaining stream]`; the enrichment slice is
  truncated at the buffer boundary so `ParseRequest` may degrade but
  the request reaches the upstream intact. Bodies above the hard cap
  are refused with HTTP 413 — either by `Content-Length` pre-check or
  by a `capCountingReader` that aborts mid-stream once the cumulative
  count surpasses the cap. `config.example.yaml` documents the new
  `request:` section.
  **Evidence:**
  - `TestProxyOversizeBodyForwardsIntact` (renamed from
    `TestProxyOversizeBodyIsCorrupted`): a 5 MiB chat-completions body
    arrives at the upstream byte-identical to what the client sent.
  - `TestProxyHardCapRejectsCleanly`: a 3 MiB body against a 2 MiB
    hard cap returns 413 with zero upstream traffic.
  - Full suite is now uniformly green for the first time in the
    project's history — the previously-documented bug-marker failure
    is gone.

- [x] **A4 — 4xx body truncated to client (item 0.2)** 🔴
  **Finding:** Error responses are replaced with a 64 KiB snippet
  buffer; clients debugging upstream errors get a truncated body they
  cannot deserialize.
  **Fix:** New `snippetCapture` type implements `io.Writer` and is
  hooked into `resp.Body` via `io.TeeReader`. The body itself flows
  through the tee untouched to the client; the side capture records
  the first 1024 bytes (for `http.response.body_snippet`) and the
  total size (for `http.response.body_size`). Span attributes are
  attached in `finishSpan` after the tee has drained.
  **Evidence:**
  - `TestProxyForwardsLarge4xxIntact`: upstream sends 200 KiB error
    body; client receives all 200 KiB; span's `body_size` is 204800
    and `body_snippet` is the bounded prefix.
  - Existing A2 tests (`TestErrorBodySnippetSuppressedWhenContentOff`,
    `TestErrorBodySnippetAttachedWhenContentEvents`,
    `TestErrorBodySizeAttachedAlways`) all stay green.

- [x] **A5 — Streaming metrics lose trace context (item 0.3)** 🔴
  **Finding:** Streaming `finalize` is invoked with
  `context.Background()`, so every streaming-emitted metric has no
  exemplar link back to its span. Defeats the project's main pitch.
  **Fix:** Captured `streamCtx := context.WithoutCancel(resp.Request.Context())`
  inside `modify` and passed it to `finalize` from the WrapStream
  onClose callback. `WithoutCancel` preserves the trace context while
  detaching from request-side cancellation, so the metric emission
  still fires after the client disconnects.
  **Evidence:** `TestStreamingMetricsCarryTraceContext` records a
  streaming chat, pulls the duration histogram exemplar from the
  ManualReader, and asserts its `TraceID` equals the span's `TraceID`.
  Before the fix, no exemplar carried a trace context at all.

- [x] **A6 — Gzipped responses record zero tokens (item 0.4)** 🔴
  **Finding:** When clients set `Accept-Encoding: gzip`, the JSON
  parser sees gzip bytes and fails silently. FinOps disaster —
  recorded cost = $0 for gzipped calls.
  **Fix:** One-line addition to the `Rewrite` hook:
  `r.Out.Header.Del("Accept-Encoding")`. With the client's header
  stripped, `http.Transport` injects its own and engages transparent
  decompression. `modify` always sees plaintext JSON. The trade-off
  (a few more bytes between llmtap and the upstream) is paid by
  llmtap, not the client.
  **Evidence:**
  - `TestProxyGzippedResponseStillCountsTokens`: client sends
    `Accept-Encoding: gzip`, upstream serves gzipped JSON, span
    records `gen_ai.usage.input_tokens > 0` (was 0 before fix).
  - `TestProxyStripsAcceptEncodingFromOutbound`: outbound request
    seen by upstream does not carry the client's `Accept-Encoding`
    verbatim.

- [x] **A7 — Pricing table cannot be overridden (item 0.5)** 🔴
  **Finding:** README claims operators can override negotiated rates;
  no config knob exists. Every production cost number is the wrong
  number.
  **Fix:** Moved the in-Go `tables` map to `internal/pricing/prices.yaml`
  and embedded via `//go:embed`. New `pricing.Table` type with
  `Default()` + `Load(path, failOpen)` factories: the override file is
  merged on top of the built-in catalogue per (system, model-prefix)
  key, so unspecified models retain the default rate. Added
  `config.Pricing{Path, FailOpen}` and threaded a per-Handler
  `*pricing.Table` through `proxy.New`; the recordMetrics call site
  uses `h.pricing.Cost(...)` instead of the package-level function.
  **Evidence:**
  - Unit tests cover the seven scenarios: default loads, override
    replaces, override falls back per-key, fail-closed errors on
    missing/malformed, fail-open silently degrades, empty path
    returns default.
  - `TestProxyEmitsOverriddenCost` (proxy integration): a config with
    `Pricing.Path` pointing at a YAML setting `gpt-4o-mini` to
    0.99 USD/Mtok records `gen_ai.cost.usd = 0.99` on the span,
    not the built-in 0.15.
  - `TestProxyNewFailsOnMissingPricingFileFailClosed`: `proxy.New`
    refuses to construct when `pricing.path` is misconfigured and
    `fail_open=false`.
  - `config.example.yaml` documents the new `pricing:` section.

- [x] **A8 — `--log-level` is a no-op (item 0.8)** 🔴
  **Finding:** `slog.SetLogLoggerLevel` does not affect the otelslog
  handler; all log levels produce identical output.
  **Fix:** Extracted `newLogger(level, name, version, stderr)` from
  `runUp`. Parses the level into a `slog.LevelVar`, builds a leveled
  `slog.TextHandler` against stderr and a `leveledHandler` wrapping
  the otelslog bridge (which has no native level option in the pinned
  SDK), then combines both via a `multiHandler` so records dispatch
  to both sinks under the same level. Deleted the rationalizing
  `setLevel` no-op and its misleading comment.
  **Evidence:** Five tests in `cmd/llmtap/logger_test.go`:
  - `TestNewLoggerRespectsLevelInfo` — debug records filtered, info
    records pass.
  - `TestNewLoggerRespectsLevelError` — info+warn filtered, error
    passes.
  - `TestNewLoggerInvalidLevel` — unknown level returns an error.
  - `TestNewLoggerEmptyLevelDefaultsToInfo` — empty string maps to
    info, matching the original behaviour.
  - `TestNewLoggerAttachesServiceAttrs` — `service.name` and
    `service.version` present on every record.

- [x] **A9 — Insecure OTLP + WAN endpoint silently leaks (item 1.7)** 🔴
  **Finding:** `Default.Telemetry.Insecure: true` paired with an
  edited-only `Endpoint` ships every trace (incl. captured content)
  in cleartext over the WAN. No validation guardrail.
  **Fix:** Added `Telemetry.AcknowledgeInsecure` config field +
  `isLocalEndpoint` helper that recognizes loopback IPs, `localhost`,
  and RFC1918 / RFC4193 private space (`net.IP.IsLoopback()` and
  `net.IP.IsPrivate()`). `Config.Validate` now refuses startup when
  `telemetry.insecure=true` and the endpoint is non-local, unless
  `acknowledge_insecure=true` is set explicitly. The error message
  mirrors the existing `listen`-side guardrail in style.
  **Evidence:**
  - `TestValidateRejectsInsecureWAN`: `endpoint: otel.example.com:4317`
    + `insecure: true` returns an error.
  - `TestValidateAcknowledgeInsecureBypassesGuard`: same config plus
    `acknowledge_insecure: true` is accepted.
  - `TestValidateInsecureLocalEndpoints`: loopback (`127.0.0.1`,
    `[::1]`, `localhost`) and RFC1918 (`10.0.0.5`, `172.16.5.5`,
    `192.168.1.5`) endpoints pass without acknowledgement. Scheme
    prefixes (`http://`, `https://`) handled.
  - `TestValidateSecureWANOK`: WAN endpoint with `insecure: false`
    passes — guard is scoped to the cleartext combination.
  - Existing `TestLoadYAMLOverrides` / `TestEnvOverrides` updated to
    use legitimate values (`acknowledge_insecure: true` or
    `insecure: false`) against their non-local endpoints.

- [x] **A10 — No client authentication (item 1.13)** 🔴
  **Finding:** llmtap forwards `Authorization: Bearer sk-…` verbatim
  with no caller check. Demo compose stack ships open-relay default.
  **Fix:** New `internal/auth` package implements argon2id PHC
  hashing (`Hash`, `Verify`) and a `Verifier` that parses an operator
  allow-list up-front and constant-time compares incoming bearer
  tokens at request time. Added `config.Auth{Tokens, Header}` with
  default header `X-LLMTAP-Token` (kept distinct from `Authorization`
  so the upstream LLM API key isn't double-claimed). Handler stores
  a `*auth.Verifier`; `ServeHTTP` short-circuits with 401 + zero
  upstream traffic when a request arrives without a matching token.
  The auth header is stripped from the inbound request before
  forwarding, so the listener-side secret never leaks to the
  upstream. New `llmtap hash-token` CLI reads plaintext from stdin
  (keeps secrets out of argv / shell history) and prints the PHC
  hash.
  **Evidence (unit):** ten tests in `internal/auth/auth_test.go`
  covering Hash↔Verify roundtrip, salt randomization, wrong-plaintext
  rejection, PHC parser refusal on malformed input, Verifier
  acceptance of any-of-N tokens, empty-token rejection, `Enabled()`
  semantics, fail-fast at construction.
  **Evidence (integration):** six tests in `internal/proxy/auth_test.go`
  covering missing-token → 401, wrong-token → 401, correct-token
  → 200 + upstream hit, bare and `Bearer …` forms both accepted,
  tokens-empty path stays untouched, custom-header configuration.
  Defensive assertion: upstream fake fails the test if the auth
  header reaches it.
  **Evidence (CLI):** `cmd/llmtap/hashtoken_test.go` pipes plaintext
  into `runHashToken`, parses the printed hash, and round-trips it
  through `auth.Verify`.

- [x] **A11 — Slowloris body upload (item 1.10)** 🔴
  **Finding:** No body-read deadline; slow uploaders pin a
  goroutine + connection indefinitely. Cheap remote-DoS.
  **Fix:** Added `HTTPTimeouts.BodyReadTimeout` (default 30s) and set
  it as `http.Server.ReadTimeout` in `proxy.NewServer`. The PLAN's
  preferred in-handler watchdog (close `r.Body` from a goroutine
  when a deadline expires) turned out to be unusable: Go's
  `(*http.body).Close()` synchronously drains the remaining bytes
  using the same slow source, so closing the body doesn't actually
  unblock the in-flight read. `Server.ReadTimeout` works at the
  TCP-conn level — the connection is torn down mid-read — and is
  the only correctness-preserving choice in the current stdlib.
  Trade-off: clients see a connection-closed transport error rather
  than a graceful 408; the goal of A11 (no pinned goroutine) is
  preserved.
  **Evidence:**
  - `TestSlowUploadIsTerminated`: a 5 KB body delivered at 50 ms/byte
    (~250 s total) against a 300 ms deadline is aborted within
    2 s and zero upstream traffic is observed.
  - `TestNoBodyReadTimeoutWhenUnset`: `BodyReadTimeout=0` keeps the
    pre-A11 behaviour — slow uploads complete and reach upstream.
  - Tests wire through `httptest.NewUnstartedServer` so the
    `ReadTimeout` knob (the A11 surface) is actually exercised.

- [x] **A12 — Per-upstream concurrency cap (item 1.5)** 🔴
  **Finding:** No bound on in-flight requests per upstream; an
  upstream brownout drains all goroutines + memory.
  **Fix:** Added `Upstream.MaxInFlight int` config field (0 =
  unlimited). At construction time `proxy.New` builds a buffered-chan
  semaphore per upstream sized to the cap. In `ServeHTTP` (after
  upstream match, before any body read or span/metric work) the
  proxy non-blocking-acquires; over-cap responses get 429 with
  `Retry-After: 1` and never reach the upstream. Buffered chan
  keeps the dep footprint at zero.
  **Evidence:**
  - `TestUpstreamConcurrencyCapReturns429`: with `cap=3`, four
    concurrent requests against a blocked upstream produce exactly
    3 × 200 + 1 × 429; the 429 carries `Retry-After: 1`; upstream
    sees exactly 3 hits.
  - `TestUpstreamConcurrencyUnboundedByDefault`: `MaxInFlight=0`
    lets 20 concurrent requests through without limit.

- [ ] **A13 — Unbounded SSE buffer (item 1.6)** 🔴
  **Finding:** `sseTee.buf` grows without limit; an upstream sending
  a single event without `\n\n` terminator OOMs the process.
  **Fix:** Hard cap; drop parser state on overflow but keep
  forwarding bytes.

- [ ] **A14 — Demo compose teaches `allow_insecure=true` (item 3.7)** 🔴
  **Finding:** First example operators ever run sets the insecure
  bypass. Copy-paste from demo to prod = open relay.
  **Fix:** Demo stack uses TLS with a self-signed cert by default;
  insecure mode behind a separate `make` target.

- [ ] **A15 — No CI / unsigned releases (item 3.1)** 🔴
  **Finding:** Every protection in this plan is on the honor system
  without enforced CI. README invites users to download unsigned
  binaries handling production credentials.
  **Fix:** PR-gating CI (test/lint/govulncheck/trivy) + signed
  release pipeline (cosign + SLSA + SBOM).

- [ ] **A16 — Content redaction profiles (item 1.1)** 🔴
  **Finding:** `content.mode: events` ships unredacted prompts. The
  "pair with collector redaction" workaround is not a credible
  privacy story for the proxy's target audience.
  **Fix:** Built-in `default`/`strict`/`off` redaction profiles,
  applied at the proxy before any export.

## Medium — log for next cycle

- [ ] **A17 — Pricing lookup nondeterministic on equal-length prefixes (item 0.6)** 🟡
- [ ] **A18 — Upstream cert pinning (item 1.2)** 🟡
- [ ] **A19 — `/healthz` / `/readyz` endpoints (item 1.3)** 🟡
- [ ] **A20 — Shutdown waits on active streams (item 1.4)** 🟡
- [ ] **A21 — SSE `\r\n\r\n` / `\r\r` framing (item 1.11)** 🟡
- [ ] **A22 — Strict operation-path matching (item 1.12)** 🟡
- [ ] **A23 — Hot certificate reload (item 2.1)** 🟡
- [ ] **A24 — Circuit breaker per upstream (item 2.2)** 🟡
- [ ] **A25 — OTel module version alignment (item 2.4)** 🟡
- [ ] **A26 — Tool-call streaming coverage (item 2.5)** 🟡
- [ ] **A27 — Transport tuning per upstream (item 2.6)** 🟡
- [ ] **A28 — Stream non-streaming JSON through (item 2.7)** 🟡
- [ ] **A29 — Content-Length pre-check (item 2.8)** 🟡

## Low — opportunistic

- [ ] LLMTAP_OTLP_HEADERS env (item 1.8) 🟢
- [ ] LLMTAP_SAMPLE_RATIO env (item 1.9) 🟢
- [ ] Cost as histogram (item 2.3) 🟢
- [ ] Pricing trie lookup (item 2.9) 🟢
- [ ] SECURITY.md + Dependabot (item 3.3) 🟢
- [ ] Documentation honesty pass (item 3.4) 🟢
- [ ] `runtime/debug.ReadBuildInfo` fallback (item 3.5) 🟢
- [ ] Load test harness (item 3.6) 🟢

## Execution order

1. **A1 (cardinality bomb)** — start here. New FATAL FLAW. Operator
   infrastructure protection.
2. A2 (token-leak suppression) — Critical; small surgical fix.
3. A3 → A4 (request/response data-loss).
4. A5 → A6 → A7 (telemetry correctness).
5. A8 (log level) — small, unblocks debugging the rest.
6. A9 → A10 → A11 → A12 → A13 — DoS + auth boundary.
7. A14 (demo) + A15 (CI) — meta-protections.
8. A16 (redaction).

Medium block (A17–A29) and the Low list address the next cycle.
