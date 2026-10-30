# Kubernetes packaging

llmtap ships a Helm chart under [`../helm/llmtap`](../helm/llmtap) plus
stand-alone examples in this directory.

```bash
# Create TLS + auth secrets first (see certificate.yaml), then:
helm upgrade --install llmtap ../helm/llmtap \
  --set auth.enabled=true \
  --set image.tag=v0.1.5
```

## Probes & drain

| Probe | Path | Notes |
|---|---|---|
| liveness | `/healthz` | Process alive; always 200 once listening |
| readiness | `/readyz` | 200 only after first successful OTLP export; **503 on SIGTERM** before stream drain |

The distroless image has no `sleep` binary, so the chart does **not** use a
`preStop` sleep. Instead:

1. SIGTERM → `BeginDrain` → `/readyz` flips 503
2. `terminationGracePeriodSeconds` = preStop budget + `shutdown_timeout`
3. kube removes the Endpoint; in-flight SSE streams drain inside the grace window

Set `GOMEMLIMIT` ~10% under the memory limit (chart default: 1800MiB / 2Gi).

## Per-replica state (important)

These are **process-local**, not cluster-wide:

| Mechanism | Multi-replica behaviour |
|---|---|
| Circuit breaker | Each pod trips independently; one brownout can open N breakers |
| `max_in_flight` | Cap is per pod; cluster fan-out ≈ `replicas × max_in_flight` |
| Auth argon2 / negative cache | Per pod — failed tokens cached 30s before argon2 re-runs |
| Model-label cardinality (`_other` bucket) | Per pod — the same model can be a real series on one replica and `_other` on another if the label set diverges |

Size caps for **per-pod** load, not cluster aggregate. Prefer fewer larger
replicas if you need coherent breaker behaviour; prefer more smaller ones for
blast-radius isolation.

## `auth.tokens_file`

Mount a Secret key `tokens` (one PHC hash per line) and set:

```yaml
auth:
  tokens_file: /etc/llmtap/auth/tokens
```

Or `LLMTAP_AUTH_TOKENS_FILE`. Inline `auth.tokens` in ConfigMaps is discouraged.

## Container health

```
llmtap healthcheck                 # GET $LLMTAP_HEALTHCHECK_URL
llmtap healthcheck --insecure      # HTTPS + self-signed (TLS pods)
```

Dockerfile `HEALTHCHECK` uses the same subcommand (no curl in distroless).
