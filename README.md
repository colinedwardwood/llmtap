# llmtap

> **OpenTelemetry tap for OpenAI- and Anthropic-compatible LLM APIs.** Drop it between your app and the provider. Get traces, metrics, cost, and (optionally) redacted prompts in the observability stack you already run. Zero code changes.

`llmtap` is a single-binary HTTP reverse proxy that speaks the OpenAI Chat Completions / Embeddings and Anthropic Messages wire formats. It forwards every request byte-for-byte, streams every response byte-for-byte, and out-of-band emits OpenTelemetry traces, metrics, and logs that follow the [GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/).

The destination is whatever already speaks OTLP — Grafana LGTM, Honeycomb, Datadog, Tempo+Prometheus, Jaeger, the Collector. **No accounts. No SDK. No vendor lock-in.**

---

## Why

Engineering teams ship LLM features faster than they instrument them. Surprise five-figure bills, agents that fail in ways nobody can debug, no audit trail for what's being sent to third-party models. Existing tools (Langfuse, LangSmith, Helicone, Logfire, Phoenix) each want their own SDK *and* their own backend. Teams that already pay for an o11y stack don't want a sixth silo.

`llmtap` solves that by not being a destination. It's the cheapest thing that turns LLM API traffic into clean OTel signals.

---

## Quick start (60 seconds, local demo)

The fastest path is the plaintext-loopback demo stack — no certificates to trust, everything on localhost. **Demo only; the production path below uses TLS.**

```bash
# 1. bring up llmtap + Tempo + Prometheus + Loki + Grafana (plaintext on :4000)
make compose-up-insecure

# 2. send a real request through the proxy
export OPENAI_BASE_URL=http://localhost:4000/v1
curl "$OPENAI_BASE_URL/chat/completions" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5-mini",
    "messages": [{"role": "user", "content": "Say hello in five words."}]
  }'

# 3. open Grafana → "LLM Observability — llmtap" dashboard
open http://localhost:3000          # anonymous admin, no login needed
```

Every SDK that lets you override the base URL works the same way — set `OPENAI_BASE_URL` (or `base_url=` in the constructor) and change nothing else. Anthropic clients point at the `/anthropic` route:

```bash
curl http://localhost:4000/anthropic/v1/messages \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-fable-5",
    "max_tokens": 64,
    "messages": [{"role": "user", "content": "Say hello in five words."}]
  }'
```

Tear down with `make compose-down`.

### Production path: TLS

For anything beyond a localhost demo, run the TLS stack. It generates a self-signed cert on first boot (SAN covers `localhost` and `127.0.0.1`); in real deployments you'd mount certs from your PKI instead.

```bash
# 1. bring the stack up on HTTPS :4443
make compose-up

# 2. copy the generated cert out of the container
docker compose -f deploy/compose/docker-compose.yml \
  cp llmtap:/etc/llmtap/tls/tls.crt ./llmtap-ca.crt

# 3. same curl as above, now over TLS
export OPENAI_BASE_URL=https://localhost:4443/v1
curl --cacert ./llmtap-ca.crt "$OPENAI_BASE_URL/chat/completions" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5-mini",
    "messages": [{"role": "user", "content": "Say hello in five words."}]
  }'
```

How to make each client trust the cert:

| Client | Trust the proxy cert with |
|---|---|
| curl | `--cacert ./llmtap-ca.crt` |
| Python (`openai`, `anthropic` — both use httpx) | `export SSL_CERT_FILE=$PWD/llmtap-ca.crt` |
| Node.js (`openai`, `@anthropic-ai/sdk`) | `export NODE_EXTRA_CA_CERTS=$PWD/llmtap-ca.crt` |
| Go (`net/http`, official SDKs) | `export SSL_CERT_FILE=$PWD/llmtap-ca.crt` (honored by `crypto/x509` on Linux/macOS), or add to `tls.Config.RootCAs` |
| Anything else | add `llmtap-ca.crt` to the OS trust store |

---

## Install

### From source

```bash
git clone https://github.com/colinedwardwood/llmtap
cd llmtap
make run          # builds, then starts against config.yaml
                  # (falls back to config.example.yaml on a fresh clone)
```

### `go install`

```bash
go install github.com/colinedwardwood/llmtap/cmd/llmtap@latest
llmtap version    # picks up the version from runtime/debug.BuildInfo
```

### Pre-built binaries

Download from [Releases](https://github.com/colinedwardwood/llmtap/releases) — Linux amd64/arm64, macOS arm64. Each release ships:

- The binary, a SHA-256 sidecar, and a [cosign](https://github.com/sigstore/cosign) signature + certificate.
- CycloneDX SBOMs for the source tree and each binary.
- A [SLSA-3](https://slsa.dev/) provenance attestation covering all binaries.

Verify a downloaded binary:

```bash
cosign verify-blob \
  --certificate llmtap-linux-amd64.pem \
  --signature   llmtap-linux-amd64.sig \
  --certificate-identity-regexp '^https://github\.com/colinedwardwood/llmtap/' \
  --certificate-oidc-issuer     https://token.actions.githubusercontent.com \
  llmtap-linux-amd64
```

### Docker

```bash
docker run --rm -p 4443:4443 \
  -v /etc/llmtap/tls:/etc/llmtap/tls:ro \
  -e LLMTAP_LISTEN=0.0.0.0:4443 \
  -e LLMTAP_TLS_CERT_FILE=/etc/llmtap/tls/tls.crt \
  -e LLMTAP_TLS_KEY_FILE=/etc/llmtap/tls/tls.key \
  -e LLMTAP_OTLP_ENDPOINT=otel-collector.example:4317 \
  ghcr.io/colinedwardwood/llmtap:latest
```

The container image is multi-arch (`linux/amd64`, `linux/arm64`) and is signed + has SLSA provenance + an embedded SBOM as buildx attestations. Verify with `cosign verify ghcr.io/colinedwardwood/llmtap:<tag>` against the same identity regex above.

---

## Configuration

Three layers, lowest precedence first: built-in defaults → `config.yaml` → `LLMTAP_*` env vars. The full schema is in [`config.example.yaml`](config.example.yaml).

| Setting | Env | Default |
|---|---|---|
| Listen address | `LLMTAP_LISTEN` | `127.0.0.1:4000` |
| Allow plaintext on non-loopback | `LLMTAP_ALLOW_INSECURE` | `false` |
| TLS cert file | `LLMTAP_TLS_CERT_FILE` | `""` |
| TLS key file | `LLMTAP_TLS_KEY_FILE` | `""` |
| Client CA (enables mTLS) | `LLMTAP_TLS_CLIENT_CA_FILE` | `""` |
| OTLP endpoint | `LLMTAP_OTLP_ENDPOINT` | `localhost:4317` |
| OTLP protocol | `LLMTAP_OTLP_PROTOCOL` | `grpc` (or `http`) |
| OTLP insecure | `LLMTAP_OTLP_INSECURE` | `true` |
| Acknowledge plaintext OTLP to a non-local host | `LLMTAP_OTLP_ACKNOWLEDGE_INSECURE` | `false` |
| OTLP headers (`k=v,k=v`) | `LLMTAP_OTLP_HEADERS` | `""` |
| OTLP sample ratio (`[0,1]`) | `LLMTAP_SAMPLE_RATIO` | `1.0` |
| Always sample calls costing at least USD | `LLMTAP_COST_SAMPLE_THRESHOLD_USD` | `0` (off) |
| Always sample calls slower than duration | `LLMTAP_SLOW_SAMPLE_THRESHOLD` | `0` (off) |
| Content capture | `LLMTAP_CAPTURE` | `off` (or `events`, `logs`) |
| Caller attribution header | `LLMTAP_ATTRIBUTION_HEADER` | `""` |
| Redacted JSONL recording path | `LLMTAP_RECORDING_PATH` | `""` (off) |
| Service name | `LLMTAP_SERVICE_NAME` | `llmtap` |
| Environment | `LLMTAP_ENV` | `dev` |

Structural config (upstreams, auth tokens, breaker thresholds, cert pins, redaction profile) lives in YAML — see `config.example.yaml`. For production, start from [`config.production.yaml`](config.production.yaml) (concurrency cap + breaker on, non-loopback TLS bind). Highlights:

```yaml
upstreams:
  - name: openai
    prefix: /v1
    target: https://api.openai.com
    provider: openai
    max_in_flight: 256              # 429 + Retry-After on overflow
    pin_sha256:                      # optional SPKI pin list
      - "abcdef…64-char-hex"
    breaker:
      failures: 5
      window: 30s
      recovery_window: 30s

content:
  mode: events                       # off | events | logs
  redact: default                    # off | default | strict
                                     # masks sk-*, AKIA*, xox*, hf_*,
                                     # xai-*, PEM keys, JWTs, emails,
                                     # JSON private_key, … (default)
                                     # + Luhn CCs, US SSNs, E.164 (strict)

auth:
  header: X-LLMTAP-Token             # bearer-token allow-list; argon2id
  tokens:                            # generate with `llmtap hash-token`
    - "$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>"

request:
  max_body_bytes: 33554432           # 32 MiB; 413 above without
                                     # touching the upstream

http:
  body_read_timeout: 30s             # slow-uploader DoS guard
  cert_reload_interval: 30s          # cert-manager / Let's Encrypt
                                     # renewals picked up live
  stream_idle_timeout: 5m            # kill hung post-header bodies

attribution:
  header: X-Caller                    # else mTLS CN, then auth fingerprint
  max_cardinality: 200                # extra callers collapse to _other

recording:
  path: ./recordings/traffic.jsonl    # payloads inherit content.redact
```

Routing is longest-prefix-match. Reserved prefixes `/healthz` and `/readyz` cannot be configured.

### Provider compatibility

| Provider/API | Parser | Streaming | Tool calls | Usage detail |
|---|---|---:|---:|---:|
| OpenAI Chat Completions | native | yes | yes | cached/reasoning |
| OpenAI Responses API | native | yes | function + built-in tool events | cached/reasoning |
| Anthropic Messages | native | yes | yes | cache reads |
| Google Gemini `generateContent` | native | yes (SSE) | yes | cached/thought tokens |
| Azure OpenAI, Groq, OpenRouter, vLLM, Ollama OpenAI endpoint | OpenAI-compatible | yes | yes | when returned |

Provider-specific extensions can differ from their compatibility target. Bedrock
(SigV4) and Ollama's native endpoint are not parsed.

For a keyless local upstream, run `llmtap demo --listen 127.0.0.1:8080`.
`llmtap gen-cert --host localhost --out-dir ./certs` creates a local ECDSA
certificate. Recorded traffic can be exercised with
`llmtap replay --target http://127.0.0.1:8080 --out replay.jsonl traffic.jsonl`
and compared using `llmtap diff traffic.jsonl replay.jsonl`.

Proxy-generated faults (notably upstream `502` and body `413`) return a JSON body `{"error":{"type":"llmtap_proxy_error","code":"…","message":"…"}}` plus `X-Request-Id` (echoed from the client header when set, else generated) and `X-Trace-Id` when a span is active — so SDK users can tell llmtap from the provider and join the complaint to a span.

---

## Sizing

llmtap is mostly I/O-bound; CPU shows up in JSON parse, redaction, and argon2 auth verifies. Ballpark budgets from the fixed buffers in tree (not a synthetic load lab):

| Resource | Bound | Notes |
|---|---|---|
| Enrichment head | 1 MiB / request | Request body sneak-peek for span attributes |
| Stream assemble (content on) | 64 KiB / stream | Completion text cap when `content.mode: events` |
| Auth verify | ~64 MiB peak / argon2 | Semaphore-bounded; prefer short allow-lists |
| `max_in_flight` | operator | Hard fan-out to one upstream |

**Memory rule of thumb:** plan ~2–4 MiB RSS per concurrent enriched request (head buffer + HTTP stacks + OTel), plus stream lifetime. With `max_in_flight: 256` per upstream, size the cgroup for roughly `256 × 4 MiB × upstreams` plus a 256–512 MiB baseline, then set:

```bash
# Match the container memory limit (example: 2 GiB cgroup).
GOMEMLIMIT=1800MiB
```

Go will GC harder as it approaches the limit; without `GOMEMLIMIT`, a rare traffic spike can grow RSS until the kubelet OOM-kills the pod mid-stream.

**Concurrency:** leave `max_in_flight` unset only for local demos. In production start at `256` (as in `config.production.yaml`) and lower it if `llmtap.requests.rejected.total{reason="concurrency"}` stays near zero while upstream p99 latency climbs — the useful knob is often the *upstream's* limit, not llmtap's. Pair with the circuit breaker (`failures: 5`, `window: 30s`) so a brownout fails fast with `503` instead of pinning every slot on hung dials.

**Streams:** `http.stream_idle_timeout` (default `5m`) force-closes bodies that go silent after headers; watch `llmtap.streams.idle_timeouts.total` and `llmtap.streams.active`. On SIGTERM, `/readyz` flips false immediately; size `terminationGracePeriodSeconds` to cover Endpoint drain + `shutdown_timeout` (distroless has no `sleep` for `preStop`).

**Auth:** `auth.tokens` is capped at **64** PHC hashes (`auth.MaxAllowListTokens`). Each miss runs argon2 against every entry — keep the allow-list short. Failed tokens are negative-cached for 30s so brute-force replays skip argon2. Prefer `auth.tokens_file` for Secret mounts.

**Throughput:** non-streaming chat on a quiet laptop typically clears **~2,000–4,000 req/s/core** through the proxy path when the upstream is a local stub (see `test/load`); real LLM backends dominate wall time. Streaming: **~100 concurrent SSE** connections with **<32 MiB** heap growth in the load harness (`TestLoadStreaming`).

---

## What gets emitted

### Traces (one span per LLM call)

Span name follows the GenAI spec: `chat <model>`, `embeddings <model>`.

Attributes (subset — full list in [`internal/genai/genai.go`](internal/genai/genai.go)):

| Attribute | Description |
|---|---|
| `gen_ai.provider.name` | `openai`, `anthropic`, `gcp.gen_ai` |
| `gen_ai.operation.name` | `chat`, `embeddings` |
| `gen_ai.request.model` / `gen_ai.response.model` | catches model fallbacks |
| `gen_ai.request.temperature` / `top_p` / `max_tokens` / `seed` / … | request shape |
| `gen_ai.response.id` / `finish_reasons` | response identity (incl. `tool_calls`, `tool_use`) |
| `gen_ai.usage.input_tokens` / `output_tokens` | from final chunk for streams |
| `gen_ai.cost.usd` | computed from a configurable price table |
| `gen_ai.time_to_first_token` | streams only; fires on text OR tool-call deltas |
| `http.response.status_code` | upstream response code |
| `http.response.body_size` | upstream body byte count on 4xx/5xx |
| `http.response.body_snippet` | first 1 KiB of an error body (only when `content.mode: events`) |

Span events (when `content.mode: events`): `gen_ai.system.message`, `gen_ai.user.message`, `gen_ai.assistant.message`, `gen_ai.tool.message`, `gen_ai.choice`. **Off by default** for privacy. When on, every content string is run through the [redaction profile](#privacy) first; raw content only ships under `content.redact: off`.

### Metrics

| Metric | Type | Labels |
|---|---|---|
| `gen_ai.client.operation.duration` | histogram (s) | system, operation, model, status |
| `gen_ai.client.token.usage` | histogram (tokens) | system, operation, model, token_type |
| `gen_ai.client.time_to_first_token` | histogram (s) | system, operation, model, status |
| `gen_ai.client.cost.usd` | **histogram (USD)** | system, request_model, response_model |
| `gen_ai.client.cost.usd.total` | counter (USD) | system, request_model, response_model |
| `llmtap.requests.total` | counter | upstream, status |
| `llmtap.requests.in_flight` | up-down counter | upstream |
| `llmtap.requests.rejected.total` | counter | reason, upstream |
| `llmtap.breaker.state` | gauge | upstream (0/1/2) |
| `llmtap.streams.active` | up-down counter | — |
| `llmtap.streams.idle_timeouts.total` | counter | upstream |
| `llmtap.telemetry.export_errors.total` | counter | — |

Model labels are normalized (date suffixes stripped, lowercase) and bounded by a per-process cardinality cap (default 200; overflow → `_other`) so a misbehaving client can't blow up your Prometheus.

The proxy itself is wrapped in [`otelhttp`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp). `slog` flows through `otelslog` so logs share the trace context.

### Example PromQL

Prometheus's OTLP receiver translates dots to underscores and appends
unit/type suffixes (`.duration` + unit `s` → `_duration_seconds`;
histograms grow `_bucket`/`_sum`/`_count`). The queries below are
written against the translated names, matching the shipped dashboard.

```promql
# Cost per hour, by model (counter)
sum by (gen_ai_request_model) (
  rate(gen_ai_client_cost_usd_total[5m])
) * 3600

# p95 cost per call, by model (histogram)
histogram_quantile(0.95, sum by (le, gen_ai_request_model) (
  rate(gen_ai_client_cost_usd_bucket[5m])
))

# p95 time-to-first-token
histogram_quantile(0.95, sum by (le, gen_ai_request_model) (
  rate(gen_ai_client_time_to_first_token_seconds_bucket[5m])
))

# Tokens / second, in vs out
sum by (token_type) (rate(gen_ai_client_token_usage_sum[5m]))
```

A starter dashboard ships in [`deploy/grafana/dashboards/llmtap-overview.json`](deploy/grafana/dashboards/llmtap-overview.json) (v2: error-rate + self-health row). Alert expressions: [`deploy/grafana/alerts/llmtap-alerts.yaml`](deploy/grafana/alerts/llmtap-alerts.yaml).

---

## Architecture

```
              ┌────────────────────────────────────────────────┐
   client ───▶│  llmtap :4443 (HTTPS)                          │───▶ api.openai.com
              │   ├─ /healthz, /readyz                         │     api.anthropic.com
              │   ├─ bearer-token gate (argon2id)              │
              │   ├─ circuit breaker + concurrency cap         │
              │   ├─ body read deadline / hard cap → 413       │
              │   ├─ httputil.ReverseProxy (tuned Transport,   │
              │   │   ServerName + optional SPKI pinning)      │
              │   ├─ provider.Parse{Request,Resp}              │
              │   ├─ TeeReader stream-through + parse-on-close │
              │   ├─ SSE tee for streaming                     │
              │   ├─ content redaction profile                 │
              │   └─ otelhttp + GenAI instruments              │
              └────────────────────────────────────────────────┘
                          │ OTLP (gRPC | HTTP)
                          ▼
              traces / metrics / logs → your OTel backend
```

- **Streaming is tee-parsed in place.** No goroutine per stream; bytes flow unchanged. The first content delta (text or tool-call) records `time_to_first_token`; the final chunk's `usage` populates token counts. The span ends exactly once — whichever of EOF, error, or Close arrives first.
- **Non-streaming also streams through.** `FlushInterval=-1` + TeeReader + parse-on-close: client first-byte latency tracks upstream, not "full body buffered first".
- **Concurrency-safe.** One `*httputil.ReverseProxy` per upstream, shared across requests; per-request state travels through `context`.
- **Graceful shutdown.** `signal.NotifyContext` → drain active SSE streams (`Handler.WaitForStreams`) → `Server.Shutdown` → telemetry flush.
- **Hot cert reload.** The cert manager re-stats `tls.cert_file` / `tls.key_file` on a ticker (`http.cert_reload_interval`, default 30s) and atomically swaps the served cert when ModTime advances. No listener drop, no process restart.

---

## Kubernetes

Helm chart: [`deploy/helm/llmtap`](deploy/helm/llmtap). Stand-alone
cert-manager Certificate, NetworkPolicy, and per-replica breaker/cap
semantics: [`deploy/k8s/README.md`](deploy/k8s/README.md).

```bash
helm upgrade --install llmtap ./deploy/helm/llmtap
```

Auth hashes for the chart belong in a Secret mounted via `auth.tokens_file`
(not the ConfigMap). Container liveness uses `/healthz`; readiness uses
`/readyz` (true readiness after first OTLP export; false on SIGTERM).

---

## TLS

llmtap → upstream is **always TLS** when the upstream URL is `https://…`. The system trust store is used by default; per-upstream `pin_sha256` opt-in pins the leaf cert's SubjectPublicKeyInfo SHA-256 (resumption-safe via `VerifyConnection`). llmtap is **not** an HTTPS-interception proxy — it never mints fake certs for `api.openai.com` and never asks you to install a CA on your clients.

Client → llmtap defaults to plaintext on **loopback only**. To expose llmtap on a non-loopback interface you must do one of:

- **Configure TLS** (recommended): set `tls.cert_file` + `tls.key_file`. Clients then use `OPENAI_BASE_URL=https://llmtap.internal:4443/v1`.
- **Enable mTLS** (recommended for shared deployments): also set `tls.client_ca_file`. Every client must present a cert chained to that CA, turning llmtap into a hard policy boundary instead of an ambient one.
- **Enable bearer-token auth**: configure `auth.tokens` (argon2id-hashed; generate with `llmtap hash-token`) and clients send the token in `X-LLMTAP-Token` (or whatever `auth.header` is set to). Works with mTLS as defense-in-depth.
- **Acknowledge plaintext**: set `allow_insecure: true` (or `LLMTAP_ALLOW_INSECURE=true`). The default config refuses to start in this configuration on a non-loopback bind so you don't end up there by accident.

OTLP-side: if `telemetry.insecure: true` is paired with a non-loopback / non-RFC1918 endpoint, the proxy refuses to start unless `telemetry.acknowledge_insecure: true` is set explicitly. Plaintext OTLP off-host is rarely intentional.

Generate a quick local cert:

```bash
openssl req -x509 -newkey ec:<(openssl ecparam -name P-256) -nodes \
  -keyout tls.key -out tls.crt -days 365 -subj '/CN=llmtap.local'
LLMTAP_TLS_CERT_FILE=$PWD/tls.crt \
LLMTAP_TLS_KEY_FILE=$PWD/tls.key \
LLMTAP_LISTEN=0.0.0.0:4443 \
  ./llmtap up
```

---

## Privacy

`content.mode: off` (the default) sends **only metadata** to spans: model, parameters, token counts, finish reasons, cost. Prompt and completion text never leave the proxy.

`content.mode: events` attaches content as span events. **`content.redact`** controls how that content is scrubbed before it reaches the wire; default is `default`:

`content.mode: logs` emits the same redacted content via the **OTel log signal** instead of span events — route logs to a separate retention tier at the collector (`content.redact` applies identically).

| Profile | What it masks |
|---|---|
| `off` | nothing — opt-in only, raw content ships |
| `default` (recommended) | OpenAI `sk-…` tokens, Slack `xox{abprs}-…`, AWS `AKIA…` access keys, Google `AIza…` keys, Groq `gsk_…`, Replicate `r8_…`, Hugging Face `hf_…`, xAI `xai-…`, GitHub `ghp_…` / `github_pat_…`, GCP service-account `private_key` field, raw PEM private-key blocks, common JWT shape, RFC-5322 emails |
| `strict` | `default` plus credit-card numbers (Luhn-validated, no false positives on prose digits), US SSNs, E.164 phone numbers |

The default profile is engaged automatically when content capture is turned on; flipping `mode: events` without thinking about redaction will NOT silently ship raw secrets. Operators who want raw content must explicitly set `content.redact: off`.

Redaction is pattern-based and best-effort: it catches well-known credential shapes, not arbitrary secrets, so treat your tracing backend as sensitive whenever content capture is on.

### Compliance & data processing

llmtap is infrastructure software, not a SaaS. **You** are the data controller when `content.mode` is `events` or `logs` — prompts and completions may contain PII and are exported to **your** OTLP backend. By default llmtap does not persist content at rest. Setting `recording.path` explicitly enables local JSONL persistence; request, response, and URL values pass through `content.redact`, files are created mode `0600`, and the operator owns retention and encryption. OTLP retention, residency, encryption, and access control are determined by your collector and observability vendor.

| Topic | Posture |
|---|---|
| GDPR / lawful basis | Metadata-only mode (`off`) minimizes personal data. Content capture requires your own legal basis and DPA with the OTLP destination. |
| Retention | Configure at the collector (e.g. shorter log pipeline TTL than traces when using `mode: logs`). |
| Residency | llmtap forwards OTLP to the endpoint you configure; no cloud hop unless you point it at one. |
| Redaction ≠ anonymization | Masked secrets can still leave semantic content in spans/logs. |
| Subprocessors | Upstream LLM providers (OpenAI, Anthropic, …) receive the original request body regardless of capture mode. |

Release artifacts ship `NOTICE` (third-party licenses) and SPDX SBOMs from the release workflow.

**Body forwarding.** llmtap forwards request bodies byte-for-byte to the upstream (`request.max_body_bytes` ceiling, default 32 MiB; above it the proxy returns 413 without contacting the upstream). It strips the `Accept-Encoding` header from the outbound request so Go's `http.Transport` can transparently decompress gzip responses, and adds a `Via: llmtap` marker so audit logs can see it in the chain. It does **not** strip auth headers. The auth header llmtap consumes (`auth.header`, default `X-LLMTAP-Token`) IS stripped before forwarding so the listener-side secret never reaches the upstream.

---

## Operability

- `/healthz` returns 200 once `Run` has accepted on the listener — k8s / ALB / Cloudflare Tunnel liveness target.
- `/readyz` returns 200 only after the first successful OTLP export round-trip, and flips to 503 as soon as SIGTERM is received (before stream drain). Until the first export succeeds it stays 503 + `Retry-After: 1`.
- Both endpoints short-circuit BEFORE the auth gate so probes work without credentials.
- Kubernetes: point `livenessProbe` at `/healthz` and `readinessProbe` at `/readyz`. On rolling update, SIGTERM makes `/readyz` fail immediately — give the LB a moment with `terminationGracePeriodSeconds` (≥ shutdown timeout) and optionally a short `preStop` sleep so in-flight requests finish after the endpoint is marked NotReady.
- The proxy logs at `--log-level=info` by default; `debug` / `warn` / `error` change verbosity for both the stderr text handler and the OTLP log bridge.

---

## Performance

- Single static binary, ~16 MB stripped.
- Enrichment buffer: 1 MiB head per request (enough to parse `model`, `stream`, `temperature` from any realistic LLM body). Above the buffer the body still forwards byte-for-byte; only enrichment degrades.
- Hard request body ceiling: 32 MiB default (configurable, 413 above).
- Per-upstream Transport: `MaxIdleConnsPerHost=64`, `ResponseHeaderTimeout=60s`, `ForceAttemptHTTP2=true`. No invisible 2-conn-per-host bottleneck.
- SSE parser buffer: 1 MiB cap per stream; overflow drops the parser state, fires a `llmtap.sse_parser_overflow` span event, and continues forwarding bytes.
- Pricing lookup: O(K) byte-trie per system; ~170 ns/op at N=500 prefixes (zero allocations).
- Model-label cardinality: hard-capped at 200 distinct families per process; overflow buckets under `_other`. No Prometheus OOM from a buggy client.
- No goroutine per in-flight request beyond what `net/http` already creates.

---

## Development

```bash
make test     # race detector + verbose
make vet      # go vet
make lint     # golangci-lint
make build    # ./llmtap
make release  # cross-compile linux/{amd64,arm64} + darwin/arm64
```

CI runs on every PR + push to `main` (`.github/workflows/ci.yml`): test (`-race -count=1 -timeout=2m -covermode=atomic` with a 70% line-coverage gate and per-package floors), fuzz smoke tests, `golangci-lint v2`, `govulncheck`, `trivy fs --severity HIGH,CRITICAL`, and a three-target build matrix.

Tags `v*` trigger the release pipeline (`.github/workflows/release.yml`): cross-compiled binaries → aggregate SHA-256 → SLSA-3 provenance → CycloneDX SBOMs (syft) → cosign keyless sign each binary → multi-arch docker buildx push to `ghcr.io` (with provenance + SBOM attestations) → cosign sign the image digest → GitHub release with every artifact.

---

## Roadmap (v0.4+)

- Bedrock (SigV4-aware) and Ollama-native parsers.
- Collector recipes for whole-trace tail sampling.
- Optional replay header templates and richer semantic response diffing.

---

## Security

See [`SECURITY.md`](SECURITY.md) for vulnerability reporting, the coordinated-disclosure window, and how to verify a release artifact.

---

## Contributing

PRs welcome. Run `make test lint` before opening one. Please match local Go style — small interfaces defined where they're consumed, no `any` shortcuts, no echo comments.

---

## License

MIT — see [LICENSE](LICENSE).

## Acknowledgements

- Built on the [OpenTelemetry Go SDK](https://github.com/open-telemetry/opentelemetry-go).
- Conforms to the [OTel GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/).
- Inspired by `mitmproxy` and the question "why isn't this a thing yet?"
