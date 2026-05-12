# llmtap

> **OpenTelemetry tap for any LLM API.** Drop it between your app and OpenAI / Anthropic. Get traces, metrics, and cost in the observability stack you already run. Zero code changes.

`llmtap` is a single-binary HTTP reverse proxy that speaks the OpenAI Chat Completions / Embeddings and Anthropic Messages wire formats. It forwards every request unchanged, streams every response unchanged, and out-of-band emits OpenTelemetry traces, metrics, and logs that follow the [GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/).

The destination is whatever already speaks OTLP — Grafana LGTM, Honeycomb, Datadog, Tempo+Prometheus, Jaeger, the Collector. **No accounts. No SDK. No vendor lock-in.**

---

## Why

Engineering teams ship LLM features faster than they instrument them. The result is the worst observability gap of the last decade: surprise five-figure bills, agents that fail in ways nobody can debug, no audit trail for what's being sent to third-party models. Existing tools (Langfuse, LangSmith, Helicone, Logfire, Phoenix) each want their own SDK *and* their own backend. Teams that already pay for an o11y stack don't want a sixth silo.

`llmtap` solves that by not being a destination. It's the cheapest possible thing that turns LLM API traffic into clean OTel signals.

---

## Quick start

```bash
# 1. bring up llmtap + Tempo + Prometheus + Loki + Grafana
make compose-up

# 2. point any OpenAI-compatible client at the proxy
export OPENAI_BASE_URL=http://localhost:4000/v1

# 3. run literally anything that talks to OpenAI
python my_agent.py

# 4. open Grafana
open http://localhost:3000
#    → "LLM Observability — llmtap" dashboard
#    → click any span → tracing tree with prompts/completions as events
```

Anthropic works the same way:

```bash
export ANTHROPIC_BASE_URL=http://localhost:4000/anthropic
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

### Pre-built binaries

Download from [Releases](https://github.com/colinedwardwood/llmtap/releases) — Linux amd64/arm64, macOS arm64.

### Docker

```bash
docker run --rm -p 4000:4000 \
  -e LLMTAP_OTLP_ENDPOINT=otel-collector.example:4317 \
  -e LLMTAP_OTLP_INSECURE=true \
  ghcr.io/colinedwardwood/llmtap:latest
```

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
| Content capture | `LLMTAP_CAPTURE` | `off` (or `events`, `logs`) |
| Service name | `LLMTAP_SERVICE_NAME` | `llmtap` |
| Environment | `LLMTAP_ENV` | `dev` |

Upstreams are configured in YAML only:

```yaml
upstreams:
  - name: openai
    prefix: /v1
    target: https://api.openai.com
    provider: openai
  - name: anthropic
    prefix: /anthropic
    target: https://api.anthropic.com
    provider: anthropic
```

Routing is longest-prefix-match.

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
| `gen_ai.response.id` / `finish_reasons` | response identity |
| `gen_ai.usage.input_tokens` / `output_tokens` | from final chunk for streams |
| `gen_ai.cost.usd` | computed from a built-in price table |
| `gen_ai.time_to_first_token` | streams only |

Span events (when `content.mode: events`): `gen_ai.system.message`, `gen_ai.user.message`, `gen_ai.assistant.message`, `gen_ai.tool.message`, `gen_ai.choice`. **Off by default** for privacy.

### Metrics

| Metric | Type | Labels |
|---|---|---|
| `gen_ai.client.operation.duration` | histogram (s) | system, operation, model, status |
| `gen_ai.client.token.usage` | histogram (tokens) | system, operation, model, token_type |
| `gen_ai.client.time_to_first_token` | histogram (s) | system, operation, model, status |
| `gen_ai.client.cost.usd` | counter (USD) | system, request_model, response_model |
| `llmtap.requests.total` | counter | upstream, status |
| `llmtap.requests.in_flight` | up-down counter | upstream |

The proxy itself is wrapped in [`otelhttp`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp) — its own HTTP server is traced, and `slog` flows through `otelslog` so logs share the trace context.

### Example PromQL

```promql
# Cost per hour, by model
sum by (gen_ai_request_model) (
  rate(gen_ai_client_cost_usd_total[5m])
) * 3600

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
              ┌─────────────────────────────────────┐
   client ───▶│  llmtap :4000                       │───▶ api.openai.com
              │   ├─ httputil.ReverseProxy          │     api.anthropic.com
              │   ├─ provider.Parse{Request,Resp}   │
              │   ├─ SSE tee for streaming          │
              │   └─ otelhttp + GenAI instruments   │
              └─────────────────────────────────────┘
                          │ OTLP (gRPC|HTTP)
                          ▼
              traces / metrics / logs → your OTel backend
```

- **Streaming is tee-parsed in place.** No goroutine per stream; bytes flow through unchanged. The first content delta records `time_to_first_token`; the final chunk's `usage` populates token counts. The span ends exactly once — whichever of EOF, error, or Close arrives first.
- **Concurrency-safe.** One `*httputil.ReverseProxy` per upstream, shared across requests; per-request state travels through `context`.
- **Graceful shutdown.** `signal.NotifyContext` → server `Shutdown` → telemetry flush. No tests left dangling.

---

## TLS

llmtap → upstream is **always TLS** when the upstream URL is `https://…`. The system trust store is used; SNI is set; the client's `Authorization` header is forwarded byte-for-byte. llmtap is **not** an HTTPS-interception proxy — it never mints fake certs for `api.openai.com` and never asks you to install a CA on your clients.

Client → llmtap defaults to plaintext on **loopback only**. To expose llmtap on a non-loopback interface you must do one of:

- **Configure TLS** (recommended): set `tls.cert_file` + `tls.key_file`. Clients then use `OPENAI_BASE_URL=https://llmtap.internal:4443/v1`.
- **Enable mTLS** (recommended for shared deployments): also set `tls.client_ca_file`. Every client must present a cert chained to that CA, turning llmtap into a hard policy boundary instead of an ambient one.
- **Acknowledge plaintext**: set `allow_insecure: true` (or `LLMTAP_ALLOW_INSECURE=true`). Only do this on a sidecar / single-host bridge / network you fully control. The default config refuses to start in this configuration so you don't end up there by accident.

Generate a quick local cert:

```bash
openssl req -x509 -newkey ec:<(openssl ecparam -name P-256) -nodes \
  -keyout tls.key -out tls.crt -days 365 -subj '/CN=llmtap.local'
LLMTAP_TLS_CERT_FILE=$PWD/tls.crt \
LLMTAP_TLS_KEY_FILE=$PWD/tls.key \
LLMTAP_LISTEN=0.0.0.0:4443 \
  ./llmtap up
```

## Privacy

Defaults are privacy-first. With `content.mode: off` (the default) llmtap only attaches **metadata** to spans: model, parameters, token counts, finish reasons, cost. Prompt and completion text never leave the proxy.

Set `content.mode: events` to attach prompt/completion text as span events; pair it with redaction at your collector. `content.mode: logs` routes the same content through the OTel log signal so retention can be scoped separately.

llmtap never alters bodies. It does not strip auth headers. It only adds a `Via: llmtap` header to the upstream request so audit logs can see it in the chain.

---

## Performance

- Single static binary (~12-15 MB).
- ~1-2 ms overhead added to non-streaming calls; ~0 added latency for streaming (bytes flow through unchanged, parsing is incremental).
- Memory: dominated by in-flight request bodies; capped at 4 MiB per request.
- No goroutine per in-flight request beyond what `net/http` already creates.

---

## Development

```bash
make test     # race detector + verbose
make vet      # go vet
make lint     # golangci-lint if installed, else go vet
make build    # ./llmtap
make release  # cross-compile linux/{amd64,arm64} + darwin/arm64
```

Test coverage includes:
- Config: defaults, YAML overrides, env overrides, validation, longest-prefix routing.
- Pricing: exact match, longest-prefix model match, unknown returns false.
- SSE tee: forwards bytes byte-for-byte while parsing events; trailing partial flush.
- OpenAI parser: request attributes, non-streaming response, streaming with `[DONE]` sentinel and `usage` in final chunk.
- Proxy end-to-end: streaming + non-streaming forwarding through `httptest`, span generation, transparent fallthrough for unknown endpoints.

---

## Roadmap (v0.2+)

- Bedrock, Gemini, Ollama-native parsers.
- `llmtap record` + `llmtap replay` + `llmtap diff` for migration A/B.
- Built-in redaction profiles (PII regex, key-list).
- Sampling strategies tied to GenAI signals (cost-weighted, error-biased).

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
