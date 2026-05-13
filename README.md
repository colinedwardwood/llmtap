# llmtap

> **OpenTelemetry tap for any LLM API.** Drop it between your app and OpenAI / Anthropic. Get traces, metrics, cost, and (optionally) redacted prompts in the observability stack you already run. Zero code changes.

`llmtap` is a single-binary HTTP reverse proxy that speaks the OpenAI Chat Completions / Embeddings and Anthropic Messages wire formats. It forwards every request byte-for-byte, streams every response byte-for-byte, and out-of-band emits OpenTelemetry traces, metrics, and logs that follow the [GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/).

The destination is whatever already speaks OTLP — Grafana LGTM, Honeycomb, Datadog, Tempo+Prometheus, Jaeger, the Collector. **No accounts. No SDK. No vendor lock-in.**

---

## Why

Engineering teams ship LLM features faster than they instrument them. Surprise five-figure bills, agents that fail in ways nobody can debug, no audit trail for what's being sent to third-party models. Existing tools (Langfuse, LangSmith, Helicone, Logfire, Phoenix) each want their own SDK *and* their own backend. Teams that already pay for an o11y stack don't want a sixth silo.

`llmtap` solves that by not being a destination. It's the cheapest thing that turns LLM API traffic into clean OTel signals.

---

## Quick start

```bash
# 1. bring up llmtap + Tempo + Prometheus + Loki + Grafana on TLS
make compose-up

# 2. point any OpenAI-compatible client at the proxy
export OPENAI_BASE_URL=https://localhost:4443/v1
# Trust the self-signed cert; e.g. with curl:
curl --cacert <(docker compose -f deploy/compose/docker-compose.yml exec llmtap cat /etc/llmtap/tls/tls.crt) \
     -H "Authorization: Bearer $OPENAI_API_KEY" \
     "$OPENAI_BASE_URL/chat/completions" -d '{...}'

# (Python SDK users: set REQUESTS_CA_BUNDLE / SSL_CERT_FILE
# to the generated cert path, or use `verify=False` for a demo.)

# 3. open Grafana
open http://localhost:3000          # admin / admin
#    → "LLM Observability — llmtap" dashboard
```

Anthropic works the same way:

```bash
export ANTHROPIC_BASE_URL=https://localhost:4443/anthropic
```

For a plaintext-loopback demo (no TLS handshake to worry about, **not for production**):

```bash
make compose-up-insecure
export OPENAI_BASE_URL=http://localhost:4000/v1
```

---

## Install

### From source

```bash
git clone https://github.com/colinedwardwood/llmtap
cd llmtap
make build
./llmtap up --config config.yaml
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
| OTLP headers (`k=v,k=v`) | `LLMTAP_OTLP_HEADERS` | `""` |
| OTLP sample ratio (`[0,1]`) | `LLMTAP_SAMPLE_RATIO` | `1.0` |
| Content capture | `LLMTAP_CAPTURE` | `off` (or `events`, `logs`) |
| Service name | `LLMTAP_SERVICE_NAME` | `llmtap` |
| Environment | `LLMTAP_ENV` | `dev` |

Structural config (upstreams, auth tokens, breaker thresholds, cert pins, redaction profile) lives in YAML — see `config.example.yaml`. Highlights:

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
                                     # masks sk-*, AKIA*, xox*, JWTs,
                                     # emails, JSON private_key (default)
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
```

Routing is longest-prefix-match. Reserved prefixes `/healthz` and `/readyz` cannot be configured.

---

## What gets emitted

### Traces (one span per LLM call)

Span name follows the GenAI spec: `chat <model>`, `embeddings <model>`.

Attributes (subset — full list in [`internal/genai/genai.go`](internal/genai/genai.go)):

| Attribute | Description |
|---|---|
| `gen_ai.system` | `openai`, `anthropic` |
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

Model labels are normalized (date suffixes stripped, lowercase) and bounded by a per-process cardinality cap (default 200; overflow → `_other`) so a misbehaving client can't blow up your Prometheus.

The proxy itself is wrapped in [`otelhttp`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp). `slog` flows through `otelslog` so logs share the trace context.

### Example PromQL

```promql
# Cost per hour, by model (counter)
sum by (gen_ai_request_model) (
  rate(gen_ai_client_cost_usd_total_total[5m])
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
sum by (token_type) (rate(gen_ai_client_token_usage_token_sum[5m]))
```

A starter dashboard ships in [`deploy/grafana/dashboards/llmtap-overview.json`](deploy/grafana/dashboards/llmtap-overview.json).

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

| Profile | What it masks |
|---|---|
| `off` | nothing — opt-in only, raw content ships |
| `default` (recommended) | OpenAI `sk-…` tokens, Slack `xox{abprs}-…`, AWS `AKIA…` access keys, GCP service-account `private_key` field, common JWT shape, RFC-5322 emails |
| `strict` | `default` plus credit-card numbers (Luhn-validated, no false positives on prose digits), US SSNs, E.164 phone numbers |

The default profile is engaged automatically when content capture is turned on; flipping `mode: events` without thinking about redaction will NOT silently ship raw secrets. Operators who want raw content must explicitly set `content.redact: off`.

`content.mode: logs` routes the same content through the OTel log signal so retention can be scoped separately at the collector.

**Body forwarding.** llmtap forwards request bodies byte-for-byte to the upstream (`request.max_body_bytes` ceiling, default 32 MiB; above it the proxy returns 413 without contacting the upstream). It strips the `Accept-Encoding` header from the outbound request so Go's `http.Transport` can transparently decompress gzip responses, and adds a `Via: llmtap` marker so audit logs can see it in the chain. It does **not** strip auth headers. The auth header llmtap consumes (`auth.header`, default `X-LLMTAP-Token`) IS stripped before forwarding so the listener-side secret never reaches the upstream.

---

## Operability

- `/healthz` returns 200 once `Run` has accepted on the listener — k8s / ALB / Cloudflare Tunnel liveness target.
- `/readyz` returns 200 once telemetry exporters are up, 503 + `Retry-After: 1` otherwise.
- Both endpoints short-circuit BEFORE the auth gate so probes work without credentials.
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

CI runs on every PR + push to `main` (`.github/workflows/ci.yml`): test (`-race -count=1 -timeout=2m -covermode=atomic` with a 60% line-coverage gate on `internal/`), `golangci-lint v2`, `govulncheck`, `trivy fs --severity HIGH,CRITICAL`, and a three-target build matrix.

Tags `v*` trigger the release pipeline (`.github/workflows/release.yml`): cross-compiled binaries → aggregate SHA-256 → SLSA-3 provenance → CycloneDX SBOMs (syft) → cosign keyless sign each binary → multi-arch docker buildx push to `ghcr.io` (with provenance + SBOM attestations) → cosign sign the image digest → GitHub release with every artifact.

---

## Roadmap (v0.2+)

- Bedrock, Gemini, Ollama-native parsers.
- `llmtap record` + `llmtap replay` + `llmtap diff` for migration A/B.
- Sampling strategies tied to GenAI signals (cost-weighted, error-biased).
- Stricter readiness signal (wait for first successful OTLP export round-trip).
- Per-upstream rate-limit (request-per-second + token-per-second), in addition to the existing concurrency cap.

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
