# Changelog

All notable changes to llmtap are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Anthropic routing: the `/anthropic` route prefix is now stripped
  before forwarding (`strip_prefix`, default on for Anthropic), so
  requests reach `/v1/messages` instead of 404ing at
  `/anthropic/v1/messages`.
- Route matching is segment-aware: `/v1x/foo` no longer matches the
  `/v1` route, and dot-segment paths (`/v1/../v1/chat/completions`)
  are canonicalized before matching.
- Client disconnects (499) and over-cap request bodies (413) no longer
  count as upstream failures, so they cannot trip the circuit breaker.
- Graceful shutdown closes the listener before draining active
  streams; sustained streaming load no longer prevents shutdown from
  completing inside its deadline.
- Cost metrics render under the documented names in Prometheus
  (`gen_ai_client_cost_usd_*`); the bundled Grafana dashboard and
  README PromQL examples now match what the proxy actually emits.
- `llmtap version` and `--help` print to stdout, not stderr.
- `make run` falls back to `config.example.yaml` when no `config.yaml`
  exists, so a fresh clone runs out of the box.

### Added

- Milestone 3 enrichment (D35–D41): OpenAI Responses API and native
  Gemini parsers (streaming, tool events, cached/reasoning usage);
  Anthropic thinking-delta TTFT; `error.type`, service-tier/refusal,
  and emitted schema URL; caller attribution with capped metric labels;
  per-request summary logs; `demo` and `gen-cert` commands; outcome-aware
  cost/error/latency sampling; redacted JSONL recording plus `replay` and
  `diff` commands.
- Self-health metrics (D22): `llmtap.breaker.state` (0/1/2 per
  upstream) with trip/recover/half-open log lines,
  `llmtap.auth.rejected.total`, `llmtap.streams.active`,
  `llmtap.requests.rejected.total{reason}`. `llmtap.requests.total`
  now counts every outcome past the health probes — enriched,
  passthrough, and rejects — with correct status codes.
- Telemetry / TLS observability (D23): `otel.SetErrorHandler` logs to
  slog and increments `llmtap.telemetry.export_errors.total`;
  `llmtap.tls.cert_not_after` gauge + `llmtap.tls.cert_reload_failures.total`
  counter; boot/reload warn when the leaf expires within 14 days.
- True readiness (D24): `/readyz` is 200 only after the first
  successful OTLP export, and flips 503 immediately on SIGTERM
  (before stream drain). README documents the k8s probe / preStop
  pairing.
- Post-header stream idle timeout (D25): `http.stream_idle_timeout`
  (default 5m) force-closes stalled upstream bodies after headers so
  hung SSE/JSON streams cannot pin concurrency slots forever. Trips
  emit span event `llmtap.stream.idle_timeout` and counter
  `llmtap.streams.idle_timeouts.total`.
- Production defaults & error bodies (D26): shipped
  `config.production.yaml` with `max_in_flight` + breaker enabled;
  README "Sizing" (GOMEMLIMIT, concurrency, stream idle); proxy-generated
  502/413 responses are JSON (`llmtap_proxy_error`) with `X-Request-Id`
  / `X-Trace-Id` on error paths.
- Kubernetes packaging (D27): Helm chart under `deploy/helm/llmtap`
  (Deployment, Service, ConfigMap, PDB, probes, resources, GOMEMLIMIT);
  cert-manager + NetworkPolicy examples in `deploy/k8s/`;
  `auth.tokens_file` / `LLMTAP_AUTH_TOKENS_FILE`; `llmtap healthcheck`
  subcommand + Dockerfile `HEALTHCHECK` (distroless-safe).
- Dashboard v2 (D28): error-rate panel + proxy self-health row (breaker,
  rejects, export errors, streams, idle timeouts, cert, auth); starter
  alert rules in `deploy/grafana/alerts/llmtap-alerts.yaml`. Grafana.com
  marketplace publish remains a release-train step (artifact is ready).
- Lane F security & test debt (D29–D34):
  - Auth DoS (D29): 30s negative-result cache; `auth.MaxAllowListTokens`
    ceiling (64); inbound W3C `baggage` stripped on upstream forward;
    global OTel propagator is TraceContext-only.
  - Test debt (D30): breaker FSM unit tests on injected clock; config
    YAML→env precedence test; telemetry setup tests; CI coverage gate
    70% with per-package floors; fuzz job.
  - `content.mode: logs` (D31): content via OTel log signal using
    `Providers.Logger` (parallel to span events).
  - More tests (D32): fuzz targets (redact, auth, config, SSE); client
    abort mid-stream; streaming redaction chunk-boundary test.
  - Load harness (D33): streaming scenario, real SDK meters, heap-delta
    assertion; published stub benchmark numbers in README Sizing.
  - Compliance (D34): GDPR/retention/residency README section; `NOTICE`
    in release artifacts.
- `LLMTAP_OTLP_ACKNOWLEDGE_INSECURE` env var. Without it, env-only
  deployments (containers) pointing at a collector by hostname could
  never pass the plaintext-OTLP validator — the demo compose stack
  itself failed to boot.
- Compose smoke test in CI: brings up the demo stack with a stub
  upstream, proxies a request, and asserts token/cost series land in
  Prometheus. Guards the quick start end to end.
- `llmtap check` subcommand: loads and validates a config file, prints
  the resolved listener / upstream / telemetry summary, and exits
  non-zero on any error. Suitable as a deploy-time gate.
- Strict config parsing: unknown YAML fields are rejected at load
  time, malformed boolean/number env overrides log a warning instead
  of being silently ignored, and `upstream.target` must be an absolute
  http(s) URL.
- Refreshed embedded pricing catalogue (2026-07 snapshot; the snapshot
  date is printed by `llmtap version`) and a
  `llmtap.pricing.misses.total` counter that increments whenever token
  usage is observed for a model with no pricing entry.
- Default redaction profile now also masks Hugging Face `hf_…` tokens,
  xAI `xai-…` keys, and raw PEM private-key blocks.
- Captured completion text is capped at 64 KiB per response
  (`llmtap.content_truncated` span attribute marks the cut), with a
  matching SDK-level span-attribute length limit as backstop.
- Error-body snippets are redacted *before* truncation, so a secret
  straddling the snippet boundary can no longer leak its prefix.

### Changed

- Internal: per-request enrichment extracted from the
  `responseInterceptor` closure pyramid into a `requestObserver`
  struct with explicit fields and a documented single-writer
  concurrency invariant. Finalize / status-recovery paths are now
  unit-testable without standing up a ReverseProxy.
- Internal: provider parsing split into pure `Decode*` + span
  `emit*` halves; shared `streamCollector` for WrapStream; path
  helpers moved to `paths.go`; provider-name validation uses the
  registry (`provider.Known`) instead of a hard-coded config list.
- `gen_ai.server.time_to_first_byte` histogram (and matching span
  attribute) from the previously unused `Info.FirstByteAt` field.
- `content.mode: logs` is rejected at validation time until the log
  signal is actually implemented (previously accepted and silently
  captured nothing).
- CI and release workflows pin all third-party actions to commit SHAs,
  Docker base images to digests, and tool installs (`syft`,
  `govulncheck`) to exact versions. Releases fail closed when SLSA
  provenance is missing, ship a signed `checksums.txt`, and only move
  the `:latest` image tag on full (non-pre-release) semver tags.

## [0.1.4] - 2026-05-12

### Added

- Env-only config additions: `LLMTAP_OTLP_HEADERS`,
  `LLMTAP_SAMPLE_RATIO`.
- `runtime/debug.ReadBuildInfo` fallback so `go install` builds report
  a real version.
- `gen_ai.client.cost.usd` is now a histogram, with a companion
  `.total` counter.
- Go-native load harness (`make load-test`) gating p99 added latency
  and goroutine leaks in CI.

### Fixed

- Auth hardening: request shape-check + LRU cache in front of argon2,
  NumCPU-bounded argon2 semaphore (429 on overload), constant-time
  verification across all stored hashes.
- Atomic cert+key reload eliminates a torn-read window during
  certificate rotation.
- Circuit-breaker probe slot released on every exit path.
- Constant-time SPKI pin comparison; SNI ServerName skipped for
  IP-literal upstreams.
- Default redaction profile extended to Google, Groq, Replicate, and
  GitHub token shapes.
- SSE parser overflow reported as a counter instead of a one-shot flag.

### Changed

- Pricing lookup rewritten as a byte trie (sub-200ns per lookup).

## [0.1.3] - 2026-05-12

### Fixed

- Release workflow downloads only the artifacts it ships.

## [0.1.2] - 2026-05-12

### Fixed

- Docker login in the sign-image job so cosign can push signatures to
  GHCR.

## [0.1.1] - 2026-05-12

### Fixed

- Dockerfile Go toolchain bumped to 1.26-alpine to satisfy the module
  floor.

## [0.1.0] - 2026-05-12

Initial release.

- Single-binary reverse proxy for OpenAI (Chat Completions,
  Embeddings) and Anthropic (Messages) wire formats, streaming and
  non-streaming, with byte-for-byte passthrough.
- OpenTelemetry traces, metrics, and logs following the GenAI
  semantic conventions: token usage, TTFT, duration, finish reasons,
  and USD cost from an embedded (overridable) pricing catalogue.
- Optional prompt/completion capture as span events, off by default,
  with mandatory-unless-explicitly-disabled redaction profiles.
- Bearer-token auth gate (argon2id), per-upstream concurrency caps,
  three-state circuit breakers, TLS with hot cert reload, mTLS,
  upstream SPKI pinning, bounded SSE parsing, graceful drain.
- Signed releases: cosign keyless signatures, SLSA-3 provenance,
  CycloneDX SBOMs; distroless nonroot container image.

[Unreleased]: https://github.com/colinedwardwood/llmtap/compare/v0.1.4...HEAD
[0.1.4]: https://github.com/colinedwardwood/llmtap/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/colinedwardwood/llmtap/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/colinedwardwood/llmtap/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/colinedwardwood/llmtap/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/colinedwardwood/llmtap/releases/tag/v0.1.0
